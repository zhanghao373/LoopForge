package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestLinearMCPOAuthAndReadTools(t *testing.T) {
	ctx := context.Background()
	l := NewLinearMCPOAuth()
	const callback = "http://127.0.0.1:18090/api/sync/linear/mcp/callback"
	const state = "oauth-state"
	const verifier = "pkce-verifier"
	const projectID = "linear-project"
	const issue = `{"id":"DEV-1","uuid":"74caf43a-527b-4746-98e2-8cae9899e1e0","title":"审计","description":"details","url":"https://linear.app/example/issue/DEV-1","status":"Under Review","statusType":"started","priority":{"value":2,"name":"High"},"updatedAt":"2026-11-25T08:00:00Z"}`
	var toolCalls []string
	l.http.Transport = linearRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "mcp.linear.app" {
			t.Fatalf("unexpected upstream: %s", r.URL)
		}
		jsonResponse := func(code int, data string) *http.Response {
			return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(data))}
		}
		switch r.URL.Path {
		case "/register":
			var registration struct {
				RedirectURIs []string `json:"redirect_uris"`
				Scope        string   `json:"scope"`
				Auth         string   `json:"token_endpoint_auth_method"`
			}
			if err := json.NewDecoder(r.Body).Decode(&registration); err != nil ||
				len(registration.RedirectURIs) != 1 || registration.RedirectURIs[0] != callback ||
				registration.Scope != "read" || registration.Auth != "none" {
				t.Fatalf("bad registration: %+v %v", registration, err)
			}
			return jsonResponse(201, `{"client_id":"test-client"}`), nil
		case "/token":
			if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != "test-client" ||
				r.Form.Get("resource") != linearMCPResource {
				t.Fatalf("bad token request: %v", err)
			}
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				if r.Form.Get("code_verifier") != verifier || r.Form.Get("redirect_uri") != callback || r.Form.Get("code") != "authorization-code" {
					t.Fatal("exchange must match the registered redirect URI and PKCE challenge")
				}
				return jsonResponse(200, `{"access_token":"first-token","refresh_token":"refresh-token","expires_in":3600,"scope":"read"}`), nil
			case "refresh_token":
				if r.Form.Get("refresh_token") != "refresh-token" {
					t.Fatal("missing refresh token")
				}
				return jsonResponse(200, `{"access_token":"test-token","expires_in":3600,"scope":"read"}`), nil
			default:
				t.Fatal("unexpected OAuth grant type")
			}
		case "/mcp/readonly":
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Fatal("MCP did not use the OAuth token")
			}
			if r.Method == http.MethodDelete {
				return jsonResponse(204, ""), nil
			}
			var rpc struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
				t.Fatal(err)
			}
			if rpc.Method == "initialize" {
				res := jsonResponse(200, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18"}}`, rpc.ID))
				res.Header.Set("Mcp-Session-Id", "test-session")
				return res, nil
			}
			if r.Header.Get("Mcp-Session-Id") != "test-session" || r.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
				t.Fatal("missing MCP session or protocol header")
			}
			if rpc.Method == "notifications/initialized" {
				if rpc.ID != 0 {
					t.Fatal("notifications must not have a request ID")
				}
				return jsonResponse(202, ""), nil
			}
			if rpc.Method != "tools/call" {
				t.Fatalf("unexpected method: %s", rpc.Method)
			}
			toolCalls = append(toolCalls, rpc.Params.Name)
			if rpc.Params.Name == "list_projects" {
				data := `{"projects":[{"id":"linear-project","name":"My project"}],"hasNextPage":false}`
				return jsonResponse(200, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"structuredContent":%s}}`, rpc.ID, data)), nil
			}
			args := rpc.Params.Arguments
			if rpc.Params.Name != "list_issues" || args["project"] != projectID || args["state"] != "started" {
				t.Fatalf("issue request didn't respect project and status: %+v", rpc.Params)
			}
			data := `{"issues":[` + issue + `],"hasNextPage":false}`
			if args["cursor"] == nil {
				data = `{"issues":[{"id":"other","title":"Irrelevant","priority":2}],"hasNextPage":true,"cursor":"page2"}`
			} else if args["cursor"] != "page2" {
				t.Fatalf("unexpected cursor: %v", args["cursor"])
			}
			result := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%q}]}}`, rpc.ID, data)
			res := jsonResponse(200, "data: "+result+"\n\n")
			res.Header.Set("Content-Type", "text/event-stream")
			return res, nil
		default:
			t.Fatalf("unexpected endpoint: %s", r.URL.Path)
		}
		return nil, fmt.Errorf("unexpected request")
	})
	clientID, err := l.Register(ctx, callback)
	if err != nil || clientID != "test-client" {
		t.Fatalf("register: %s %v", clientID, err)
	}
	authorize, err := url.Parse(l.AuthorizeURL(clientID, callback, state, "s256-code"))
	if err != nil || authorize.Query().Get("scope") != "read" || authorize.Query().Get("resource") != linearMCPResource ||
		authorize.Query().Get("state") != state || authorize.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("invalid OAuth authorize URL")
	}
	grant, err := l.Exchange(ctx, clientID, callback, "authorization-code", verifier)
	if err != nil || grant.AccessToken != "first-token" {
		t.Fatalf("exchange failed: %v", err)
	}
	grant, err = l.Refresh(ctx, grant)
	if err != nil || grant.AccessToken != "test-token" || grant.RefreshToken != "refresh-token" {
		t.Fatalf("refresh failed: %v", err)
	}
	projects, err := l.Projects(ctx, grant.AccessToken)
	if err != nil || len(projects) != 1 || projects[0].ID != projectID {
		t.Fatalf("list projects: %+v %v", projects, err)
	}
	high := 2
	issues, more, err := l.Issues(ctx, grant.AccessToken, projectID, LinearIssueFilter{State: "started", Priority: &high, Query: "审计"}, true)
	if err != nil || more || len(issues) != 1 || issues[0].ID != "74caf43a-527b-4746-98e2-8cae9899e1e0" || issues[0].Identifier != "DEV-1" || issues[0].State.Type != "started" {
		t.Fatalf("list issues: %+v %v %v", issues, more, err)
	}
	if strings.Join(toolCalls, ",") != "list_projects,list_issues,list_issues" {
		t.Fatalf("unexpected tools called: %v", toolCalls)
	}
}
