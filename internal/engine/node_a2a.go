package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"agentflow/internal/domain"
	"agentflow/internal/runtime/a2a"
)

func (e *Engine) ConfigureA2A(endpoint, token string) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" && token == "" {
		return nil
	}
	client, err := a2a.NewClient(endpoint, token)
	if err != nil {
		return fmt.Errorf("configure SOP A2A executor: %w", err)
	}
	e.a2aClient = client
	return nil
}

func (e *Engine) A2AEnabled() bool {
	return e.a2aClient != nil
}

func a2aMessageID(runID, nodeID string, attempt int) string {
	key := fmt.Sprintf("%q/%q/%d", runID, nodeID, attempt)
	return fmt.Sprintf("loopforge-%x", sha256.Sum256([]byte(key)))
}

func a2aDocuments(inputs []domain.RunnerInputFile) (map[string]string, error) {
	if len(inputs) > 8 {
		return nil, errors.New("A2A accepts up to eight input artifacts per node")
	}
	documents := make(map[string]string, len(inputs))
	for _, input := range inputs {
		if _, exists := documents[input.Path]; exists {
			return nil, fmt.Errorf("duplicate A2A input artifact %q", input.Path)
		}
		if !utf8.ValidString(input.Content) || strings.ContainsRune(input.Content, 0) {
			return nil, fmt.Errorf("A2A input artifact %q must be a text file", input.Path)
		}
		if utf8.RuneCountInString(input.Content) > 64000 || utf8.RuneCountInString(input.Path) > 128 {
			return nil, fmt.Errorf("A2A input artifact %q exceeds the service limit", input.Path)
		}
		documents[input.Path] = input.Content
	}
	return documents, nil
}

func a2aPrompt(prompt, rejectTo string, contracts []domain.ArtifactContract) string {
	var instructions strings.Builder
	instructions.WriteString("Read supplied input artifacts by their document name. ")
	instructions.WriteString("Return your final text and structured result. ")
	instructions.WriteString("Set data.gateResult to an object with decision and reason. ")
	instructions.WriteString("Use pass, fail, or human, and include actionRequired for human. ")
	if rejectTo != "" {
		instructions.WriteString("Use rework when the configured upstream node needs to redo work. ")
	}
	if len(contracts) > 0 {
		instructions.WriteString("Put text artifacts in data.outputs, keyed by declared artifact name. ")
		for _, contract := range contracts {
			fmt.Fprintf(&instructions, "Artifact %q (required=%t). ", contract.Name, contract.Required)
		}
	}
	return prompt + "\n\n" + instructions.String()
}

type a2aNodeResult struct {
	text  string
	gate  *domain.GateResult
	files map[string]string
}

