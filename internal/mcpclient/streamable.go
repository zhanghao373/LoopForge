package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

var ErrStreamableUnauthorized = errors.New("MCP authorization required")
var ErrStreamableRateLimited = errors.New("MCP rate limit exceeded")
var errStreamableRequest = errors.New("MCP request failed")

// Streamable is a negotiated HTTP MCP session used by callers that need
// strict session handling and structured results.
type Streamable struct {
	client    *Client
	sessionID string
	version   string
}

func OpenStreamable(ctx context.Context, endpoint string, headers map[string]string, client *http.Client) (*Streamable, error) {
	s := &Streamable{client: &Client{url: endpoint, hdrs: headers, http: client}}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := s.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "LoopForge", "version": "1.0.0"},
	}, &initialized); err != nil {
		return nil, err
	}
	if initialized.ProtocolVersion == "" {
		return nil, errStreamableRequest
	}
	s.version = initialized.ProtocolVersion
	if err := s.request(ctx, "notifications/initialized", nil, nil); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Streamable) Call(ctx context.Context, name string, args map[string]any, out any) error {
	var result struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := s.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &result); err != nil {
		return err
	}
	if result.IsError {
		return errStreamableRequest
	}
	if len(result.StructuredContent) > 0 && string(result.StructuredContent) != "null" {
		if json.Unmarshal(result.StructuredContent, out) == nil {
			return nil
		}
	}
	for _, block := range result.Content {
		if block.Type == "text" && json.Unmarshal([]byte(block.Text), out) == nil {
			return nil
		}
	}
	return errStreamableRequest
}

func (s *Streamable) Close(ctx context.Context) {
	if s.sessionID == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.client.url, nil)
	if err != nil {
		return
	}
	s.headers(req)
	if res, err := s.client.http.Do(req); err == nil {
		res.Body.Close()
	}
}

func (s *Streamable) headers(req *http.Request) {
	for key, value := range s.client.hdrs {
		req.Header.Set(key, value)
	}
	if s.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", s.sessionID)
	}
	if s.version != "" {
		req.Header.Set("MCP-Protocol-Version", s.version)
	}
}

func (s *Streamable) request(ctx context.Context, method string, params any, out any) error {
	payload := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		payload["params"] = params
	}
	if out != nil {
		s.client.nextID++
		payload["id"] = s.client.nextID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return errStreamableRequest
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.client.url, bytes.NewReader(body))
	if err != nil {
		return errStreamableRequest
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	s.headers(req)
	res, err := s.client.http.Do(req)
	if err != nil {
		return errStreamableRequest
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrStreamableUnauthorized
	case http.StatusTooManyRequests:
		return ErrStreamableRateLimited
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return errStreamableRequest
	}
	if out == nil {
		return nil
	}
	var raw []byte
	if strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		raw, err = s.client.readSSE(io.LimitReader(res.Body, 16<<20))
	} else {
		raw, err = io.ReadAll(io.LimitReader(res.Body, 16<<20))
	}
	var rpc rpcResponse
	if err != nil || json.Unmarshal(raw, &rpc) != nil || rpc.ID != s.client.nextID || rpc.Error != nil ||
		len(rpc.Result) == 0 || string(rpc.Result) == "null" || json.Unmarshal(rpc.Result, out) != nil {
		return errStreamableRequest
	}
	if method == "initialize" {
		s.sessionID = res.Header.Get("Mcp-Session-Id")
	}
	return nil
}
