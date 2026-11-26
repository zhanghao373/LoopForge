package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agentflow/internal/domain"
	"agentflow/internal/runeval"
	"agentflow/internal/runtime"

	"github.com/gin-gonic/gin"
)

const (
	runEvalTimeout     = 10 * time.Minute
	runEvalMaxInflight = 2
	runEvalMaxFeedback = 2000
)

type runEvalExecFunc func(ctx context.Context, rec domain.RunEvalRecord, run domain.Run, user domain.User, emit func(string, any)) (domain.RunEvalResult, error)

type runEvalJobEvent struct {
	Seq     int
	Type    string
	Payload any
}

type runEvalJob struct {
	runID  string
	evalID string
	mu     sync.Mutex
	seq    int
	events []runEvalJobEvent
	subs   map[chan runEvalJobEvent]struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

type runEvalHub struct {
	startMu sync.Mutex
	mu      sync.Mutex
	jobs    map[string]*runEvalJob
	tokens  chan struct{}
}

var (
	runEvalExecMu       sync.Mutex
	runEvalExecOverride runEvalExecFunc
)

func setRunEvalExecForTest(fn runEvalExecFunc) {
	runEvalExecMu.Lock()
	runEvalExecOverride = fn
	runEvalExecMu.Unlock()
}

func currentRunEvalExec() runEvalExecFunc {
	runEvalExecMu.Lock()
	defer runEvalExecMu.Unlock()
	if runEvalExecOverride != nil {
		return runEvalExecOverride
	}
	return nil
}

func newRunEvalHub() *runEvalHub {
	return &runEvalHub{jobs: map[string]*runEvalJob{}, tokens: make(chan struct{}, runEvalMaxInflight)}
}

func (h *runsHandlers) evalHub() *runEvalHub {
	if h.evalJobs == nil {
		h.evalJobs = newRunEvalHub()
	}
	return h.evalJobs
}

func (h *runsHandlers) startRunEval(c *gin.Context) {
	id := c.Param("id")
	run, ok := h.repo.Run(id)
	if !ok {
		c.JSON(http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	if !domain.TerminalRunStatus(run.Status) {
		c.JSON(http.StatusConflict, map[string]string{"error": "请等 SOP 跑完后再评测"})
		return
	}
	current, loggedIn := h.requestUser(c.Request, true)
	if !loggedIn {
		c.JSON(http.StatusUnauthorized, map[string]string{"error": "请先登录后评测"})
		return
	}
	user, ok := h.repo.User(current.ID)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "评测账号不存在"})
		return
	}
	var req struct {
		Model    string `json:"model"`
		Feedback string `json:"feedback"`
	}
	if c.Request.ContentLength != 0 && !decode(c, &req) {
		return
	}
	feedback := strings.TrimSpace(req.Feedback)
	if utf8.RuneCountInString(feedback) > runEvalMaxFeedback {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("评测反馈最多 %d 个字", runEvalMaxFeedback)})
		return
	}
	model := strings.TrimSpace(req.Model)
	runnerID := ""
	if runtime.PersonalExecutorsOnly() {
		model = firstNonEmpty(model, "codex:default")
		engine, modelID := assistantModelEngine(model)
		if engine != assistantEngineCodex || !assistantValidEngineModel(modelID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "本地运行评测需要有效的 Codex 模型"})
			return
		}
		runnerID = h.repo.DefaultRunner(user.ID)
		if _, err := h.runEvalRunnerLogin(runnerID, user); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	} else {
		if strings.TrimSpace(user.CodeBuddyAPIKey) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请先在个人配置中配置自己的 CodeBuddy API Key"})
			return
		}
		if engine, _ := assistantModelEngine(model); engine != assistantEngineCodeBuddy {
			c.JSON(http.StatusBadRequest, gin.H{"error": "运行评测目前只支持 CodeBuddy 模型，请改选 CodeBuddy 模型"})
			return
		}
		model = assistantAllowedModel(model, loadAssistantModels(c.Request.Context(), user.CodeBuddyInternetEnvironment))
	}
	hub := h.evalHub()
	hub.startMu.Lock()
	defer hub.startMu.Unlock()
	if rec, busy := h.repo.ActiveRunEval(id); busy {
		hub.mu.Lock()
		job := hub.jobs[id]
		hub.mu.Unlock()
		if job != nil {
			c.JSON(http.StatusConflict, map[string]string{"error": "这次运行已有评测在进行"})
			return
		}
		// The row was saved as running, then startup failed before a job existed.
		h.failRunEval(rec, "上次评测没有真正启动")
	}
	project, _ := h.repo.Project(run.ExecutionContext.ProjectID)
	rec := domain.RunEvalRecord{
		ID:        "eval-" + randomID(),
		RunID:     id,
		Status:    domain.RunEvalRunning,
		Model:     model,
		RunnerID:  runnerID,
		Feedback:  redactAgentToolResult(feedback, user),
		Policy:    domain.EffectiveRunEvalPolicy(project.RunEval),
		CreatedBy: user.ID,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.repo.PutRunEval(rec); err != nil {
		respond(c, err)
		return
	}
	h.launchRunEval(rec, run, user)
	c.JSON(http.StatusAccepted, rec)
}

