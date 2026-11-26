package httpapi

import (
	"agentflow/internal/botinteraction"
	"agentflow/internal/repo"
	"agentflow/internal/runtime/a2a"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestChatBridgeDirectAndGroup(t *testing.T) {
	model := newChatTestModel(t)
	defer model.Close()
	testChatBridgeConversation(t, model.URL, "test", true)
}

func newChatTestModel(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ID     uint64
			Method string
			Params struct {
				Message struct {
					Parts []a2a.Part
				}
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil ||
			input.Method != "SendMessage" || len(input.Params.Message.Parts) != 2 {
			t.Error("A2A received an invalid request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		instruction := input.Params.Message.Parts[0].Text
		var history struct {
			Conversation *[]a2a.ConversationTurn `json:"conversation"`
		}
		if err := json.Unmarshal(input.Params.Message.Parts[1].Data, &history); err != nil || history.Conversation == nil {
			t.Error("A2A did not receive a structured conversation")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply := "new conversation"
		if instruction == "remember moon" {
			reply = "remembered moon"
		}
		if instruction == "what did I say?" {
			for _, turn := range *history.Conversation {
				if strings.Contains(turn.User, "remember moon") && turn.Assistant == "remembered moon" {
					reply = "you said moon"
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": input.ID,
			"result": map[string]any{"task": map[string]any{
				"id": "task-1", "status": map[string]string{"state": "TASK_STATE_COMPLETED"},
				"artifacts": []any{map[string]any{"parts": []any{map[string]string{"text": reply}}}},
			}},
		})
	}))
}

func TestChatBridgeAgainstRuntime(t *testing.T) {
	url := os.Getenv("AGENT_BRIDGE_TEST_RUNTIME_ENDPOINT")
	if url == "" {
		t.Skip("AGENT_BRIDGE_TEST_RUNTIME_ENDPOINT is not configured")
	}
	testChatBridgeConversation(t, url, os.Getenv("AGENT_BRIDGE_TEST_RUNTIME_TOKEN"), false)
	t.Run("web", func(t *testing.T) {
		testWebChat(t, url, os.Getenv("AGENT_BRIDGE_TEST_RUNTIME_TOKEN"))
	})
}

func testChatBridgeConversation(t *testing.T, endpoint, runtimeToken string, testRejections bool) {
	t.Helper()
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client, err := a2a.NewClient(endpoint, runtimeToken)
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := botinteraction.NewChatBridge(store, client, fmt.Sprintf("test-%d", time.Now().UnixNano()),
		[]string{"alice", "bob"}, []string{"team-a", "team-b"})
	if err != nil {
		t.Fatal(err)
	}
	handler := ChatBridgeHandler(bridge, "test-bridge-token")
	request := func(method, path, token string, body any) (*httptest.ResponseRecorder, map[string]string) {
		t.Helper()
		buff, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, ChatBridgePath+path, bytes.NewReader(buff))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var payload map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return rec, payload
	}
	post := func(event botinteraction.ChatEvent, status int) string {
		t.Helper()
		rec, payload := request(http.MethodPost, "messages", "test-bridge-token", event)
		if rec.Code != status {
			t.Fatalf("message %s: got HTTP %d, want %d", event.MessageID, rec.Code, status)
		}
		return payload["id"]
	}
	group := botinteraction.ChatEvent{MessageID: "g1", Kind: "group", ChatID: "team-a", SenderID: "alice", Mentioned: true, Text: "remember moon"}
	firstGroupID := post(group, 202)
	if duplicate := post(group, 202); firstGroupID == "" || duplicate != firstGroupID {
		t.Fatalf("duplicate received a different ID")
	}
	if testRejections {
		original := group
		group.Text = "changed body"
		post(group, 409)
		group = original
		group.MessageID = "g-unmentioned"
		group.Mentioned = false
		post(group, 403)
		group.Mentioned = true
		group.SenderID = "unknown-sender"
		post(group, 403)
		rec, _ := request(http.MethodGet, "messages/"+firstGroupID, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized lookup returned %d", rec.Code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		bridge.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	await := func(id, want string) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			rec, payload := request(http.MethodGet, "messages/"+id, "test-bridge-token", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("lookup returned HTTP %d", rec.Code)
			}
			switch payload["state"] {
			case "replied":
				if payload["reply"] != want {
					t.Fatalf("reply = %q, want %q", payload["reply"], want)
				}
				return
			case "reply_failed":
				t.Fatal("chat turn failed")
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatal("chat turn did not finish")
	}
	await(firstGroupID, "remembered moon")
	await(post(botinteraction.ChatEvent{MessageID: "g2", Kind: "group", ChatID: "team-a", SenderID: "bob", Mentioned: true, Text: "what did I say?"}, 202), "you said moon")
	await(post(botinteraction.ChatEvent{MessageID: "g3", Kind: "group", ChatID: "team-b", SenderID: "alice", Mentioned: true, Text: "what did I say?"}, 202), "new conversation")
	await(post(botinteraction.ChatEvent{MessageID: "d1", Kind: "single", SenderID: "alice", Text: "what did I say?"}, 202), "new conversation")
	await(post(botinteraction.ChatEvent{MessageID: "d2", Kind: "single", SenderID: "alice", Text: "remember moon"}, 202), "remembered moon")
	await(post(botinteraction.ChatEvent{MessageID: "d3", Kind: "single", SenderID: "alice", Text: "what did I say?"}, 202), "you said moon")
	await(post(botinteraction.ChatEvent{MessageID: "d4", Kind: "single", SenderID: "bob", Text: "what did I say?"}, 202), "new conversation")
}
