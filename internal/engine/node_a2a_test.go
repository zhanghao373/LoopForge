package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
)

const a2aTestToken = "test-token"

type testA2ARequest struct {
	ID     uint64 `json:"id"`
	Method string `json:"method"`
	Params struct {
		ID      string `json:"id"`
		Message struct {
			ID    string `json:"messageId"`
			Parts []struct {
				Text string `json:"text"`
				Data struct {
					Documents map[string]string `json:"documents"`
				} `json:"data"`
			} `json:"parts"`
		} `json:"message"`
	} `json:"params"`
}

func serveSOPA2ATest(t *testing.T, handle func(testA2ARequest) map[string]any) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+a2aTestToken ||
			req.Header.Get("A2A-Version") != "1.0" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var rpc testA2ARequest
		if err := json.NewDecoder(req.Body).Decode(&rpc); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		response := handle(rpc)
		response["jsonrpc"] = "2.0"
		response["id"] = rpc.ID
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func a2aTestTask(id, state string) map[string]any {
	return map[string]any{
		"id": id, "status": map[string]string{"state": "TASK_STATE_" + state},
	}
}

func a2aTestCompletedTask(id string) map[string]any {
	task := a2aTestTask(id, "COMPLETED")
	task["artifacts"] = []any{map[string]any{"name": "result", "parts": []any{
		map[string]string{"text": "Reviewed"},
		map[string]any{"data": map[string]any{
			"gateResult": map[string]string{"decision": "pass", "reason": "checked"},
			"outputs":    map[string]string{"report": "Reviewed the input."},
		}},
	}}}
	return task
}

