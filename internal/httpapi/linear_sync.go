package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/tracker"

	"github.com/gin-gonic/gin"
)

type linearImportRequest struct {
	ProjectID       string                    `json:"projectId"`
	LinearProjectID string                    `json:"linearProjectId"`
	APIKey          string                    `json:"apiKey"`
	Connection      string                    `json:"connection"`
	Filter          tracker.LinearIssueFilter `json:"filter"`
}

type linearSource interface {
	Projects(context.Context, string) ([]tracker.LinearProject, error)
	Issues(context.Context, string, string, tracker.LinearIssueFilter, bool) ([]tracker.LinearIssue, bool, error)
}

func (h *syncHandlers) linearConnection(ctx context.Context, c *gin.Context, req linearImportRequest) (linearSource, string, error) {
	if req.Connection != "mcp" {
		return h.linear, req.APIKey, nil
	}
	user, ok := h.requestUser(c.Request, false)
	if !ok || user == nil || !h.linearMCP.enabled() {
		return nil, "", tracker.ErrLinearMCPAuthorization
	}
	token, err := h.linearMCP.access(ctx, user.ID, h.linearMCPSecret())
	return h.linearMCP.client, token, err
}

func (h *syncHandlers) allowLinearImport(c *gin.Context, projectID string) bool {
	if projectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先选择导入到哪个 LoopForge 项目"})
		return false
	}
	if _, found := h.repo.Project(projectID); !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "找不到要导入的 LoopForge 项目"})
		return false
	}
	return h.requireProjectWriter(c, projectID)
}

func (h *syncHandlers) linearRequest(c *gin.Context, needsSelection bool) (linearImportRequest, bool) {
	var req linearImportRequest
	user, ok := h.requestUser(c.Request, true)
	if !ok || user == nil || user.ID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先登录 LoopForge"})
		return req, false
	}
	c.Header("Cache-Control", "no-store")
	if !decodeLimit(c, &req, 8<<10) {
		return req, false
	}
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	if !h.allowLinearImport(c, req.ProjectID) {
		return req, false
	}
	switch req.Connection {
	case "", "key":
		req.APIKey = strings.TrimSpace(req.APIKey)
		if req.APIKey == "" || len(req.APIKey) > 512 || strings.ContainsAny(req.APIKey, "\r\n") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请输入有效的 Linear 个人 API Key"})
			return req, false
		}
	case "mcp":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择 Linear MCP 或 API Key"})
		return req, false
	}
	if !needsSelection {
		return req, true
	}
	req.LinearProjectID = strings.TrimSpace(req.LinearProjectID)
	req.Filter.Query = strings.TrimSpace(req.Filter.Query)
	if req.LinearProjectID == "" || len(req.LinearProjectID) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择 Linear 项目"})
		return req, false
	}
	if _, known := linearInitialStatus[req.Filter.State]; req.Filter.State != "" && !known {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 Linear 状态筛选"})
		return req, false
	}
	if priority := req.Filter.Priority; priority != nil && (*priority < 0 || *priority > 4) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 Linear 优先级筛选"})
		return req, false
	}
	if len([]rune(req.Filter.Query)) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "搜索关键词最多 200 个字符"})
		return req, false
	}
	return req, true
}

func linearReadError(c *gin.Context, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, tracker.ErrLinearMCPAuthorization):
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error(), "code": "linear_mcp_reconnect"})
		return
	case errors.Is(err, tracker.ErrLinearKey):
		status = http.StatusForbidden
	case errors.Is(err, tracker.ErrLinearRate):
		status = http.StatusTooManyRequests
	case errors.Is(err, tracker.ErrLinearProject):
		status = http.StatusNotFound
	}
	c.JSON(status, gin.H{"error": err.Error()})
}

func (h *syncHandlers) linearProjects(c *gin.Context) {
	req, ok := h.linearRequest(c, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	source, token, err := h.linearConnection(ctx, c, req)
	if err != nil {
		linearReadError(c, err)
		return
	}
	projects, err := source.Projects(ctx, token)
	if err != nil {
		linearReadError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

func (h *syncHandlers) linearPreview(c *gin.Context) {
	h.linearIssues(c, true)
}

func (h *syncHandlers) linearImport(c *gin.Context) {
	h.linearIssues(c, false)
}

func (h *syncHandlers) linearIssues(c *gin.Context, preview bool) {
	req, ok := h.linearRequest(c, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	source, token, err := h.linearConnection(ctx, c, req)
	if err != nil {
		linearReadError(c, err)
		return
	}
	issues, hasMore, err := source.Issues(ctx, token, req.LinearProjectID, req.Filter, preview)
	if err != nil {
		linearReadError(c, err)
		return
	}
	if preview {
		c.JSON(http.StatusOK, gin.H{"issues": issues, "hasMore": hasMore})
		return
	}
	incoming := make([]domain.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.ID == "" || issue.UpdatedAt.IsZero() || strings.TrimSpace(issue.Title) == "" || len([]rune(issue.Title)) > 512 {
			c.JSON(http.StatusBadGateway, gin.H{"error": "Linear 需求须包含原始 ID、标题（最多 512 字）和更新时间"})
			return
		}
		incoming = append(incoming, importedLinearIssue(issue))
	}
	result, err := h.repo.ImportLinearIssues(req.ProjectID, incoming)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导入未保存，请稍后重试"})
		return
	}
	c.JSON(http.StatusOK, result)
}

var linearInitialStatus = map[string]string{
	"triage": "backlog", "backlog": "backlog", "unstarted": "todo",
	"started": "in_progress", "completed": "done", "canceled": "cancelled",
}

func importedLinearIssue(issue tracker.LinearIssue) domain.Issue {
	status := linearInitialStatus[issue.State.Type]
	if status == "" {
		status = "todo"
	}
	issueURL := ""
	if parsed, err := url.Parse(issue.URL); err == nil && parsed.Scheme == "https" && parsed.Host == "linear.app" && parsed.User == nil {
		issueURL = parsed.String()
	}
	priority := map[int]string{1: "urgent", 2: "high", 3: "medium", 4: "low"}[issue.Priority]
	return domain.Issue{
		Title: issue.Title, Description: issue.Description, Priority: priority,
		Status: status, Kind: "requirement", AutomationState: "ignored",
		ExternalRef: &domain.ExternalRef{
			Source: "linear", ExternalID: issue.ID, ExternalIdentifier: issue.Identifier,
			ExternalURL: issueURL, ExternalState: issue.State.Name, ExternalType: "issue",
			SyncedAt: time.Now().UTC(), ExternalUpdatedAt: &issue.UpdatedAt, Imported: true,
		},
	}
}