func a2aNodeOutput(task a2a.Task, contracts []domain.ArtifactContract) (a2aNodeResult, error) {
	if task.Status.State != "TASK_STATE_COMPLETED" {
		return a2aNodeResult{}, fmt.Errorf("A2A task ended in %s", task.Status.State)
	}
	var result a2aNodeResult
	var data json.RawMessage
	for _, artifact := range task.Artifacts {
		if artifact.Name != "result" {
			continue
		}
		for _, part := range artifact.Parts {
			result.text += part.Text
			if len(part.Data) > 0 {
				if data != nil {
					return result, errors.New("A2A task returned multiple structured results")
				}
				data = part.Data
			}
		}
	}
	if data == nil {
		return result, errors.New("A2A task omitted the structured result")
	}
	var payload struct {
		GateResult json.RawMessage   `json:"gateResult"`
		Outputs    map[string]string `json:"outputs"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return result, fmt.Errorf("invalid A2A task result: %w", err)
	}
	gate, err := decodeGateResult(payload.GateResult)
	if err != nil {
		return result, fmt.Errorf("invalid A2A gate result: %w", err)
	}
	result.gate = gate
	result.files = make(map[string]string, len(contracts))
	for _, contract := range contracts {
		content, ok := payload.Outputs[contract.Name]
		if !ok {
			continue
		}
		rel, ok := artifactRelPath("", contract.Path)
		if !ok {
			return result, fmt.Errorf("invalid path for A2A artifact %q", contract.Name)
		}
		result.files[rel] = content
	}
	return result, nil
}

// executeA2ANode submits exactly one task per node attempt. The durable link is
// independent from the LoopForge worker generation, so a replacement can wait
// on the remote task without re-rendering or resubmitting the input.
func (e *Engine) executeA2ANode(
	ctx context.Context, run domain.Run, node domain.WorkflowNode, prompt string,
	inputs []domain.RunnerInputFile, sink func(domain.TelemetryEvent),
) (a2aNodeResult, error) {
	nr := run.Nodes[node.ID]
	var task a2a.Task
	var err error
	if nr.A2ATaskID != "" {
		task, err = e.a2aClient.Get(ctx, nr.A2ATaskID)
	} else {
		documents, inputErr := a2aDocuments(inputs)
		if inputErr != nil {
			return a2aNodeResult{}, inputErr
		}
		if err := ctx.Err(); err != nil {
			return a2aNodeResult{}, err
		}
		// A control request can land during submission. Let the bounded
		// exchange obtain the Task ID so explicit stop can reach the remote.
		submit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		task, err = e.a2aClient.Send(submit, a2a.SendInput{
			MessageID:   a2aMessageID(run.ID, node.ID, nr.Attempt),
			Instruction: a2aPrompt(prompt, node.RejectTo, node.ArtifactOutputs),
			Documents:   documents,
		})
		cancel()
		if err == nil {
			linked := false
			stored, writeErr := e.updateRunAtErr(run.ID, false, func(current *domain.Run) {
				state := current.Nodes[node.ID]
				if shouldCommitNodeOutcome(*current, state, run.Generation, nr.Attempt, false) {
					state.A2ATaskID = task.ID
					linked = true
				}
			})
			err = writeErr
			if err == nil && (!stored || !linked) {
				err = errors.New("node ownership changed before the A2A task was linked")
			}
		}
	}
	if task.ID != "" {
		defer e.stopA2ATaskIfRequested(run.ID, task.ID)
	}
	if err != nil {
		return a2aNodeResult{}, err
	}
	sink(domain.TelemetryEvent{Category: "lifecycle", Name: "a2a.task.attached", Phase: "start",
		Provider: domain.A2AExecutorID, Attributes: map[string]any{"taskId": task.ID}})
	task, err = e.a2aClient.Wait(ctx, task)
	if err != nil {
		return a2aNodeResult{}, err
	}
	return a2aNodeOutput(task, node.ArtifactOutputs)
}

func (e *Engine) stopA2ATaskIfRequested(runID, taskID string) {
	current, ok := e.repo.RunState(runID)
	if !ok || (!current.CancelRequested && !current.PauseRequested && current.Status != "cancelled") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := e.a2aClient.Cancel(ctx, taskID); err != nil && !a2a.IsTaskNotCancelable(err) {
		e.AppendRunEvent(runID, "a2a.cancel.failed", "A2A task cancellation was not acknowledged")
	}
}

func (e *Engine) validateA2ANode(node domain.WorkflowNode, role domain.AgentDefinition, gates []verificationRun) error {
	switch {
	case !e.A2AEnabled():
		return errors.New("A2A executor is not configured")
	case normalizeExecutionScope(node.ExecutionScope) == domain.ExecutionScopeRepositories:
		return errors.New("A2A artifacts require run-scoped outputs")
	case role.Loop.Enabled:
		return errors.New("A2A nodes require a single role invocation per attempt")
	case role.PlatformMCP || role.KnowledgeBase || len(role.Rules)+len(role.Skills)+len(role.MCPServers)+len(role.Hooks) != 0:
		return errors.New("A2A nodes take their runtime capabilities from the configured service")
	case node.ExecutorOverrideID == "" && role.Model != "" && role.Model != "default":
		return errors.New("A2A nodes use the configured service model")
	}
	for _, candidate := range gates {
		gate := candidate.gate
		if gate.Mode != "manual" && gate.Mode != "submit" && len(gate.Commands) != 0 {
			return errors.New("A2A node verification commands must run in a downstream script node")
		}
	}
	return nil
}

func (e *Engine) stopUnownedA2ATasks(run domain.Run) {
	if e.a2aClient == nil {
		return
	}
	for _, state := range run.Nodes {
		if state == nil || state.A2ATaskID == "" {
			continue
		}
		switch state.Status {
		case "running", "pending", "pending_input", "waiting_approval":
			e.stopA2ATaskIfRequested(run.ID, state.A2ATaskID)
		}
	}
}
