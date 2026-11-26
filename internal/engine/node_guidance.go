package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

const guidanceRestartActivity = "已收到定向指导，等待恢复"

var ErrNodeGuidanceUnavailable = errors.New("节点执行状态已变化，请查看最新运行详情")

type NodeGuidanceRequest struct {
	NodeID    string    `json:"nodeId"`
	Attempt   int       `json:"attempt"`
	StartedAt time.Time `json:"startedAt"`
	Guidance  string    `json:"guidance"`
}

func (req *NodeGuidanceRequest) Validate() error {
	req.NodeID = strings.TrimSpace(req.NodeID)
	req.Guidance = strings.TrimSpace(req.Guidance)
	if req.NodeID == "" || req.Attempt < 1 || req.StartedAt.IsZero() || req.Guidance == "" {
		return errors.New("请选择正在执行的 Agent 节点并填写指导")
	}
	if len(req.Guidance) > maxHumanContextBytes {
		return fmt.Errorf("指导内容最多 %d 字节", maxHumanContextBytes)
	}
	return nil
}

func (e *Engine) GuideNode(ctx context.Context, runID string, req NodeGuidanceRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	r, ok := e.repo.RunState(runID)
	if !ok || r.OwnerInstanceID == "" || !r.LeaseUntil.After(time.Now().UTC()) {
		return ErrNodeGuidanceUnavailable
	}
	if e.ownsRun(r) {
		return e.guideOwnedNode(runID, req)
	}
	return e.forwardNodeGuidance(ctx, r, req)
}

func (e *Engine) guideOwnedNode(runID string, req NodeGuidanceRequest) error {
	if err := e.recordNodeGuidance(runID, req); err != nil {
		return err
	}
	e.reconcileNodeExecutions(runID)
	e.publish(runID)
	return nil
}

func (e *Engine) recordNodeGuidance(runID string, req NodeGuidanceRequest) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	r, ok := e.repo.RunState(runID)
	if !ok || !e.ownsRun(r) || r.Status != "running" || r.PauseRequested || r.CancelRequested || r.IsWorkspace() || r.HistoricalGroup() {
		return ErrNodeGuidanceUnavailable
	}
	nr := r.Nodes[req.NodeID]
	if nr == nil || nr.Status != "running" || nr.Attempt != req.Attempt || !nr.StartedAt.Equal(req.StartedAt) {
		return ErrNodeGuidanceUnavailable
	}
	if err := forbidClosedGongfengIssue(e.repo, r); err != nil {
		return err
	}
	wf := workflowForRun(e.repo, r)
	var node domain.WorkflowNode
	for _, candidate := range wf.Nodes {
		if candidate.ID == req.NodeID {
			node = candidate
			break
		}
	}
	agent, found := workflowAgent(e.repo, wf, node.AgentID)
	if node.Type != "agent" || !found {
		return errors.New("此节点不支持定向指导")
	}
	executor, err := resolveNodeExecutor(e.repo, node, agent)
	if err != nil {
		return err
	}
	if executor.Kind == domain.A2AExecutorID {
		return errors.New("远程 A2A 节点不支持定向指导")
	}
	resetNodeForResume(&r, nr, guidanceRestartActivity)
	nr.Guidance = req.Guidance
	nr.GateResult = nil
	e.event(&r, node.ID, "node.guidance", "已收到定向指导，准备继续执行")
	appendControlTelemetry(&r, node.ID, "node.guidance", "定向指导：\n"+gitwork.Redact(req.Guidance))
	// Fence the control decision against a concurrent run-wide pause or cancel.
	return e.repo.PutRunAtRevision(r, r.Revision)
}

func (e *Engine) forwardNodeGuidance(ctx context.Context, run domain.Run, guidance NodeGuidanceRequest) error {
	e.mu.Lock()
	token := e.controlToken
	e.mu.Unlock()
	base := e.repo.EngineInstanceControlURL(run.OwnerInstanceID)
	if base == "" || token == "" {
		return errors.New("无法连接运行所属的 Worker，请检查内部控制连接配置")
	}
	body, err := json.Marshal(guidance)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/internal/runs/"+url.PathEscape(run.ID)+"/guide-node", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set(controlTokenHeader, token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("提交定向指导：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		message, err := io.ReadAll(io.LimitReader(response.Body, 2048))
		if err == nil && strings.TrimSpace(string(message)) != "" {
			return errors.New(strings.TrimSpace(string(message)))
		}
	}
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("Worker 无法接收定向指导（HTTP %d）", response.StatusCode)
	}
	return nil
}

// HandleNodeGuidance applies the request on the lease owner, serialized with
// that worker's own finish and retry writes.
func (e *Engine) HandleNodeGuidance(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	configured := e.controlToken != ""
	e.mu.Unlock()
	if !configured || !e.controlTokenOK(r.Header.Get(controlTokenHeader)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req NodeGuidanceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid node guidance", http.StatusBadRequest)
		return
	}
	if err := req.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := e.guideOwnedNode(r.PathValue("id"), req); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