func newA2ASOP(t *testing.T, endpoint, token string) (*Engine, *repo.Store, domain.Run) {
	t.Helper()
	prepare := scriptNode("prepare", "printf 'check this change' > source.txt")
	prepare.ArtifactOutputs = []domain.ArtifactContract{{Name: "source", Path: "source.txt", MediaType: "text/plain", Required: true}}
	review := domain.WorkflowNode{
		ID: "review", Name: "review", Type: "agent", AgentID: "remote-reviewer", MaxAttempts: 1,
		ArtifactOutputs: []domain.ArtifactContract{{Name: "report", Path: "review.md", Required: true}},
	}
	verify := scriptNode("verify", "grep -F Reviewed .agentflow/inputs/review/report.md")
	wf := domain.WorkflowDefinition{
		ID: "remote-review", Name: "remote review", SandboxProfileID: "local",
		Nodes: []domain.WorkflowNode{prepare, review, verify},
		Edges: []domain.WorkflowEdge{edge("prepare", "review"), edge("review", "verify")},
	}
	e, s := parallelTestEngine(t, wf)
	err := s.PutAgent(domain.AgentDefinition{
		ID: "remote-reviewer", Name: "Remote reviewer", ExecutorID: domain.A2AExecutorID,
		Prompt: "Review the input.",
		Verification: []domain.VerificationGate{
			{ID: "human", Name: "Confirm review", Mode: "manual", Required: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ConfigureA2A(endpoint, token); err != nil {
		t.Fatal(err)
	}
	run, err := e.Start(wf.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	return e, s, run
}

func assertA2ASOP(t *testing.T, e *Engine, s *repo.Store, run domain.Run) string {
	t.Helper()
	waitFor(t, s, run.ID, "waiting")
	before, _ := s.Run(run.ID)
	review := before.Nodes["review"]
	if review.Status != "waiting_approval" || review.A2ATaskID == "" || review.SessionID != "" ||
		review.GateResult == nil || review.GateResult.Decision != "pass" {
		t.Fatalf("review did not enter the existing manual gate: %+v", review)
	}
	if before.Nodes["verify"].Status != "pending" {
		t.Fatalf("downstream node ran before approval: %+v", before.Nodes["verify"])
	}
	if err := e.Approve(run.ID, "review", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, run.ID, "succeeded")
	after, _ := s.Run(run.ID)
	if after.Nodes["verify"].Status != "succeeded" || after.Nodes["review"].A2ATaskID != review.A2ATaskID {
		t.Fatalf("review output did not reach the downstream verification: %+v", after.Nodes)
	}
	return review.A2ATaskID
}

func TestSOPA2AUsesExistingGateAndArtifactHandoff(t *testing.T) {
	submitted := make(chan testA2ARequest, 1)
	endpoint := serveSOPA2ATest(t, func(req testA2ARequest) map[string]any {
		if req.Method != "SendMessage" {
			return map[string]any{"error": map[string]int{"code": -32601}}
		}
		select {
		case submitted <- req:
		default:
		}
		return map[string]any{"result": map[string]any{"task": a2aTestCompletedTask("remote-task")}}
	})
	e, store, run := newA2ASOP(t, endpoint, a2aTestToken)
	if got := assertA2ASOP(t, e, store, run); got != "remote-task" {
		t.Fatalf("persisted task ID = %q", got)
	}
	request := <-submitted
	if request.Params.Message.ID != a2aMessageID(run.ID, "review", 1) ||
		len(request.Params.Message.Parts) != 2 ||
		!strings.Contains(request.Params.Message.Parts[0].Text, "Review the input.") ||
		!reflect.DeepEqual(request.Params.Message.Parts[1].Data.Documents, map[string]string{
			".agentflow/inputs/prepare/source.txt": "check this change",
		}) {
		t.Fatalf("unexpected remote request: %+v", request)
	}
}

func TestSOPA2ACancelStopsLinkedTask(t *testing.T) {
	stopped := make(chan string, 1)
	endpoint := serveSOPA2ATest(t, func(req testA2ARequest) map[string]any {
		switch req.Method {
		case "SendMessage":
			return map[string]any{"result": map[string]any{"task": a2aTestTask("live-task", "WORKING")}}
		case "GetTask":
			return map[string]any{"result": a2aTestTask("live-task", "WORKING")}
		case "CancelTask":
			select {
			case stopped <- req.Params.ID:
			default:
			}
			return map[string]any{"result": a2aTestTask("live-task", "CANCELED")}
		default:
			return map[string]any{"error": map[string]int{"code": -32601}}
		}
	})
	e, store, run := newA2ASOP(t, endpoint, a2aTestToken)
	waitRun(t, store, run.ID, 10*time.Second, func(r domain.Run) bool {
		return r.Nodes["review"] != nil && r.Nodes["review"].A2ATaskID == "live-task"
	})
	if err := e.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-stopped:
		if id != "live-task" {
			t.Fatalf("cancel routed to task %q", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CancelTask was not sent")
	}
	waitFor(t, store, run.ID, "cancelled")
}

func TestSOPA2AWorkerReplacementReusesTask(t *testing.T) {
	var attached atomic.Bool
	var submissions, cancellations atomic.Int32
	endpoint := serveSOPA2ATest(t, func(req testA2ARequest) map[string]any {
		switch req.Method {
		case "SendMessage":
			submissions.Add(1)
			return map[string]any{"result": map[string]any{"task": a2aTestTask("live-task", "WORKING")}}
		case "GetTask":
			if attached.Load() {
				return map[string]any{"result": a2aTestCompletedTask(req.Params.ID)}
			}
			return map[string]any{"result": a2aTestTask("live-task", "WORKING")}
		case "CancelTask":
			cancellations.Add(1)
			return map[string]any{"result": a2aTestTask(req.Params.ID, "CANCELED")}
		default:
			return map[string]any{"error": map[string]int{"code": -32601}}
		}
	})
	first, store, run := newA2ASOP(t, endpoint, a2aTestToken)
	waitRun(t, store, run.ID, 10*time.Second, func(r domain.Run) bool {
		return r.Nodes["review"] != nil && r.Nodes["review"].A2ATaskID == "live-task"
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := first.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	parked, _ := store.RunState(run.ID)
	if got := parked.Nodes["review"]; got.Attempt != 1 || got.Status != "pending" || got.A2ATaskID != "live-task" {
		t.Fatalf("drain lost the task link: %+v", got)
	}
	attached.Store(true)
	replacement := New(store, runtime.NewManager(first.sandboxes.Root()))
	replacement.ConfigureExecution("replacement", first.workspaceHost, 30*time.Second, true)
	if err := replacement.ConfigureA2A(endpoint, a2aTestToken); err != nil {
		t.Fatal(err)
	}
	if ids := replacement.ResumeInterruptedRuns(); !reflect.DeepEqual(ids, []string{run.ID}) {
		t.Fatalf("replacement resumed %v, wanted %s", ids, run.ID)
	}
	assertA2ASOP(t, replacement, store, run)
	if submissions.Load() != 1 || cancellations.Load() != 0 {
		t.Fatalf("replacement submitted %d tasks and canceled %d", submissions.Load(), cancellations.Load())
	}
}

func TestSOPA2AAgainstRuntime(t *testing.T) {
	endpoint := os.Getenv("AGENT_RUNTIME_SOP_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set AGENT_RUNTIME_SOP_TEST_ENDPOINT to test the real SOP against an A2A server")
	}
	e, store, run := newA2ASOP(t, endpoint, os.Getenv("AGENT_RUNTIME_TEST_TOKEN"))
	assertA2ASOP(t, e, store, run)
}
