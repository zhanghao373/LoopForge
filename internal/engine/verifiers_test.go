package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"agentflow/internal/domain"
)

func TestVerifierSnapshotsAndRunsScriptBeforeReview(t *testing.T) {
	wf := domain.WorkflowDefinition{
		ID: "verifier-snapshot", Name: "Verifier snapshot", ProjectID: "p", SandboxProfileID: "local",
		Nodes: []domain.WorkflowNode{
			{ID: "approve", Name: "Confirm", Type: "approval"},
			scriptNode("test", "printf 'approved evidence' > evidence.txt"),
			scriptNode("deliver", "true"),
		},
		Edges: []domain.WorkflowEdge{edge("approve", "test"), edge("test", "deliver")},
	}
	e, store := parallelTestEngine(t, wf)
	verifier, err := store.PutVerifier(domain.VerifierDefinition{
		ID: "test-evidence", ProjectID: "p", Name: "Evidence",
		Script: "test -s evidence.txt\nprintf 'script confirmed'", ExecutionScope: "run", TimeoutSec: 5,
		Review: &domain.VerifierReview{ExecutorID: "codex", Model: "review-model", Instructions: "Check the expected evidence", ArtifactNames: []string{"evidence"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wf, _ = store.Workflow(wf.ID)
	wf.Nodes[1].ArtifactOutputs = []domain.ArtifactContract{{Name: "evidence", Path: "evidence.txt", Required: true}}
	wf.Nodes[1].Verification = &domain.NodeVerification{Verifiers: []domain.VerifierReference{{VerifierID: verifier.ID}}}
	if err := store.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	type call struct {
		review   domain.VerifierReview
		evidence VerifierEvidence
	}
	calls := make(chan call, 3)
	var seen atomic.Int32
	e.ReviewVerifier = func(_ context.Context, _ domain.Run, review domain.VerifierReview, evidence VerifierEvidence) (VerifierVerdict, error) {
		calls <- call{review, evidence}
		return VerifierVerdict{Passed: seen.Add(1) == 1, Reason: "review conclusion"}, nil
	}
	first, err := e.Start(wf.ID, "Check verification")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, store, first.ID, "waiting")
	verifier.Script = "exit 26"
	failing, err := store.PutVerifier(verifier)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Approve(first.ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, store, first.ID, "succeeded")
	actual := <-calls
	if actual.review.Model != "review-model" || !strings.Contains(actual.evidence.ScriptOutput, "script confirmed") ||
		actual.evidence.Artifacts["evidence"] != "approved evidence" {
		t.Fatalf("unexpected model selection/evidence: %+v", actual)
	}
	frozen, _ := store.Run(first.ID)
	if frozen.WorkflowSnapshot.VerifierSnapshots[verifier.ID].Version != 1 {
		t.Fatal("existing run did not retain the version it started with")
	}
	observed := false
	for _, artifact := range frozen.Artifacts {
		observed = observed || strings.HasPrefix(artifact.Name, "verifier-") && strings.Contains(artifact.Content, "review conclusion")
	}
	if !observed {
		t.Fatal("model verdict was not retained with the run")
	}
	run, err := e.Start(wf.ID, "Check gate failure")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, store, run.ID, "waiting")
	if err := e.Approve(run.ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, store, run.ID, "failed")
	blocked, _ := store.Run(run.ID)
	if blocked.Nodes["deliver"].Status != "pending" || blocked.WorkflowSnapshot.VerifierSnapshots[verifier.ID].Version != failing.Version {
		t.Fatalf("new gate version did not block downstream: %+v", blocked.Nodes["deliver"])
	}
	if seen.Load() != 1 {
		t.Fatal("script failure invoked model review")
	}
	failing.Script = "true"
	if _, err := store.PutVerifier(failing); err != nil {
		t.Fatal(err)
	}
	rejected, err := e.Start(wf.ID, "Check rejection by review")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, store, rejected.ID, "waiting")
	if err := e.Approve(rejected.ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, store, rejected.ID, "failed")
	rejected, _ = store.Run(rejected.ID)
	if seen.Load() != 2 || !strings.Contains(rejected.Nodes["test"].Error, "review conclusion") || rejected.Nodes["deliver"].Status != "pending" {
		t.Fatalf("model rejection did not block downstream: %+v", rejected.Nodes["test"])
	}
}

func TestVerifierConfigurationOverridesLegacyRoleGate(t *testing.T) {
	wf := domain.WorkflowDefinition{
		ID: "verifier-legacy", Name: "Legacy role gate", ProjectID: "p", SandboxProfileID: "local",
		Nodes: []domain.WorkflowNode{{ID: "work", Name: "Work", Type: "agent", AgentID: "legacy-role", MaxAttempts: 1}},
	}
	e, store := parallelTestEngine(t, wf)
	putGateTestAgent(t, store, "legacy-role", []domain.VerificationGate{{
		ID: "legacy", Name: "Legacy check", Mode: "command",
		Commands: []string{"exit 31"}, ExecutionScope: "run", Required: true,
	}})
	inherited, err := e.Start(wf.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, store, inherited.ID, "failed")
	state, _ := store.Run(inherited.ID)
	if !strings.Contains(state.Nodes["work"].Error, "Legacy check") {
		t.Fatalf("legacy gate did not execute: %+v", state.Nodes["work"])
	}

	wf, _ = store.Workflow(wf.ID)
	wf.Nodes[0].Verification = &domain.NodeVerification{Verifiers: []domain.VerifierReference{}}
	if err := store.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	disabled, err := e.Start(wf.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, store, disabled.ID, "succeeded")

	ref, err := store.PutVerifier(domain.VerifierDefinition{
		ID: "selected-check", ProjectID: "p", Name: "Selected check", Script: "true", ExecutionScope: "run", TimeoutSec: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	wf, _ = store.Workflow(wf.ID)
	wf.Nodes[0].Verification.Verifiers = []domain.VerifierReference{{VerifierID: ref.ID}}
	if err := store.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	selected, err := e.Start(wf.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, store, selected.ID, "succeeded")
	got, _ := store.Run(selected.ID)
	if got.WorkflowSnapshot.VerifierSnapshots[ref.ID].Version != 1 {
		t.Fatal("selected verifier was not resolved")
	}
}

func TestVerifierRoundRunsScriptsConcurrentlyAndAggregatesFailures(t *testing.T) {
	wf := domain.WorkflowDefinition{
		ID: "verifier-concurrent", Name: "Parallel verification", ProjectID: "p", SandboxProfileID: "local",
		Nodes: []domain.WorkflowNode{scriptNode("verify", "true"), scriptNode("deliver", "true")},
		Edges: []domain.WorkflowEdge{edge("verify", "deliver")},
	}
	e, store := parallelTestEngine(t, wf)
	refs := make([]domain.VerifierReference, 0, 2)
	for _, check := range []struct {
		id, script, action string
	}{
		{"left", "touch left-started; while [ ! -f right-started ]; do sleep .05; done; printf left-finding; exit 2", "fail"},
		{"right", "touch right-started; while [ ! -f left-started ]; do sleep .05; done; sleep .15; printf right-finding; exit 3", "human"},
	} {
		v, err := store.PutVerifier(domain.VerifierDefinition{
			ID: check.id, Name: check.id, ProjectID: "p", Script: check.script, ExecutionScope: "run", TimeoutSec: 5,
		})
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, domain.VerifierReference{VerifierID: v.ID, OnFailure: check.action})
	}
	wf, _ = store.Workflow(wf.ID)
	wf.Nodes[0].Verification = &domain.NodeVerification{Verifiers: refs}
	if err := store.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	run, err := e.Start(wf.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, store, run.ID, "waiting")
	got, _ := store.Run(run.ID)
	if got.Nodes["verify"].Status != "waiting_approval" || got.Nodes["deliver"].Status != "pending" {
		t.Fatalf("expected an aggregated human decision before delivery: %+v", got.Nodes)
	}
	summary := got.Nodes["verify"].Error
	if !strings.Contains(summary, "left-finding") || !strings.Contains(summary, "right-finding") {
		t.Fatalf("did not retain both concurrent findings: %s", summary)
	}
}

func TestVerifierSelfCorrectionRepeatsAllModelOnlyChecks(t *testing.T) {
	wf := domain.WorkflowDefinition{
		ID: "verifier-recheck", Name: "Verification retry", ProjectID: "p", SandboxProfileID: "local",
		Nodes: []domain.WorkflowNode{
			{ID: "work", Name: "Work", Type: "agent", AgentID: "repair-role", MaxAttempts: 1},
			scriptNode("deliver", "true"),
		},
		Edges: []domain.WorkflowEdge{edge("work", "deliver")},
	}
	e, store := parallelTestEngine(t, wf)
	if err := store.PutAgent(domain.AgentDefinition{
		ID: "repair-role", Name: "Repair role", ExecutorID: "mock",
		Loop: domain.AgentLoopPolicy{Enabled: true, MaxIterations: 2},
	}); err != nil {
		t.Fatal(err)
	}
	var refs []domain.VerifierReference
	for _, id := range []string{"always-pass", "needs-repair"} {
		verifier, err := store.PutVerifier(domain.VerifierDefinition{
			ID: id, Name: id, ProjectID: "p", Review: &domain.VerifierReview{
				ExecutorID: "codex", Model: id, Instructions: "Review the task",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, domain.VerifierReference{VerifierID: verifier.ID, OnFailure: "loop"})
	}
	wf, _ = store.Workflow(wf.ID)
	wf.Nodes[0].Verification = &domain.NodeVerification{Verifiers: refs}
	if err := store.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	items, err := e.SOPLaunchCLIs(wf)
	if err != nil || len(items) != 4 || !items[1].Review || !items[2].Review || items[3].Node != "deliver" {
		t.Fatalf("model-only checks must require a reviewer without terminal: %+v, %v", items, err)
	}
	var first, second atomic.Int32
	e.ReviewVerifier = func(_ context.Context, _ domain.Run, review domain.VerifierReview, evidence VerifierEvidence) (VerifierVerdict, error) {
		if evidence.ScriptOutput != "" || evidence.Task != "Validate the task" {
			t.Errorf("unexpected model-only evidence: %+v", evidence)
		}
		if review.Model == "always-pass" {
			first.Add(1)
			return VerifierVerdict{Passed: true, Reason: "passed"}, nil
		}
		return VerifierVerdict{Passed: second.Add(1) > 1, Reason: "needs a repair round"}, nil
	}
	run, err := e.Start(wf.ID, "Validate the task")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, store, run.ID, "succeeded")
	if first.Load() != 2 || second.Load() != 2 {
		t.Fatalf("expected both verifiers in both rounds: %d, %d", first.Load(), second.Load())
	}
	got, _ := store.Run(run.ID)
	for _, name := range []string{"verifier-always-pass-1", "verifier-needs-repair-1", "verifier-always-pass-2", "verifier-needs-repair-2"} {
		found := false
		for _, artifact := range got.Artifacts {
			found = found || artifact.Name == name
		}
		if !found {
			t.Errorf("missing model verdict for %s", name)
		}
	}
}
