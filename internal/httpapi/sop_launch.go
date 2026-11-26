package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/runtime"

	"github.com/gin-gonic/gin"
)

type sopLaunchOptions struct {
	RequiresSource bool                  `json:"requiresSource"`
	DefaultRunner  string                `json:"defaultRunner"`
	Runners        []sopLaunchRunner     `json:"runners"`
	Cloud          sopLaunchRunner       `json:"cloud"`
	CLIs           []engine.SOPLaunchCLI `json:"clis"`
}

type sopLaunchRunner struct {
	Auth  map[string]string `json:"auth,omitempty"`
	ID    string            `json:"id,omitempty"`
	Name  string            `json:"name"`
	Error string            `json:"error,omitempty"`
}

type sopLaunchRequest struct {
	Mode        string   `json:"mode"`
	RunnerID    string   `json:"runnerId,omitempty"`
	SourcePath  string   `json:"sourcePath,omitempty"`
	SourcePaths []string `json:"sourcePaths,omitempty"`
}

func (r sopLaunchRequest) localSources() []string {
	var paths []string
	for _, path := range domain.LocalSourceSelection(strings.TrimSpace(r.SourcePath), r.SourcePaths) {
		if path = strings.TrimSpace(path); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func (h *runsHandlers) sopLaunchOptions(c *gin.Context) {
	current, ok := h.requestUser(c.Request, true)
	if !ok || current.ID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先登录"})
		return
	}
	projectID, workflowID := c.Query("projectId"), c.Query("workflowId")
	project, projectOK := h.repo.Project(projectID)
	wf, workflowOK := h.repo.Workflow(workflowID)
	if !projectOK || !workflowOK {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目或 SOP 不存在"})
		return
	}
	if !h.requireProjectWriter(c, project.ID) || !catalogCanUseWorkflowInProject(wf, project.ID) {
		if !c.Writer.Written() {
			c.JSON(http.StatusForbidden, gin.H{"error": "此项目无法使用该 SOP"})
		}
		return
	}
	wf, err := wf.SelectNodes(c.QueryArray("disabledNodeId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	items, err := h.engine.SOPLaunchCLIs(wf)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	user, _ := h.repo.User(current.ID)
	out := sopLaunchOptions{DefaultRunner: h.repo.DefaultRunner(user.ID), Runners: []sopLaunchRunner{}, CLIs: items, RequiresSource: sopLaunchNeedsSource(wf)}
	for _, runner := range h.repo.Runners() {
		if runner.OwnerUserID == current.ID {
			out.Runners = append(out.Runners, launchRunnerStatus(runner, user, items, true))
		}
	}
	out.Cloud = h.cloudLaunchStatus(project.ID, user, items)
	c.JSON(http.StatusOK, out)
}

func sopLaunchNeedsSource(wf domain.WorkflowDefinition) bool {
	if wf.RequiresSourceCode() {
		return true
	}
	// Workflows published by the first personal SOP installer predate this
	// requirement. Preserve their published revisions.
	if wf.ProjectID == "local-sop" {
		switch wf.ID {
		case "local-sop-dev", "local-sop-qa", "local-sop-rsi":
			return true
		}
	}
	return false
}

func catalogCanUseWorkflowInProject(wf domain.WorkflowDefinition, projectID string) bool {
	return wf.Generic || wf.Shared || wf.ProjectID == projectID
}

func launchRunnerStatus(runner domain.Runner, user domain.User, items []engine.SOPLaunchCLI, private bool) sopLaunchRunner {
	out := sopLaunchRunner{ID: runner.ID, Name: runner.Name, Auth: map[string]string{}}
	if runner.Status != domain.RunnerStatusOnline {
		out.Error = runner.Name + " 当前离线"
		return out
	}
	const terminal = "terminal"
	var problems []string
	seen := map[string]bool{}
	reviewed := map[string]bool{}
	for _, cli := range items {
		if cli.Review && !reviewed[cli.Provider] {
			if _, err := verifierRunnerLogin(runner, user, cli.Provider, private); err != nil {
				problems = append(problems, err.Error())
			}
			reviewed[cli.Provider] = true
		}
		if seen[cli.Provider] {
			continue
		}
		seen[cli.Provider] = true
		label := cli.Name
		if label == "" {
			label = "脚本执行"
		}
		if cli.Provider != terminal {
			out.Auth[cli.Provider] = runner.RuntimeAuthMode(cli.Provider)
		}
		if !runner.SupportsProvider(cli.Provider) {
			problems = append(problems, "缺少 "+label)
		} else if cli.Provider != terminal && runner.RuntimeAuthMode(cli.Provider) == domain.RuntimeAuthPersonal {
			key, _ := assistantEngineCredential(user, cli.Provider)
			if key == "" {
				problems = append(problems, "未配置 "+label+" API Key")
			}
		}
	}
	if len(problems) > 0 {
		out.Error = strings.Join(problems, "，")
	}
	return out
}

func (h *runsHandlers) cloudLaunchStatus(projectID string, user domain.User, items []engine.SOPLaunchCLI) sopLaunchRunner {
	out := sopLaunchRunner{Name: "云端执行池", Error: "项目暂无在线的云端执行机器"}
	for _, runner := range h.engine.SOPLaunchCloudRunners(projectID) {
		out = launchRunnerStatus(runner, user, items, false)
		if out.Error == "" {
			return out
		}
	}
	return out
}

func (h *runsHandlers) launchLocalSources(c *gin.Context) {
	if !runtime.PersonalExecutorsOnly() {
		c.Status(http.StatusNotFound)
		return
	}
	current, ok := h.requestUser(c.Request, true)
	runner, found := h.repo.Runner(c.Param("id"))
	if !ok || !found || runner.OwnerUserID != current.ID || current.ID <= 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "执行机器不可用"})
		return
	}
	sources, err := h.runnerLocalSources(c.Request.Context(), runner, c.Query("path"))
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, sources)
}