func (h *runsHandlers) runEvalRunnerLogin(runnerID string, user domain.User) (bool, error) {
	runner, ok := h.repo.Runner(runnerID)
	if !ok || runner.OwnerUserID != user.ID {
		return false, fmt.Errorf("请先在设置 → 执行机器中接入自己的 Codex 执行机器并设为默认")
	}
	if runner.Status != domain.RunnerStatusOnline {
		return false, fmt.Errorf("默认执行机器「%s」当前离线", runner.Name)
	}
	if h.assistant == nil || h.assistant.runnerControls == nil {
		return false, fmt.Errorf("当前服务无法连接执行机器")
	}
	return verifierRunnerLogin(runner, user, assistantEngineCodex, true)
}

func (h *runsHandlers) getRunEval(c *gin.Context) {
	id := c.Param("id")
	if !h.repo.RunExists(id) {
		c.JSON(http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	if rec, ok := h.repo.LatestRunEval(id); ok {
		c.JSON(http.StatusOK, rec)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"runId": id, "status": ""})
}

func (h *runsHandlers) listRunEvals(c *gin.Context) {
	id := c.Param("id")
	if !h.repo.RunExists(id) {
		c.JSON(http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	c.JSON(http.StatusOK, h.repo.ListRunEvals(id))
}

func (h *runsHandlers) streamRunEval(c *gin.Context) {
	id := c.Param("id")
	if !h.repo.RunExists(id) {
		c.JSON(http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	hub := h.evalHub()
	hub.mu.Lock()
	job := hub.jobs[id]
	hub.mu.Unlock()
	if job == nil {
		if rec, ok := h.repo.LatestRunEval(id); ok {
			c.Header("Content-Type", "text/event-stream; charset=utf-8")
			c.Status(http.StatusOK)
			writeSSE(c, "done", rec)
			return
		}
		c.JSON(http.StatusNotFound, map[string]string{"error": "eval not found"})
		return
	}
	ch, replay := job.subscribe()
	defer job.unsubscribe(ch)
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	for _, ev := range replay {
		writeSSE(c, ev.Type, ev.Payload)
		if ev.Type == "done" || ev.Type == "error" {
			return
		}
	}
	notify := c.Request.Context().Done()
	for {
		select {
		case <-notify:
			return
		case <-job.done:
			if rec, ok := h.repo.RunEval(job.evalID); ok {
				writeSSE(c, "done", rec)
			}
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(c, ev.Type, ev.Payload)
			if ev.Type == "done" || ev.Type == "error" {
				return
			}
		}
	}
}

func writeSSE(c *gin.Context, event string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, raw)
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *runsHandlers) applyRunEval(c *gin.Context) {
	id := c.Param("id")
	run, ok := h.repo.Run(id)
	if !ok {
		c.JSON(http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	if _, loggedIn := h.requestUser(c.Request, true); !loggedIn {
		c.JSON(http.StatusUnauthorized, map[string]string{"error": "请先登录后再应用评测建议"})
		return
	}
	var req struct {
		EvalID            string   `json:"evalId"`
		RecommendationIDs []string `json:"recommendationIds"`
	}
	if !decode(c, &req) {
		return
	}
	rec, ok := h.repo.LatestRunEval(id)
	if req.EvalID != "" {
		rec, ok = h.repo.RunEval(req.EvalID)
	}
	if !ok || rec.RunID != id {
		c.JSON(http.StatusNotFound, map[string]string{"error": "eval not found"})
		return
	}
	if rec.Status != domain.RunEvalSucceeded {
		c.JSON(http.StatusConflict, map[string]string{"error": "评测尚未完成"})
		return
	}
	wanted := map[string]bool{}
	for _, item := range req.RecommendationIDs {
		wanted[strings.TrimSpace(item)] = true
	}
	if len(wanted) == 0 {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "请选择要应用的建议"})
		return
	}
	projectID := run.ExecutionContext.ProjectID
	changed := false
	for i, item := range rec.Result.Recommendations {
		if !wanted[item.ID] {
			continue
		}
		if item.Status == domain.RunEvalRecApplied {
			continue
		}
		if !item.Applyable() {
			rec.Result.Recommendations[i].ApplyError = "这条建议只能查看，不能一键应用"
			changed = true
			continue
		}
		applied, err := h.applyOneRecommendation(run, projectID, item)
		if err != nil {
			rec.Result.Recommendations[i].ApplyError = err.Error()
			changed = true
			continue
		}
		now := time.Now().UTC()
		applied.Status = domain.RunEvalRecApplied
		applied.AppliedAt = &now
		applied.ApplyError = ""
		rec.Result.Recommendations[i] = applied
		changed = true
	}
	if changed {
		if err := h.repo.PutRunEval(rec); err != nil {
			respond(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, rec)
}

func (h *runsHandlers) applyOneRecommendation(run domain.Run, projectID string, rec domain.RunEvalRecommendation) (domain.RunEvalRecommendation, error) {
	action := domain.NormalizeRunEvalAction(rec.Target, rec.Action)
	switch {
	case strings.HasPrefix(action, "workflow."):
		workflowID := firstNonEmpty(rec.TargetID, run.WorkflowID, run.WorkflowSnapshot.ID)
		if workflowID != run.WorkflowID && workflowID != run.WorkflowSnapshot.ID {
			return rec, fmt.Errorf("评测建议的目标必须是本次运行的 SOP")
		}
		wf, err := h.ensureProjectWorkflow(run, projectID, workflowID)
		if err != nil {
			return rec, err
		}
		if err := runeval.ApplyRecommendation(rec, &wf, nil); err != nil {
			return rec, err
		}
		if err := h.repo.PutWorkflow(wf); err != nil {
			return rec, err
		}
		rec.AppliedTargetID = wf.ID
		return rec, nil
	case strings.HasPrefix(action, "agent."):
		agentID := firstNonEmpty(rec.TargetID, agentIDFromRun(run, rec.NodeID))
		usedByRun := false
		for _, node := range run.WorkflowSnapshot.Nodes {
			if agentID != "" && node.AgentID == agentID {
				usedByRun = true
				break
			}
		}
		if !usedByRun {
			return rec, fmt.Errorf("评测建议的目标必须是本次运行使用的角色")
		}
		agent, err := h.ensureProjectAgent(run, projectID, agentID)
		if err != nil {
			return rec, err
		}
		if err := runeval.ApplyRecommendation(rec, nil, &agent); err != nil {
			return rec, err
		}
		if err := h.repo.PutAgent(agent); err != nil {
			return rec, err
		}
		rec.AppliedTargetID = agent.ID
		return rec, nil
	default:
		return rec, fmt.Errorf("unsupported action")
	}
}

func (h *runsHandlers) ensureProjectWorkflow(run domain.Run, projectID, workflowID string) (domain.WorkflowDefinition, error) {
	wf, ok := h.repo.Workflow(workflowID)
	if !ok {
		if run.WorkflowSnapshot.ID != "" {
			wf, ok = run.WorkflowSnapshot, true
		}
	}
	if !ok {
		return domain.WorkflowDefinition{}, fmt.Errorf("SOP %q 不存在", workflowID)
	}
	if strings.TrimSpace(wf.Name) == "" {
		wf.Name = firstNonEmpty(wf.ID, "workflow")
	}
	if !wf.Generic && (wf.ProjectID == "" || wf.ProjectID == projectID) {
		return wf, nil
	}
	cloneID := runeval.CloneID(wf.ID, firstNonEmpty(projectID, "copy"))
	if existing, exists := h.repo.Workflow(cloneID); exists {
		return existing, nil
	}
	wf.ID = cloneID
	wf.Name = strings.TrimSpace(wf.Name) + "（评测副本）"
	wf.ProjectID = projectID
	wf.Generic = false
	wf.Shared = false
	wf.Status = "draft"
	wf.Revision = 0
	wf.CreatedAt, wf.UpdatedAt = time.Time{}, time.Time{}
	if err := h.repo.PutWorkflow(wf); err != nil {
		return domain.WorkflowDefinition{}, err
	}
	stored, _ := h.repo.Workflow(cloneID)
	return stored, nil
}

func (h *runsHandlers) ensureProjectAgent(run domain.Run, projectID, agentID string) (domain.AgentDefinition, error) {
	agent, ok := h.repo.Agent(agentID)
	if !ok {
		return domain.AgentDefinition{}, fmt.Errorf("角色 %q 不存在", agentID)
	}
	if !agent.Shared && (agent.ProjectID == "" || agent.ProjectID == projectID) {
		return agent, nil
	}
	cloneID := runeval.CloneID(agent.ID, firstNonEmpty(projectID, "copy"))
	if existing, exists := h.repo.Agent(cloneID); exists {
		return existing, nil
	}
	agent.ID = cloneID
	agent.Name = strings.TrimSpace(agent.Name) + "（评测副本）"
	agent.ProjectID = projectID
	agent.Shared = false
	agent.Version = 1
	agent.UpdatedAt = time.Time{}
	if err := h.repo.PutAgent(agent); err != nil {
		return domain.AgentDefinition{}, err
	}
	if wf, err := h.ensureProjectWorkflow(run, projectID, run.WorkflowID); err == nil {
		changed := false
		for i, node := range wf.Nodes {
			if node.AgentID == agentID {
				wf.Nodes[i].AgentID = cloneID
				changed = true
			}
		}
		if changed {
			_ = h.repo.PutWorkflow(wf)
		}
	}
	stored, _ := h.repo.Agent(cloneID)
	return stored, nil
}

func agentIDFromRun(run domain.Run, nodeID string) string {
	for _, node := range run.WorkflowSnapshot.Nodes {
		if node.ID == nodeID {
			return node.AgentID
		}
	}
	return ""
}

func (h *runsHandlers) launchRunEval(rec domain.RunEvalRecord, run domain.Run, user domain.User) {
	hub := h.evalHub()
	ctx, cancel := context.WithTimeout(context.Background(), runEvalTimeout)
	job := &runEvalJob{
		runID:  rec.RunID,
		evalID: rec.ID,
		subs:   map[chan runEvalJobEvent]struct{}{},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	hub.mu.Lock()
	if old := hub.jobs[rec.RunID]; old != nil && old.cancel != nil {
		old.cancel()
	}
	hub.jobs[rec.RunID] = job
	hub.mu.Unlock()
	go func() {
		defer cancel()
		h.runEvalJob(ctx, job, rec, run, user)
	}()
}

func (h *runsHandlers) runEvalJob(ctx context.Context, job *runEvalJob, rec domain.RunEvalRecord, run domain.Run, user domain.User) {
	defer close(job.done)
	defer func() {
		hub := h.evalHub()
		hub.mu.Lock()
		if hub.jobs[job.runID] == job {
			delete(hub.jobs, job.runID)
		}
		hub.mu.Unlock()
	}()
	hub := h.evalHub()
	select {
	case hub.tokens <- struct{}{}:
		defer func() { <-hub.tokens }()
	case <-ctx.Done():
		h.failRunEval(rec, "评测排队超时")
		job.publish("error", map[string]string{"error": "评测排队超时"})
		return
	}
	emit := func(event string, payload any) { job.publish(event, payload) }
	emit("thought", map[string]string{"kind": "thinking", "text": "正在整理本次 SOP 轨迹…"})
	exec := currentRunEvalExec()
	if exec == nil {
		exec = h.executeRunEval
	}
	result, err := exec(ctx, rec, run, user, emit)
	if err != nil {
		h.failRunEval(rec, err.Error())
		job.publish("error", map[string]string{"error": err.Error()})
		return
	}
	if saved, ok := h.repo.RunEval(rec.ID); ok {
		rec = saved
	}
	rec.Status = domain.RunEvalSucceeded
	rec.Score = result.Score
	rec.Passed = result.Passed
	rec.Result = result
	rec.FinishedAt = time.Now().UTC()
	if err := h.repo.PutRunEval(rec); err != nil {
		h.failRunEval(rec, err.Error())
		job.publish("error", map[string]string{"error": err.Error()})
		return
	}
	job.publish("done", rec)
}

func (h *runsHandlers) executeRunEval(ctx context.Context, rec domain.RunEvalRecord, run domain.Run, user domain.User, emit func(string, any)) (domain.RunEvalResult, error) {
	catalog := runeval.BuildCatalog(run, func(id string) (domain.AgentDefinition, bool) {
		return h.repo.Agent(id)
	})
	facts := runeval.BuildFacts(run, catalog)
	rec.Facts = facts
	_ = h.repo.PutRunEval(rec)
	if emit != nil {
		emit("thought", map[string]string{"kind": "thinking", "text": "正在用 " + rec.Model + " 评测…"})
	}
	if rec.RunnerID != "" {
		localLogin, err := h.runEvalRunnerLogin(rec.RunnerID, user)
		if err != nil {
			return domain.RunEvalResult{}, err
		}
		ctx = h.assistant.helperExecutionContext(ctx, user.ID, &domain.AssistantExecutionTarget{RunnerID: rec.RunnerID}, localLogin)
	}
	root, err := os.MkdirTemp("", "loopforge-run-eval-")
	if err != nil {
		return domain.RunEvalResult{}, err
	}
	defer os.RemoveAll(root)

	system, prompt := runeval.BuildPrompt(rec.Policy, facts, rec.Feedback)
	raw, _, err := runAssistantCLI(ctx, assistantCLIRequest{
		Prompt: redactAgentToolResult(prompt, user), Model: rec.Model, SystemPrompt: system,
		UserID: user.ID, SessionRoot: root, ReviewOnly: true,
	}, user, func(ev assistantStreamEvent) {
		if emit == nil {
			return
		}
		if text := redactAgentToolResult(ev.Text, user); text != "" {
			emit("thought", map[string]string{"kind": ev.Kind, "text": text})
		}
	})
	if err != nil {
		return domain.RunEvalResult{}, fmt.Errorf("评测调用失败：%s", redactAgentToolResult(err.Error(), user))
	}
	result, err := runeval.ParseModelResult(redactAssistantSecrets(raw, user), rec.Policy)
	if err != nil {
		return domain.RunEvalResult{}, err
	}
	return result, nil
}

func (h *runsHandlers) failRunEval(rec domain.RunEvalRecord, message string) {
	rec.Status = domain.RunEvalFailed
	rec.Error = message
	rec.FinishedAt = time.Now().UTC()
	if err := h.repo.PutRunEval(rec); err != nil {
		log.Printf("run eval persist failure %s: %v", rec.ID, err)
	}
}

func (j *runEvalJob) publish(event string, payload any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	item := runEvalJobEvent{Seq: j.seq, Type: event, Payload: payload}
	j.events = append(j.events, item)
	for ch := range j.subs {
		select {
		case ch <- item:
		default:
		}
	}
}

func (j *runEvalJob) subscribe() (chan runEvalJobEvent, []runEvalJobEvent) {
	ch := make(chan runEvalJobEvent, 16)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.subs[ch] = struct{}{}
	return ch, append([]runEvalJobEvent(nil), j.events...)
}

func (j *runEvalJob) unsubscribe(ch chan runEvalJobEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.subs, ch)
	close(ch)
}
