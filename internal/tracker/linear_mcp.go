package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/mcpclient"
)

type linearMCPPage[T any] struct {
	Projects    []T    `json:"projects"`
	Issues      []T    `json:"issues"`
	HasNextPage bool   `json:"hasNextPage"`
	HasMore     bool   `json:"hasMore"`
	NextCursor  string `json:"nextCursor"`
	Cursor      string `json:"cursor"`
}

func (p linearMCPPage[T]) next() string {
	if p.NextCursor != "" {
		return p.NextCursor
	}
	if p.HasNextPage || p.HasMore {
		return p.Cursor
	}
	return ""
}

func linearMCPError(err error) error {
	switch {
	case errors.Is(err, mcpclient.ErrStreamableUnauthorized):
		return ErrLinearMCPAuthorization
	case errors.Is(err, mcpclient.ErrStreamableRateLimited):
		return ErrLinearRate
	default:
		return errLinearMCPRequest
	}
}

func (l *LinearMCPOAuth) open(ctx context.Context, token string) (*mcpclient.Streamable, error) {
	return mcpclient.OpenStreamable(ctx, linearMCPResource, map[string]string{"Authorization": "Bearer " + token}, l.http)
}

func (l *LinearMCPOAuth) Projects(ctx context.Context, token string) ([]LinearProject, error) {
	session, err := l.open(ctx, token)
	if err != nil {
		return nil, linearMCPError(err)
	}
	defer session.Close(ctx)
	var results = make([]LinearProject, 0)
	args := map[string]any{"limit": 50}
	for {
		var page linearMCPPage[LinearProject]
		if err := session.Call(ctx, "list_projects", args, &page); err != nil {
			return nil, linearMCPError(err)
		}
		if page.Projects == nil {
			return nil, errLinearMCPRequest
		}
		for _, project := range page.Projects {
			if project.ID != "" && project.Name != "" {
				results = append(results, project)
			}
		}
		next := page.next()
		if next == "" {
			if page.HasNextPage || page.HasMore {
				return nil, errLinearMCPRequest
			}
			return results, nil
		}
		if args["cursor"] == next {
			return nil, errLinearMCPRequest
		}
		args["cursor"] = next
	}
}

type linearMCPIssue struct {
	ID          string          `json:"id"`
	UUID        string          `json:"uuid"`
	Identifier  string          `json:"identifier"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	URL         string          `json:"url"`
	Priority    json.RawMessage `json:"priority"`
	UpdatedAt   time.Time       `json:"updatedAt"`
	Status      string          `json:"status"`
	StatusType  string          `json:"statusType"`
	State       struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
}

func (row linearMCPIssue) issue(filteredState string) LinearIssue {
	issue := LinearIssue{
		ID: row.ID, Identifier: row.Identifier, Title: row.Title, Description: row.Description,
		URL: row.URL, Priority: linearMCPPriority(row.Priority), UpdatedAt: row.UpdatedAt,
	}
	// Linear MCP's id is the readable key. Its uuid matches the GraphQL id
	// used by existing imports and remains stable if the key changes.
	if row.UUID != "" {
		issue.ID = row.UUID
		if issue.Identifier == "" {
			issue.Identifier = row.ID
		}
	}
	issue.State = row.State
	if issue.State.Name == "" {
		issue.State.Name = row.Status
	}
	if issue.State.Type == "" {
		issue.State.Type = row.StatusType
	}
	if issue.State.Type == "" {
		issue.State.Type = filteredState
	}
	if issue.State.Type == "" {
		switch strings.ToLower(strings.TrimSpace(issue.State.Name)) {
		case "triage":
			issue.State.Type = "triage"
		case "backlog":
			issue.State.Type = "backlog"
		case "in progress", "in review", "started":
			issue.State.Type = "started"
		case "done", "completed":
			issue.State.Type = "completed"
		case "canceled", "cancelled":
			issue.State.Type = "canceled"
		default:
			issue.State.Type = "unstarted"
		}
	}
	return issue
}

func linearMCPPriority(raw json.RawMessage) int {
	var rank int
	if json.Unmarshal(raw, &rank) == nil {
		return rank
	}
	var named struct {
		Value int `json:"value"`
	}
	if json.Unmarshal(raw, &named) == nil {
		return named.Value
	}
	var label string
	if json.Unmarshal(raw, &label) == nil {
		if rank, err := strconv.Atoi(label); err == nil {
			return rank
		}
		return map[string]int{"urgent": 1, "high": 2, "medium": 3, "low": 4}[strings.ToLower(label)]
	}
	return 0
}

func (l *LinearMCPOAuth) Issues(ctx context.Context, token, projectID string, filter LinearIssueFilter, preview bool) ([]LinearIssue, bool, error) {
	session, err := l.open(ctx, token)
	if err != nil {
		return nil, false, linearMCPError(err)
	}
	defer session.Close(ctx)
	args := map[string]any{"project": projectID, "limit": 50}
	if filter.State != "" {
		args["state"] = filter.State
	}
	query := strings.ToLower(filter.Query)
	issues := make([]LinearIssue, 0)
	for {
		var page linearMCPPage[linearMCPIssue]
		if err := session.Call(ctx, "list_issues", args, &page); err != nil {
			return nil, false, linearMCPError(err)
		}
		if page.Issues == nil {
			return nil, false, errLinearMCPRequest
		}
		for _, row := range page.Issues {
			issue := row.issue(filter.State)
			if filter.State != "" && issue.State.Type != filter.State ||
				filter.Priority != nil && issue.Priority != *filter.Priority ||
				query != "" && !strings.Contains(strings.ToLower(issue.Title), query) && !strings.Contains(strings.ToLower(issue.Description), query) {
				continue
			}
			if preview && len(issues) == 20 {
				return issues, true, nil
			}
			issues = append(issues, issue)
		}
		next := page.next()
		if next == "" {
			if page.HasNextPage || page.HasMore {
				return nil, false, errLinearMCPRequest
			}
			return issues, false, nil
		}
		if args["cursor"] == next {
			return nil, false, errLinearMCPRequest
		}
		args["cursor"] = next
	}
}
