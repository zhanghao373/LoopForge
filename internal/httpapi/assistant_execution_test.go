package httpapi

import (
	"agentflow/internal/domain"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestConversationDraftMachineSelectionAndFirstTurnBinding(t *testing.T) {
	h, user := workbenchFixture(t)
	for _, runner := range []domain.Runner{
		{ID: "mine", OwnerUserID: user.ID, RunnerVersion: domain.RunnerVersionConversationCLI},
		{ID: "other", OwnerUserID: user.ID + 1, RunnerVersion: domain.RunnerVersionConversationCLI},
	} {
		if err := h.repo.PutRunner(runner); err != nil {
			t.Fatal(err)
		}
	}
	put := func(target *domain.AssistantExecutionTarget, want int) {
		draft, ok := h.repo.AssistantSession(user.ID, "machine-draft")
		if !ok {
			draft = domain.AssistantSession{ID: "machine-draft", ProjectID: "workbench"}
		}
		draft.ExecutionTarget = target
		body, _ := json.Marshal(draft)
		c, rec := workbenchContext(user, "PUT", "/api/assistant/sessions/machine-draft", bytes.NewBuffer(body))
		c.Params = gin.Params{{Key: "id", Value: "machine-draft"}}
		h.session(c)
		if rec.Code != want {
			t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
		}
	}
	cloud := &domain.AssistantExecutionTarget{Pool: domain.ExecutionModeCloud}
	put(cloud, 200)
	put(&domain.AssistantExecutionTarget{Pool: domain.ExecutionModeRunner, RunnerID: "other"}, 400)
	put(&domain.AssistantExecutionTarget{Pool: domain.ExecutionModeRunner, RunnerID: "mine"}, 200)
	put(cloud, 200)
	old, _ := h.repo.AssistantSession(user.ID, "machine-draft")
	if old.ExecutionLocked() {
		t.Fatal("selection locked an empty conversation")
	}
	entered, finished := make(chan struct{}), make(chan struct{})
	h.generate = func(ctx context.Context, _ string, _ domain.User) (string, error) {
		close(entered)
		defer close(finished)
		<-ctx.Done()
		return "", ctx.Err()
	}
	body, _ := json.Marshal(assistantRequest{SessionID: old.ID, ProjectID: old.ProjectID, ExecutionTarget: &domain.AssistantExecutionTarget{Pool: domain.ExecutionModeRunner, RunnerID: "mine"}, Messages: []assistantMessage{{Role: "user", Content: "hello"}}})
	c, rec := workbenchContext(user, "POST", "/api/assistant/generations", bytes.NewBuffer(body))
	h.startGeneration(c)
	if rec.Code != 202 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	g, _ := h.repo.AssistantGeneration(user.ID, old.ID)
	t.Cleanup(func() { _ = h.repo.CancelAssistantGeneration(user.ID, old.ID, g.ID) })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("generation did not start")
	}
	saved, _ := h.repo.AssistantSession(user.ID, old.ID)
	if !saved.ExecutionLocked() || saved.Execution().RunnerID != "mine" {
		t.Fatal("first turn did not bind the final draft choice")
	}
	_ = h.repo.CancelAssistantGeneration(user.ID, old.ID, g.ID)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("generation did not stop")
	}
	put(cloud, 409)
}

func TestDraftCloudAllocationIsServerOwned(t *testing.T) {
	old := domain.AssistantSession{ID: "draft", ExecutionTarget: &domain.AssistantExecutionTarget{Pool: domain.ExecutionModeCloud, RunnerID: "allocated"}}
	for _, requested := range []*domain.AssistantExecutionTarget{nil, old.ExecutionTarget} {
		if got := draftExecutionTarget(old, requested); got.Pool != domain.ExecutionModeCloud || got.RunnerID != "" {
			t.Fatalf("saved allocation reused as client input: %+v", got)
		}
	}
	forged := &domain.AssistantExecutionTarget{Pool: domain.ExecutionModeCloud, RunnerID: "forged"}
	if draftExecutionTarget(old, forged).RunnerID != "forged" {
		t.Fatal("forged cloud runner silently accepted")
	}
}

