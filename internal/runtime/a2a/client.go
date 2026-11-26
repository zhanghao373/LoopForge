package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

const maxResponseBytes = 1024 * 1024

// Client implements the A2A 1.0 JSON-RPC task operations used by SOP nodes.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
	nextID   atomic.Uint64
}

// NewClient takes the operator-configured URL of an A2A 1.0 JSON-RPC interface.
// HTTP redirects are disabled so credentials stay scoped to this endpoint.
func NewClient(endpoint, token string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("invalid A2A JSON-RPC endpoint")
	}
	if token == "" {
		return nil, errors.New("A2A bearer token is required")
	}
	return &Client{
		endpoint: u.String(),
		token:    token,
		http: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

type Part struct {
	Text      string          `json:"text,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	MediaType string          `json:"mediaType,omitempty"`
}

type Artifact struct {
	ID    string `json:"artifactId"`
	Name  string `json:"name"`
	Parts []Part `json:"parts"`
}

type Task struct {
	ID        string     `json:"id"`
	ContextID string     `json:"contextId"`
	Artifacts []Artifact `json:"artifacts"`
	Status    struct {
		State   string   `json:"state"`
		Message *Message `json:"message,omitempty"`
	} `json:"status"`
}

func (t Task) settled() (bool, error) {
	switch t.Status.State {
	case "TASK_STATE_SUBMITTED", "TASK_STATE_WORKING":
		return false, nil
	case "TASK_STATE_COMPLETED", "TASK_STATE_FAILED", "TASK_STATE_CANCELED",
		"TASK_STATE_REJECTED", "TASK_STATE_INPUT_REQUIRED", "TASK_STATE_AUTH_REQUIRED":
		return true, nil
	default:
		return false, errors.New("remote task returned an unknown state")
	}
}

type Message struct {
	ID       string           `json:"messageId"`
	Role     string           `json:"role"`
	Parts    []Part           `json:"parts"`
	Metadata *MessageMetadata `json:"metadata,omitempty"`
}

type MessageMetadata struct {
	TabbitRoom *RoomContext `json:"tabbitRoom,omitempty"`
}

type RoomContext struct {
	AccountID string `json:"accountId"`
	Scope     string `json:"scope"`
}

type SendInput struct {
	// Use the same ID when retrying a submission after a lost response.
	MessageID   string
	Instruction string
	Documents   map[string]string
	// A non-nil conversation selects dialogue, even on its first turn.
	Conversation []ConversationTurn
	Routing      *RoutingInput
	RoomContext  *RoomContext
}

type ConversationTurn struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
}

type RoutingCandidate struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type RoutingInput struct {
	Candidates         []RoutingCandidate `json:"candidates,omitempty"`
	History            []ConversationTurn `json:"history,omitempty"`
	AwaitingTicketName bool               `json:"awaiting_ticket_name,omitempty"`
}

func (c *Client) Send(ctx context.Context, input SendInput) (Task, error) {
	message := Message{
		ID:    input.MessageID,
		Role:  "ROLE_USER",
		Parts: []Part{{Text: input.Instruction}},
	}
	if input.RoomContext != nil {
		message.Metadata = &MessageMetadata{TabbitRoom: input.RoomContext}
	}
	if len(input.Documents) > 0 || input.Conversation != nil || input.Routing != nil {
		inputData := struct {
			Documents    map[string]string   `json:"documents,omitempty"`
			Conversation *[]ConversationTurn `json:"conversation,omitempty"`
			Routing      *RoutingInput       `json:"routing,omitempty"`
		}{Documents: input.Documents, Routing: input.Routing}
		if input.Conversation != nil {
			inputData.Conversation = &input.Conversation
		}
		data, err := json.Marshal(inputData)
		if err != nil {
			return Task{}, err
		}
		message.Parts = append(message.Parts, Part{Data: data})
	}
	var response struct {
		Task Task `json:"task"`
	}
	params := struct {
		Message       Message         `json:"message"`
		Configuration map[string]bool `json:"configuration"`
	}{
		Message:       message,
		Configuration: map[string]bool{"returnImmediately": true},
	}
	if err := c.call(ctx, "SendMessage", params, &response); err != nil {
		return Task{}, err
	}
	if response.Task.ID == "" {
		return Task{}, errors.New("remote agent did not return a task")
	}
	return response.Task, nil
}

type taskRequest struct {
	ID string `json:"id"`
}

func (c *Client) Get(ctx context.Context, taskID string) (Task, error) {
	return c.taskCall(ctx, "GetTask", taskID)
}

func (c *Client) Cancel(ctx context.Context, taskID string) (Task, error) {
	return c.taskCall(ctx, "CancelTask", taskID)
}

func (c *Client) taskCall(ctx context.Context, method, taskID string) (Task, error) {
	if taskID == "" {
		return Task{}, errors.New("A2A task ID is required")
	}
	var task Task
	if err := c.call(ctx, method, taskRequest{ID: taskID}, &task); err != nil {
		return Task{}, err
	}
	if task.ID != taskID {
		return Task{}, errors.New("A2A response returned the wrong task ID")
	}
	return task, nil
}

// Wait follows task state without canceling the remote task when ctx ends.
// SOP worker handoff and a user's stop action have different semantics.
func (c *Client) Wait(ctx context.Context, task Task) (Task, error) {
	return c.wait(ctx, task, nil)
}

// WaitWithStop checks for a durable caller stop request while following the task.
func (c *Client) WaitWithStop(ctx context.Context, task Task, stopRequested func() (bool, error)) (Task, error) {
	return c.wait(ctx, task, stopRequested)
}

func (c *Client) wait(ctx context.Context, task Task, stopRequested func() (bool, error)) (Task, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		settled, err := task.settled()
		if settled || err != nil {
			return task, err
		}
		if stopRequested != nil {
			stop, err := stopRequested()
			if err != nil {
				return Task{}, err
			}
			if stop {
				updated, err := c.Cancel(ctx, task.ID)
				if err != nil && !IsTaskNotCancelable(err) {
					return Task{}, err
				}
				if err == nil {
					task = updated
				}
				stopRequested = nil
				continue
			}
		}
		select {
		case <-ctx.Done():
			return Task{}, ctx.Err()
		case <-ticker.C:
		}
		task, err = c.Get(ctx, task.ID)
		if err != nil {
			return Task{}, err
		}
	}
}

type rpcRequest struct {
	Version string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcResponse struct {
	Version string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code int `json:"code"`
	} `json:"error"`
}

type rpcError struct {
	method string
	code   int
}

type httpStatusError struct {
	method string
	status int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("A2A %s returned HTTP %d", e.method, e.status)
}

// Retryable reports whether the caller should continue following the same task
// or repeat a submission with the same message ID and immutable input.
func Retryable(err error) bool {
	var status *httpStatusError
	if errors.As(err, &status) {
		return status.status == http.StatusRequestTimeout ||
			status.status == http.StatusTooManyRequests || status.status >= 500
	}
	var remote *rpcError
	if errors.As(err, &remote) {
		return remote.code == -32603
	}
	var transport net.Error
	return errors.As(err, &transport) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("A2A %s failed with RPC error %d", e.method, e.code)
}

func IsTaskNotCancelable(err error) bool {
	var remote *rpcError
	return errors.As(err, &remote) && remote.code == -32002
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	requestID := c.nextID.Add(1)
	body, err := json.Marshal(rpcRequest{
		Version: "2.0",
		ID:      requestID,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &httpStatusError{method: method, status: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponseBytes {
		return errors.New("A2A response exceeded its size limit")
	}
	var payload rpcResponse
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("invalid A2A response: %w", err)
	}
	if payload.Version != "2.0" || payload.ID != requestID {
		return errors.New("invalid A2A response envelope")
	}
	if payload.Error != nil {
		return &rpcError{method: method, code: payload.Error.Code}
	}
	return json.Unmarshal(payload.Result, result)
}
