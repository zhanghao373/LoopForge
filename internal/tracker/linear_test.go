package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type linearRoundTrip func(*http.Request) (*http.Response, error)

func (f linearRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLinearProjectsAndFilteredIssues(t *testing.T) {
	type response struct {
		operation string
		after     string
		first     float64
		body      string
	}
	responses := []response{
		{"ImportProjects", "", 0, `{"data":{"projects":{"nodes":[{"id":"p1","name":"One"}],"pageInfo":{"hasNextPage":true,"endCursor":"projects-2"}}}}`},
		{"ImportProjects", "projects-2", 0, `{"data":{"projects":{"nodes":[{"id":"p2","name":"Two"}],"pageInfo":{"hasNextPage":false}}}}`},
		{"ImportIssues", "", 20, `{"data":{"project":{"issues":{"nodes":[{"id":"i1","identifier":"ENG-1","title":"Login","state":{"name":"In progress","type":"started"}}],"pageInfo":{"hasNextPage":true,"endCursor":"issues-2"}}}}}`},
		{"ImportIssues", "", 50, `{"data":{"project":{"issues":{"nodes":[{"id":"i1","identifier":"ENG-1"}],"pageInfo":{"hasNextPage":true,"endCursor":"issues-2"}}}}}`},
		{"ImportIssues", "issues-2", 50, `{"data":{"project":{"issues":{"nodes":[{"id":"i2","identifier":"ENG-2"}],"pageInfo":{"hasNextPage":false}}}}}`},
	}
	clicks := 0
	client := NewLinearClient()
	client.http.Transport = linearRoundTrip(func(req *http.Request) (*http.Response, error) {
		t.Helper()
		if clicks >= len(responses) {
			t.Fatal("unexpected extra Linear request")
		}
		expected := responses[clicks]
		clicks++
		var payload struct {
			Query     string
			Variables map[string]json.RawMessage
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if req.Method != http.MethodPost || req.URL.String() != linearEndpoint || req.Header.Get("Authorization") != "lin_api_placeholder" ||
			!strings.HasPrefix(payload.Query, "query "+expected.operation) {
			t.Fatalf("unexpected Linear request: %s %s", req.Method, req.URL)
		}
		var after string
		if value := payload.Variables["after"]; value != nil {
			if err := json.Unmarshal(value, &after); err != nil {
				t.Fatal(err)
			}
		}
		if after != expected.after {
			t.Fatalf("cursor = %q, want %q", after, expected.after)
		}
		if expected.operation == "ImportIssues" {
			const expectedFilter = `{"or":[{"title":{"containsIgnoreCase":"auth"}},{"description":{"containsIgnoreCase":"auth"}}],"priority":{"eq":0},"state":{"type":{"eq":"started"}}}`
			var first float64
			if err := json.Unmarshal(payload.Variables["first"], &first); err != nil || first != expected.first ||
				string(payload.Variables["id"]) != `"p1"` || string(payload.Variables["filter"]) != expectedFilter {
				t.Fatalf("unexpected issue variables: %s", payload.Variables)
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(expected.body))}, nil
	})
	ctx := context.Background()
	projects, err := client.Projects(ctx, "lin_api_placeholder")
	if err != nil || len(projects) != 2 || projects[1].ID != "p2" {
		t.Fatalf("projects=%+v, err=%v", projects, err)
	}
	zero := 0
	filter := LinearIssueFilter{State: "started", Query: "auth", Priority: &zero}
	preview, more, err := client.Issues(ctx, "lin_api_placeholder", "p1", filter, true)
	if err != nil || !more || len(preview) != 1 || preview[0].State.Name != "In progress" {
		t.Fatalf("preview=%+v, more=%v, err=%v", preview, more, err)
	}
	all, more, err := client.Issues(ctx, "lin_api_placeholder", "p1", filter, false)
	if err != nil || more || len(all) != 2 || all[1].Identifier != "ENG-2" || clicks != len(responses) {
		t.Fatalf("all=%+v, more=%v, err=%v, requests=%d", all, more, err, clicks)
	}
}

func TestLinearRejectsPartialGraphQLResponse(t *testing.T) {
	client := NewLinearClient()
	client.http.Transport = linearRoundTrip(func(*http.Request) (*http.Response, error) {
		body := `{"data":{"projects":{"nodes":[{"id":"p1","name":"One"}]}},"errors":[{"message":"secret","extensions":{"code":"RATELIMITED"}}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	projects, err := client.Projects(context.Background(), "lin_api_placeholder")
	if projects != nil || !errors.Is(err, ErrLinearRate) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("projects=%+v, err=%v", projects, err)
	}
}