func TestConversationUsesBoundRunnerCredentialPreference(t *testing.T) {
	h, original := workbenchFixture(t)
	user := domain.User{LoginName: "local-login-owner"}
	if err := h.repo.PutUser(user); err != nil {
		t.Fatal(err)
	}
	user, _ = h.repo.UserByLoginName(user.LoginName)
	if err := h.repo.PutProject(domain.Project{ID: "workbench", Name: "Workbench", OwnerID: user.ID}); err != nil {
		t.Fatal(err)
	}
	h.generate = func(_ context.Context, _ string, _ domain.User) (string, error) {
		return `{"message":"local reply","drafts":[]}`, nil
	}
	h.titleSummary = func(context.Context, domain.User, string) (string, error) { return "", nil }

	runner := domain.Runner{ID: "local-login", OwnerUserID: user.ID, RunnerVersion: domain.RunnerVersionLocalConversationAuth}
	target := &domain.AssistantExecutionTarget{Pool: domain.ExecutionModeRunner, RunnerID: runner.ID}
	for _, provider := range []string{"codex", "claude-code", "cursor"} {
		t.Run(provider, func(t *testing.T) {
			runner.Runtimes = []domain.RunnerRuntime{{Provider: provider, AuthMode: domain.RuntimeAuthLocal}}
			if err := h.repo.PutRunner(runner); err != nil {
				t.Fatal(err)
			}
			send := func(actor domain.User, id string, target *domain.AssistantExecutionTarget, status int, detail string) {
				t.Helper()
				req := assistantRequest{ProjectID: "workbench", SessionID: id, Executor: provider, Model: provider + ":default", ExecutionTarget: target, Messages: []assistantMessage{{Role: "user", Content: "hello"}}}
				body, _ := json.Marshal(req)
				c, rec := workbenchContext(actor, "POST", "/api/assistant/generations", bytes.NewBuffer(body))
				h.startGeneration(c)
				if rec.Code != status || !strings.Contains(rec.Body.String(), detail) {
					t.Fatalf("start: %d %s, want %d with %q", rec.Code, rec.Body.String(), status, detail)
				}
			}
			id := "local-" + provider
			send(user, id, target, 202, runner.ID)
			deadline := time.Now().Add(5 * time.Second)
			for {
				g, _ := h.repo.AssistantGeneration(user.ID, id)
				if g.Status != "running" {
					if g.Status != "completed" {
						t.Fatalf("generation: %+v", g)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("generation did not complete")
				}
				time.Sleep(10 * time.Millisecond)
			}
			saved, _ := h.repo.AssistantSession(user.ID, id)
			if saved.Model != provider+":default" || saved.Execution().RunnerID != runner.ID {
				t.Fatalf("lost model or Runner binding: %+v", saved)
			}
			if local, err := h.assistantLocalLogin(original.ID, target, saved.Model); err != nil || local {
				t.Fatalf("another actor could use local login: %v %v", local, err)
			}
			send(user, "cloud-"+provider, nil, 400, "API Key")

			runner.RunnerVersion = domain.RunnerVersionConversationCLI
			if err := h.repo.PutRunner(runner); err != nil {
				t.Fatal(err)
			}
			send(user, "outdated-"+provider, target, 400, domain.RunnerVersionLocalConversationAuth)
			runner.RunnerVersion = domain.RunnerVersionLocalConversationAuth
			runner.Runtimes[0].AuthMode = domain.RuntimeAuthPersonal
			if err := h.repo.PutRunner(runner); err != nil {
				t.Fatal(err)
			}
			send(user, "personal-"+provider, target, 400, "API Key")
		})
	}
}
