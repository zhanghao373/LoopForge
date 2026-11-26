package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestSendAndCancel(t *testing.T) {
	client, err := NewClient("http://agent.test/a2a", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	var methods []string
	client.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("A2A-Version") != "1.0" {
			t.Fatal("unexpected headers")
		}
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		methods = append(methods, req.Method)
		if req.Method == "SendMessage" {
			params := req.Params.(map[string]any)
			message := params["message"].(map[string]any)
			if message["messageId"] != "stable" || message["role"] != "ROLE_USER" ||
				params["configuration"].(map[string]any)["returnImmediately"] != true {
				t.Fatal("unexpected submit payload")
			}
			data := message["parts"].([]any)[1].(map[string]any)["data"].(map[string]any)
			conversation, isConversation := data["conversation"].([]any)
			if !isConversation || len(conversation) != 0 ||
				data["documents"].(map[string]any)["attachment.txt"] != "Unit price: 63" {
				t.Fatalf("first conversation turn lost its attachment or empty history: %#v", data)
			}
		}
		state := "TASK_STATE_SUBMITTED"
		if req.Method == "CancelTask" {
			state = "TASK_STATE_CANCELED"
		}
		task := map[string]any{"id": "task-id", "status": map[string]string{"state": state}}
		var result any = task
		if req.Method == "SendMessage" {
			result = map[string]any{"task": task}
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	task, err := client.Send(context.Background(), SendInput{
		MessageID: "stable", Instruction: "Review", Conversation: []ConversationTurn{},
		Documents: map[string]string{"attachment.txt": "Unit price: 63"},
	})
	if err != nil || task.ID != "task-id" {
		t.Fatalf("submit: %v, %+v", err, task)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Wait(ctx, task); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait: %v", err)
	}
	if len(methods) != 1 || methods[0] != "SendMessage" {
		t.Fatalf("wait unexpectedly changed the remote task: %v", methods)
	}
	result, err := client.Cancel(context.Background(), task.ID)
	if err != nil || result.Status.State != "TASK_STATE_CANCELED" {
		t.Fatalf("cancel: %v, %+v", err, result)
	}
}

func TestAgainstRuntime(t *testing.T) {
	endpoint := os.Getenv("AGENT_RUNTIME_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set AGENT_RUNTIME_TEST_ENDPOINT for live A2A interoperability")
	}
	client, err := NewClient(endpoint, os.Getenv("AGENT_RUNTIME_TEST_TOKEN"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	input := SendInput{
		MessageID:   time.Now().UTC().Format(time.RFC3339Nano),
		Instruction: "Read the provided diff and return a review.",
		Documents:   map[string]string{"diff": "+ example"},
	}
	first, err := client.Send(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Send(ctx, input)
	if err != nil || second.ID != first.ID {
		t.Fatalf("idempotent retry: %v, %q vs %q", err, second.ID, first.ID)
	}
	finished, err := client.Wait(ctx, second)
	if err != nil || finished.Status.State != "TASK_STATE_COMPLETED" {
		t.Fatalf("wait: %v, %+v", err, finished)
	}
	if len(finished.Artifacts) != 1 || len(finished.Artifacts[0].Parts) == 0 ||
		finished.Artifacts[0].Parts[0].Text != "Reviewed" {
		t.Fatalf("unexpected artifact: %+v", finished.Artifacts)
	}
}
