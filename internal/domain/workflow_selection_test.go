package domain

import (
	"reflect"
	"testing"
)

func TestSelectNodesPreservesDependenciesAndRejectsEmptySelection(t *testing.T) {
	workflow := WorkflowDefinition{
		Tags: []string{SelectableWorkflowNodesTag},
		Nodes: []WorkflowNode{
			{ID: "plan"}, {ID: "backend"}, {ID: "frontend"},
			{ID: "review", RejectTo: "backend", OnFailure: "reject"},
		},
		Edges: []WorkflowEdge{
			{ID: "e1", Source: "plan", Target: "backend", When: "success"},
			{ID: "e2", Source: "plan", Target: "frontend", When: "success"},
			{ID: "e3", Source: "backend", Target: "review", When: "success"},
			{ID: "e4", Source: "frontend", Target: "review", When: "success"},
		},
		Requirements: []WorkflowRequirement{{Key: "source-code", NodeID: "backend"}},
	}

	selected, err := workflow.SelectNodes([]string{"backend"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := selected.DisabledNodeIDs, []string{"backend"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("disabled IDs = %v, want %v", got, want)
	}
	review := selected.Nodes[2]
	if review.ID != "review" || review.RejectTo != "" || review.OnFailure != "fail" || selected.RequiresSourceCode() {
		t.Fatalf("selected review and requirements = %+v, %+v", review, selected.Requirements)
	}
	wantEdges := []WorkflowEdge{workflow.Edges[1], workflow.Edges[3],
		{ID: "selection-1", Source: "plan", Target: "review", When: "success"}}
	if !reflect.DeepEqual(selected.Edges, wantEdges) {
		t.Fatalf("selected dependencies = %+v, want %+v", selected.Edges, wantEdges)
	}
	only, err := workflow.SelectNodes([]string{"plan", "backend", "review"})
	if err != nil || len(only.Nodes) != 1 || only.Nodes[0].ID != "frontend" || len(only.Edges) != 0 {
		t.Fatalf("single-node selection = %+v, %v", only, err)
	}
	if len(workflow.Nodes) != 4 || workflow.Nodes[3].RejectTo != "backend" {
		t.Fatal("selection mutated the blueprint")
	}
	if _, err := workflow.SelectNodes([]string{"plan", "backend", "frontend", "review"}); err == nil {
		t.Fatal("expected an empty selection to be rejected")
	}
	if _, err := workflow.SelectNodes([]string{"missing"}); err == nil {
		t.Fatal("expected a stale node selection to be rejected")
	}
}
