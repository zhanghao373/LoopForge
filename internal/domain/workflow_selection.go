package domain

import (
	"fmt"
	"slices"
)

const SelectableWorkflowNodesTag = "selectable-nodes"

func (w WorkflowDefinition) HasSelectableNodes() bool {
	return slices.Contains(w.Tags, SelectableWorkflowNodesTag)
}

// SelectNodes projects the published graph onto the nodes chosen for this run.
// A disabled node's consumers still wait for its nearest enabled predecessors.
func (w WorkflowDefinition) SelectNodes(disabledIDs []string) (WorkflowDefinition, error) {
	if len(disabledIDs) == 0 {
		return w, nil
	}
	if !w.HasSelectableNodes() {
		return w, fmt.Errorf("此 SOP 不支持跳过节点")
	}
	w, err := AttachPreviewBuild(w)
	if err != nil {
		return w, err
	}
	nodes := make(map[string]WorkflowNode, len(w.Nodes))
	for _, node := range w.Nodes {
		nodes[node.ID] = node
	}
	disabled := make(map[string]bool, len(disabledIDs))
	for _, id := range disabledIDs {
		if _, ok := nodes[id]; !ok {
			return w, fmt.Errorf("SOP 节点 %q 已变更，请重新选择", id)
		}
		disabled[id] = true
	}
	if len(disabled) == len(nodes) {
		return w, fmt.Errorf("至少选择一个 SOP 节点")
	}

	selected := w
	selected.Nodes, selected.Edges, selected.DisabledNodeIDs = nil, nil, nil
	selected.Requirements = nil
	if disabled[PreviewBuildNodeID] {
		selected.Preview = nil
	}
	for _, node := range w.Nodes {
		if disabled[node.ID] {
			selected.DisabledNodeIDs = append(selected.DisabledNodeIDs, node.ID)
			continue
		}
		// A skipped rework target cannot be reset or reintroduced by a rejection.
		if disabled[node.RejectTo] {
			node.RejectTo, node.OnFailure = "", "fail"
			if node.Verification != nil {
				refs := slices.Clone(node.Verification.Verifiers)
				for i := range refs {
					if refs[i].OnFailure == "reject" {
						refs[i].OnFailure = "fail"
					}
				}
				node.Verification = &NodeVerification{Verifiers: refs}
			}
		}
		selected.Nodes = append(selected.Nodes, node)
	}
	for _, requirement := range w.Requirements {
		if !disabled[requirement.NodeID] {
			selected.Requirements = append(selected.Requirements, requirement)
		}
	}

	type pair struct{ source, target string }
	outgoing := make(map[string][]WorkflowEdge)
	connected := make(map[pair]bool)
	edgeIDs := make(map[string]bool)
	for _, edge := range w.Edges {
		outgoing[edge.Source] = append(outgoing[edge.Source], edge)
		edgeIDs[edge.ID] = true
		if !disabled[edge.Source] && !disabled[edge.Target] {
			connected[pair{edge.Source, edge.Target}] = true
			selected.Edges = append(selected.Edges, edge)
		}
	}
	nextEdge := 0
	for _, node := range selected.Nodes {
		visited := make(map[string]bool)
		var bypass func(string)
		bypass = func(id string) {
			if visited[id] {
				return
			}
			visited[id] = true
			for _, edge := range outgoing[id] {
				if disabled[edge.Target] {
					bypass(edge.Target)
					continue
				}
				connection := pair{node.ID, edge.Target}
				if connected[connection] {
					continue
				}
				connected[connection] = true
				for {
					nextEdge++
					edgeID := fmt.Sprintf("selection-%d", nextEdge)
					if !edgeIDs[edgeID] {
						selected.Edges = append(selected.Edges, WorkflowEdge{
							ID: edgeID, Source: node.ID, Target: edge.Target, When: "success",
						})
						break
					}
				}
			}
		}
		for _, edge := range outgoing[node.ID] {
			if disabled[edge.Target] {
				bypass(edge.Target)
			}
		}
	}
	return selected, nil
}

func (w WorkflowDefinition) RequiresSourceCode() bool {
	return slices.ContainsFunc(w.Requirements, func(requirement WorkflowRequirement) bool {
		return requirement.Key == "source-code"
	})
}
