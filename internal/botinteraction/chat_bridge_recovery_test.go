package botinteraction

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime/a2a"
)

type bridgeRPCStep struct {
	method string
	status int
	state  string
}

type bridgeRPCCall struct {
	ID     uint64
	Method string
	Params json.RawMessage
}

func recoveryBridge(t *testing.T, steps []bridgeRPCStep) (*ChatBridge, func() []bridgeRPCCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []bridgeRPCCall
	)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call bridgeRPCCall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		index := len(calls)
		calls = append(calls, call)
		mu.Unlock()
		if index >= len(steps) || call.Method != steps[index].method {
			t.Errorf("unexpected A2A call %d: %s", index, call.Method)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		step := steps[index]
		if step.status != 0 {
			w.WriteHeader(step.status)
			return
		}
		task := map[string]any{
			"id": "remote-task", "status": map[string]string{"state": "TASK_STATE_" + step.state},
			"artifacts": []any{map[string]any{"parts": []any{map[string]string{"text": "verified reply"}}}},
		}
		var result any = task
		if call.Method == "SendMessage" {
			result = map[string]any{"task": task}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(remote.Close)

	var store *repo.Store
	var err error
	if dsn := os.Getenv("AGENTFLOW_BOT_TEST_MYSQL_DSN"); dsn != "" {
		store, err = repo.Open(dsn)
	} else {
		store, err = repo.OpenTest()
	}
	if err != nil {
		t.Fatal(err)
	}
	client, err := a2a.NewClient(remote.URL, "test")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	botID := fmt.Sprintf("web-chat:recovery-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		store.DB.Where("bot_id = ?", botID).Delete(&domain.BotInteraction{})
		store.DB.Where("bot_id = ?", botID).Delete(&domain.BotConversation{})
		store.DB.Where("bot_id = ?", botID).Delete(&domain.BotConnection{})
		store.Close()
	})
	bridge, err := newChatBridge(store, client, botID, []string{"1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge.webStop = true
	return bridge, func() []bridgeRPCCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]bridgeRPCCall(nil), calls...)
	}
}

func TestWebChatRecoversTheSameTask(t *testing.T) {
	bridge, calls := recoveryBridge(t, []bridgeRPCStep{
		{method: "SendMessage", status: 503},
		{method: "SendMessage", state: "WORKING"},
		{method: "GetTask", status: 503},
		{method: "GetTask", state: "COMPLETED"},
	})
	accept := func(key, thread string) string {
		t.Helper()
		id, err := bridge.Accept(ChatEvent{
			MessageID: key, Kind: "single", SenderID: "1", Text: "test",
			InputDocument: "attachment", WebThread: thread,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := accept("first", "")
	second := accept("second", "")
	other := accept("other", "parallel")
	claimConnection(t, bridge, "initial")
	executeBridgeTurn(t, bridge, "initial", first)
	row, err := bridge.store.BotInteraction(first)
	if err != nil || row.Status != "queued" || row.A2ARetryAt == nil || row.A2AConversation != "[]" || row.A2ATaskID != "" {
		t.Fatalf("lost submission was not preserved: status=%s err=%v", row.Status, err)
	}
	if item, err := bridge.store.ClaimBridgeTurn(bridge.botID, "initial", 0); err != nil || item == nil || item.ID != other {
		t.Fatalf("the delayed head blocked another conversation or was overtaken: %v", err)
	}
	if item, err := bridge.store.ClaimBridgeTurn(bridge.botID, "initial", 0); err != nil || item != nil {
		t.Fatalf("expected both conversations to wait: %v", err)
	}

	makeBridgeTurnDue(t, bridge, first)
	executeBridgeTurn(t, bridge, "initial", first)
	row, err = bridge.store.BotInteraction(first)
	if err != nil || row.Status != "queued" || row.A2ARetryAt == nil || row.A2ATaskID != "remote-task" {
		t.Fatalf("temporary GetTask failure lost its remote link: status=%s err=%v", row.Status, err)
	}
	if err := bridge.store.ReleaseBotConnection(bridge.botID, "initial"); err != nil {
		t.Fatal(err)
	}
	claimConnection(t, bridge, "replacement")
	if err := bridge.store.RequeueBridgeTurns(bridge.botID, "replacement"); err != nil {
		t.Fatal(err)
	}
	makeBridgeTurnDue(t, bridge, first)
	executeBridgeTurn(t, bridge, "replacement", first)
	state, reply, err := bridge.Lookup(first)
	if err != nil || state != "replied" || reply != "verified reply" {
		t.Fatalf("same task did not finish: %s %q %v", state, reply, err)
	}
	passed := calls()
	if len(passed) != 4 || !reflect.DeepEqual(passed[0].Params, passed[1].Params) {
		t.Fatalf("expected an identical retry and two GetTask requests; got %d calls", len(passed))
	}
	if item, err := bridge.store.ClaimBridgeTurn(bridge.botID, "replacement", 0); err != nil || item == nil || item.ID != second {
		t.Fatalf("next turn did not remain in queue until recovery: %v", err)
	}
}

func TestWebChatContinuesCancelAfterTemporaryFailure(t *testing.T) {
	bridge, calls := recoveryBridge(t, []bridgeRPCStep{
		{method: "SendMessage", state: "WORKING"},
		{method: "GetTask", status: 503},
		{method: "GetTask", state: "WORKING"},
		{method: "CancelTask", status: 503},
		{method: "GetTask", state: "WORKING"},
		{method: "CancelTask", state: "CANCELED"},
	})
	id, err := bridge.Accept(ChatEvent{MessageID: "stop", Kind: "single", SenderID: "1", Text: "test"})
	if err != nil {
		t.Fatal(err)
	}
	claimConnection(t, bridge, "current")
	executeBridgeTurn(t, bridge, "current", id)
	if err := bridge.Stop("1", "", id); err != nil {
		t.Fatal(err)
	}
	executeBridgeTurn(t, bridge, "current", id)
	stopping, err := bridge.store.BridgeStopRequested(bridge.botID, id)
	if err != nil || !stopping {
		t.Fatalf("stop was lost after the temporary failure: %v", err)
	}
	makeBridgeTurnDue(t, bridge, id)
	executeBridgeTurn(t, bridge, "current", id)
	state, reply, err := bridge.Lookup(id)
	if err != nil || state != "canceled" || reply != "" || len(calls()) != 6 {
		t.Fatalf("same task did not cancel: %s %q %v", state, reply, err)
	}
}

func claimConnection(t *testing.T, bridge *ChatBridge, owner string) {
	t.Helper()
	if claimed, err := bridge.store.ClaimBotConnection(bridge.botID, owner, time.Now().UTC()); err != nil || !claimed {
		t.Fatalf("claim connection: %t %v", claimed, err)
	}
}

func makeBridgeTurnDue(t *testing.T, bridge *ChatBridge, id string) {
	t.Helper()
	if err := bridge.store.DB.Model(&domain.BotInteraction{}).Where("id = ?", id).
		Update("a2a_retry_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
}

func executeBridgeTurn(t *testing.T, bridge *ChatBridge, owner, id string) {
	t.Helper()
	item, err := bridge.store.ClaimBridgeTurn(bridge.botID, owner, 0)
	if err != nil || item == nil || item.ID != id {
		t.Fatalf("expected to claim %s: %v", id, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := bridge.runTurn(ctx, owner, *item); err != nil {
		t.Fatal(err)
	}
}
