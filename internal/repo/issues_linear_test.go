package repo

import (
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestImportLinearIssuesPreservesLocalWork(t *testing.T) {
	store, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	updatedAt := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	incoming := domain.Issue{
		Title: "Original", Status: "todo", Priority: "medium", Kind: "requirement", AutomationState: "ignored",
		ExternalRef: &domain.ExternalRef{Source: "linear", ExternalID: "remote-1", ExternalIdentifier: "ENG-1", ExternalUpdatedAt: &updatedAt},
	}
	importOne := func(want LinearImportResult) {
		t.Helper()
		if got, err := store.ImportLinearIssues("platform", []domain.Issue{incoming}); err != nil || got != want {
			t.Fatalf("import got %+v, error %v, want %+v", got, err, want)
		}
	}

	if _, err := store.ImportLinearIssues("platform", []domain.Issue{incoming, {Title: "Invalid"}}); err == nil {
		t.Fatal("expected invalid batch to fail")
	}
	importOne(LinearImportResult{Created: 1})
	var original domain.Issue
	for _, issue := range store.Issues() {
		if issue.Source == "linear" {
			original = issue
		}
	}
	if original.ID == "" || original.Status != "todo" || original.AutomationState != "ignored" {
		t.Fatalf("unexpected new Linear issue: %+v", original)
	}

	localSOPs := []domain.IssueSOPBinding{{WorkflowID: "local-sop"}}
	local, err := store.PatchIssue(original.ID, func(issue *domain.Issue) error {
		issue.Status = "in_progress"
		issue.AssigneeUserID = 1
		issue.RepositoryIDs = []string{"local-repo"}
		issue.SOPBindings = localSOPs
		issue.Clarification.Summary = "local clarification"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	importOne(LinearImportResult{Unchanged: 1})
	unchanged, _ := store.Issue(original.ID)
	if !unchanged.UpdatedAt.Equal(local.UpdatedAt) {
		t.Fatal("unchanged import rewrote the issue")
	}

	oldRef := *incoming.ExternalRef
	newer := updatedAt.Add(time.Hour)
	incoming.Title = "Revised"
	incoming.Description = "Revised description"
	incoming.Status = "done"
	incoming.Priority = "urgent"
	incoming.ExternalRef.ExternalState = "Done"
	incoming.ExternalRef.ExternalUpdatedAt = &newer
	importOne(LinearImportResult{Updated: 1})
	incoming.Title = "Stale"
	incoming.ExternalRef = &oldRef
	importOne(LinearImportResult{Unchanged: 1})

	stored, _ := store.Issue(original.ID)
	if stored.Title != "Revised" || stored.Description != "Revised description" || stored.Priority != "urgent" || stored.ExternalRef.ExternalState != "Done" {
		t.Fatalf("Linear fields not updated: %+v", stored)
	}
	if stored.Status != local.Status || stored.AssigneeUserID != local.AssigneeUserID ||
		len(stored.RepositoryIDs) != 1 || stored.RepositoryIDs[0] != "local-repo" ||
		len(stored.SOPBindings) != 1 || stored.SOPBindings[0].WorkflowID != "local-sop" ||
		stored.Clarification.Summary != local.Clarification.Summary || stored.Number != original.Number ||
		!stored.CreatedAt.Equal(original.CreatedAt) {
		t.Fatalf("local issue fields changed: %+v", stored)
	}

	if err := store.PutProject(domain.Project{ID: "second", Name: "Second"}); err != nil {
		t.Fatal(err)
	}
	if result, err := store.ImportLinearIssues("second", []domain.Issue{incoming}); err != nil || result.Created != 1 {
		t.Fatalf("second board should receive an independent copy: %+v, %v", result, err)
	}
}