func (h *runsHandlers) runnerLocalSources(ctx context.Context, runner domain.Runner, path string) (domain.RunnerLocalSources, error) {
	var sources domain.RunnerLocalSources
	err := h.requestLocalSources(ctx, runner, domain.RunnerWorkspaceRequest{
		Operation: domain.RunnerWorkspaceOperationLocalSources, Path: path,
	}, &sources)
	return sources, err
}

func (h *runsHandlers) validatedLocalSources(ctx context.Context, runner domain.Runner, paths []string) ([]string, error) {
	if len(paths) == 1 {
		sources, err := h.runnerLocalSources(ctx, runner, paths[0])
		if err != nil {
			return nil, err
		}
		if !sources.Selectable || sources.Path == "" {
			return nil, fmt.Errorf("请选择一个可用的项目目录")
		}
		return []string{sources.Path}, nil
	}
	var resolved []string
	err := h.requestLocalSources(ctx, runner, domain.RunnerWorkspaceRequest{
		Operation: domain.RunnerWorkspaceOperationValidateLocalSources, LocalSourcePaths: paths,
	}, &resolved)
	if err == nil && len(resolved) == 0 {
		err = fmt.Errorf("请选择可用的项目目录")
	}
	return resolved, err
}

func (h *runsHandlers) requestLocalSources(ctx context.Context, runner domain.Runner, request domain.RunnerWorkspaceRequest, output any) error {
	if runner.Status != domain.RunnerStatusOnline {
		return fmt.Errorf("%s 当前离线", runner.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	response, err := h.runnerControls.requestWorkspace(ctx, runner.ID, request)
	if err != nil {
		return fmt.Errorf("无法连接 %s 读取目录，请确认执行机器已升级并在线", runner.Name)
	}
	if response.Error != "" {
		return fmt.Errorf("%s", response.Error)
	}
	if err := json.Unmarshal(response.Payload, output); err != nil {
		return fmt.Errorf("执行机器无法读取本机目录")
	}
	return nil
}

func (h *runsHandlers) resolveSOPLaunch(ctx context.Context, requested sopLaunchRequest, projectID string, workflow domain.WorkflowDefinition, userID int64) (domain.TelemetryContext, error) {
	result := domain.TelemetryContext{ProjectID: projectID, ResponsibleUserID: userID, ExecutionPinned: true}
	if userID <= 0 {
		return result, fmt.Errorf("请先登录")
	}
	user, _ := h.repo.User(userID)
	items, err := h.engine.SOPLaunchCLIs(workflow)
	if err != nil {
		return result, err
	}
	paths := requested.localSources()
	switch requested.Mode {
	case "runner":
		runner, ok := h.repo.Runner(strings.TrimSpace(requested.RunnerID))
		if !ok || runner.OwnerUserID != userID {
			return result, fmt.Errorf("请选择自己的执行机器")
		}
		status := launchRunnerStatus(runner, user, items, true)
		if status.Error != "" {
			return result, fmt.Errorf("%s", status.Error)
		}
		if len(paths) > 0 {
			if !runtime.PersonalExecutorsOnly() {
				return result, fmt.Errorf("当前部署未启用本机目录运行")
			}
			sources, err := h.validatedLocalSources(ctx, runner, paths)
			if err != nil {
				return result, err
			}
			if len(sources) == 1 {
				result.LocalSourcePath = sources[0]
			} else {
				result.LocalSourcePaths = sources
			}
		}
		result.ExecutionRunnerID = runner.ID
	case "cloud":
		if len(paths) != 0 || requested.RunnerID != "" {
			return result, fmt.Errorf("云端运行通过执行池分配，只接受可访问的 Git 仓库")
		}
		status := h.cloudLaunchStatus(projectID, user, items)
		if status.Error != "" {
			return result, fmt.Errorf("%s", status.Error)
		}
		result.ExecutionRunnerID, result.ExecutionPool = status.ID, domain.ExecutionModeCloud
	default:
		return result, fmt.Errorf("请选择执行环境")
	}
	result.ExecutionMode = domain.ExecutionModeRunner
	return result, nil
}
