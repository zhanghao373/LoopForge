package engine

import (
	"fmt"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

type SOPLaunchCLI struct {
	Node     string `json:"node"`
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Review   bool   `json:"review,omitempty"`
}

func (e *Engine) SOPLaunchCLIs(workflow domain.WorkflowDefinition) ([]SOPLaunchCLI, error) {
	items := []SOPLaunchCLI{}
	seen := map[string]bool{}
	var collect func(domain.WorkflowDefinition) error
	collect = func(wf domain.WorkflowDefinition) error {
		if seen[wf.ID] {
			return nil
		}
		seen[wf.ID] = true
		snapshots, err := e.repo.WorkflowVerifiers(wf, nil)
		if err != nil {
			return err
		}
		for _, node := range wf.Nodes {
			switch node.Type {
			case "sop":
				child, ok := e.repo.Workflow(node.SOPWorkflowID)
				if !ok {
					return fmt.Errorf("节点「%s」的子流程不存在", node.Name)
				}
				if err := collect(child); err != nil {
					return err
				}
			case "approval":
			case "script", "command":
				items = append(items, SOPLaunchCLI{Node: node.Name, Provider: "terminal"})
			default:
				role, found := workflowAgent(e.repo, wf, node.AgentID)
				if !found {
					return fmt.Errorf("节点「%s」的角色不可用", node.Name)
				}
				executor, err := resolveNodeExecutor(e.repo, node, role)
				if err != nil {
					return err
				}
				items = append(items, SOPLaunchCLI{Node: node.Name, Provider: executor.Kind, Name: executor.Name})
			}
			if node.Verification != nil {
				for _, ref := range node.Verification.Verifiers {
					v := snapshots[ref.VerifierID]
					if v.HasScript() {
						items = append(items, SOPLaunchCLI{Node: node.Name, Provider: "terminal"})
					}
					if v.Review != nil {
						ex, _ := repo.SOPExecutor(v.Review.ExecutorID)
						items = append(items, SOPLaunchCLI{Node: node.Name + " · " + v.Name, Provider: ex.Kind, Name: ex.Name, Review: true})
					}
				}
			}
		}
		return nil
	}
	if err := collect(workflow); err != nil {
		return nil, err
	}
	return items, nil
}

func (e *Engine) SOPLaunchCloudRunners(projectID string) []domain.Runner {
	e.mu.Lock()
	pool := e.cloudPool
	e.mu.Unlock()
	if !pool.serves(projectID) {
		return nil
	}
	return e.eligiblePoolRunners(pool.Label)
}
