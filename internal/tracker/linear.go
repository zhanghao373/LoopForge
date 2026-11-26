package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const linearEndpoint = "https://api.linear.app/graphql"

var (
	ErrLinearKey     = errors.New("Linear API Key 无效，或缺少读取权限")
	ErrLinearRate    = errors.New("Linear 请求达到频率限制，请稍后重试")
	ErrLinearProject = errors.New("找不到该 Linear 项目，或 API Key 无权访问")
	errLinearRequest = errors.New("读取 Linear 失败，请检查网络连接或 API Key 的项目权限")
)

type LinearClient struct {
	http *http.Client
}

func NewLinearClient() *LinearClient {
	return &LinearClient{http: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type LinearProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type LinearIssue struct {
	ID          string    `json:"id"`
	Identifier  string    `json:"identifier"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	URL         string    `json:"url"`
	Priority    int       `json:"priority"`
	UpdatedAt   time.Time `json:"updatedAt"`
	State       struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
}

type LinearIssueFilter struct {
	State    string `json:"state"`
	Priority *int   `json:"priority"`
	Query    string `json:"query"`
}

type linearPage[T any] struct {
	Nodes    []T `json:"nodes"`
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

const linearProjectsQuery = `query ImportProjects($after: String) {
  projects(first: 50, after: $after) {
    nodes { id name }
    pageInfo { hasNextPage endCursor }
  }
}`

const linearIssuesQuery = `query ImportIssues($id: String!, $filter: IssueFilter!, $first: Int!, $after: String) {
  project(id: $id) {
    issues(first: $first, after: $after, filter: $filter) {
      nodes { id identifier title description url priority updatedAt state { name type } }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

func (c *LinearClient) Projects(ctx context.Context, key string) ([]LinearProject, error) {
	vars := map[string]any{}
	projects := make([]LinearProject, 0)
	for {
		var data struct {
			Projects linearPage[LinearProject]
		}
		if err := c.query(ctx, key, linearProjectsQuery, vars, &data); err != nil {
			return nil, err
		}
		projects = append(projects, data.Projects.Nodes...)
		if !data.Projects.PageInfo.HasNextPage {
			return projects, nil
		}
		if err := advanceLinearCursor(vars, data.Projects.PageInfo.EndCursor); err != nil {
			return nil, err
		}
	}
}

func (c *LinearClient) Issues(ctx context.Context, key, projectID string, selected LinearIssueFilter, preview bool) ([]LinearIssue, bool, error) {
	filter := map[string]any{}
	if selected.State != "" {
		filter["state"] = map[string]any{"type": map[string]string{"eq": selected.State}}
	}
	if selected.Priority != nil {
		filter["priority"] = map[string]int{"eq": *selected.Priority}
	}
	if selected.Query != "" {
		match := map[string]string{"containsIgnoreCase": selected.Query}
		filter["or"] = []map[string]any{{"title": match}, {"description": match}}
	}

	vars := map[string]any{"id": projectID, "filter": filter, "first": 50}
	if preview {
		vars["first"] = 20
	}
	issues := make([]LinearIssue, 0)
	for {
		var data struct {
			Project *struct {
				Issues linearPage[LinearIssue]
			}
		}
		if err := c.query(ctx, key, linearIssuesQuery, vars, &data); err != nil {
			return nil, false, err
		}
		if data.Project == nil {
			return nil, false, ErrLinearProject
		}
		page := data.Project.Issues
		issues = append(issues, page.Nodes...)
		if preview || !page.PageInfo.HasNextPage {
			return issues, page.PageInfo.HasNextPage, nil
		}
		if err := advanceLinearCursor(vars, page.PageInfo.EndCursor); err != nil {
			return nil, false, err
		}
	}
}

func advanceLinearCursor(vars map[string]any, cursor string) error {
	if cursor == "" || vars["after"] == cursor {
		return errLinearRequest
	}
	vars["after"] = cursor
	return nil
}

func (c *LinearClient) query(ctx context.Context, key, query string, vars map[string]any, result any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return errLinearRequest
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, linearEndpoint, bytes.NewReader(payload))
	if err != nil {
		return errLinearRequest
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", key)
	res, err := c.http.Do(req)
	if err != nil {
		return errLinearRequest
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrLinearKey
	case http.StatusTooManyRequests:
		return ErrLinearRate
	}
	var response struct {
		Data   json.RawMessage
		Errors []struct {
			Extensions struct {
				Code string
			}
		}
	}
	if json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&response) != nil {
		return errLinearRequest
	}
	if len(response.Errors) > 0 {
		for _, item := range response.Errors {
			switch item.Extensions.Code {
			case "RATELIMITED":
				return ErrLinearRate
			case "UNAUTHENTICATED", "FORBIDDEN", "AUTHENTICATION_ERROR", "AUTHORIZATION_ERROR":
				return ErrLinearKey
			case "ENTITY_NOT_FOUND":
				return ErrLinearProject
			}
		}
		return errLinearRequest
	}
	if res.StatusCode != http.StatusOK || len(response.Data) == 0 || string(response.Data) == "null" || json.Unmarshal(response.Data, result) != nil {
		return errLinearRequest
	}
	return nil
}
