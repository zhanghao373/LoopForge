package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

func TestNodeGuidanceResumesOnlySelectedRunnerTask(t *testing.T) {
	wf := domain.WorkflowDefinition{ID: "guided-parallel", Name: "Parallel guidance", SandboxProfileID: "local",
		Nodes: []domain.WorkflowNode{
			{ID: "frontend", Name: "Frontend", Type: "agent", AgentID: "frontend-role", MaxAttempts: 1},
			{ID: "backend", Name: "Backend", Type: "agent", AgentID: "backend-role", MaxAttempts: 1},
		},
	}
	worker, store := parallelTestEngine(t, wf)
	worker.ConfigureExecution("guidance-worker", "worker-host", time.Minute, true)
	worker.ConfigureControl(ControlConfig{Token: "test-control-token"})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/runs/{id}/guide-node", worker.HandleNodeGuidance)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	if err := store.HeartbeatEngineInstance(repo.EngineInstance{
		InstanceID: "guidance-worker", Role: "worker", ControlURL: server.URL,
	}, time.Minute); err != nil {
		t.Fatal(err)
	}
	api := New(store, worker.sandboxes)
	api.ConfigureExecution("guidance-api", "api-host", time.Minute, false)
	api.ConfigureControl(ControlConfig{Token: "test-control-token"})
	for _, node := range wf.Nodes {
		if err := store.PutAgent(domain.AgentDefinition{ID: node.AgentID, Name: node.Name, ExecutorID: "codex", Prompt: node.Name}); err != nil {
			t.Fatal(err)
		}
	}
	runner := cvERunner(t, worker, "guidance-runner", domain.RunnerStatusOnline, "codex")
	run, err := worker.StartWithExecutionContext(wf.ID, "Build both features", domain.RepositoryContext{}, domain.TelemetryContext{
		ExecutionPinned: true, ExecutionRunnerID: runner.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if worker.isRunSchedulerActive(run.ID) {
			_ = worker.Cancel(run.ID)
		}
	})
	claim := func() domain.RunnerTask {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			task, ok, err := store.ClaimRunnerTask(runner.ID, []string{"codex"}, time.Now().UTC(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				if err := store.StartRunnerTask(task.ID, runner.ID, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				return task
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("Runner did not receive task")
		return domain.RunnerTask{}
	}
	complete := func(task domain.RunnerTask, status, output, message string) {
		t.Helper()
		outputs := &domain.RunnerTaskOutputs{Files: map[string]string{
			task.Invocation.GateResultPath: `{"decision":"pass","reason":"completed"}`,
		}}
		if err := store.CompleteRunnerTask(task.ID, runner.ID, status, nil, output, message, nil, outputs, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		worker.NotifyRunnerTaskChanged(task.ID)
	}
	frontend, backend := claim(), claim()
	if frontend.NodeID != "frontend" {
		frontend, backend = backend, frontend
	}
	if frontend.NodeID != "frontend" || backend.NodeID != "backend" {
		t.Fatalf("unexpected parallel tasks %s and %s", frontend.NodeID, backend.NodeID)
	}
	raw, err := json.Marshal(domain.TelemetryEvent{
		Category: "lifecycle", Name: "agent.session", SessionID: "frontend-session",
		Source: domain.TelemetrySource{Layer: "executor", Adapter: "codex-jsonl-v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendRunnerTaskEvents(frontend.ID, []domain.RunnerTaskEvent{{Seq: 1, Type: "observation", Data: string(raw), Ts: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	worker.NotifyRunnerTaskChanged(frontend.ID)
	started := waitRun(t, store, run.ID, 5*time.Second, func(r domain.Run) bool {
		worker.flushAllObservations()
		return r.Nodes["frontend"] != nil && r.Nodes["frontend"].SessionID == "frontend-session"
	})
	const guidance = "Use the existing filter component."
	request := NodeGuidanceRequest{NodeID: "frontend", Attempt: started.Nodes["frontend"].Attempt,
		StartedAt: started.Nodes["frontend"].StartedAt, Guidance: guidance}
	if err := api.GuideNode(context.Background(), run.ID, request); err != nil {
		t.Fatal(err)
	}
	complete(backend, domain.RunnerTaskSuccess, "Backend finished", "")
	mid := waitRun(t, store, run.ID, 5*time.Second, func(r domain.Run) bool {
		return r.Nodes["backend"] != nil && r.Nodes["backend"].Status == "succeeded"
	})
	active, err := store.ActiveRunnerTasksForRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != frontend.ID || active[0].DesiredState != domain.RunnerTaskDesiredCancel {
		t.Fatalf("expected only the previous frontend task to be stopping: %#v", active)
	}
	if mid.PauseRequested || mid.HumanContext != "" || mid.Nodes["backend"].Guidance != "" ||
		!mid.Nodes["backend"].StartedAt.Equal(started.Nodes["backend"].StartedAt) {
		t.Fatalf("guidance escaped the frontend node: %+v", mid.Nodes)
	}
	complete(frontend, domain.RunnerTaskCanceled, "", "")
	resumed := claim()
	if resumed.NodeID != "frontend" || resumed.Workspace != frontend.Workspace ||
		resumed.Invocation.ResumeSessionID != "frontend-session" || !strings.Contains(resumed.Prompt, guidance) {
		t.Fatalf("frontend did not resume with its workspace, session and guidance: %+v", resumed)
	}
	if err := api.GuideNode(context.Background(), run.ID, request); err == nil {
		t.Fatal("a response to the previous execution was accepted")
	}
	complete(resumed, domain.RunnerTaskFailed, "", "No conversation found with session ID: frontend-session")
	fallback := claim()
	if fallback.NodeID != "frontend" || fallback.Workspace != frontend.Workspace ||
		fallback.Invocation.ResumeSessionID != "" || !strings.Contains(fallback.Prompt, guidance) {
		t.Fatalf("fresh frontend session lost its workspace or guidance: %+v", fallback)
	}
	complete(fallback, domain.RunnerTaskSuccess, "Frontend finished", "")
	waitFor(t, store, run.ID, "succeeded")
	final, _ := store.RunState(run.ID)
	if final.Nodes["frontend"].Attempt != request.Attempt || nodeEvents(final, "frontend", "node.guidance") != 1 ||
		nodeEvents(final, "backend", "node.started") != 1 || nodeEvents(final, "frontend", "node.succeeded") != 1 {
		t.Fatalf("wrong attempts or guidance events: %+v", final.Nodes)
	}
}
