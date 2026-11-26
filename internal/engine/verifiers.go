package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

type VerifierEvidence struct {
	Task         string            `json:"task"`
	ScriptOutput string            `json:"scriptOutput,omitempty"`
	Artifacts    map[string]string `json:"artifacts,omitempty"`
}

type VerifierVerdict struct {
	Passed   bool     `json:"passed"`
	Reason   string   `json:"reason"`
	Findings []string `json:"findings,omitempty"`
}

func nodeVerifiers(wf domain.WorkflowDefinition, node domain.WorkflowNode, defaultFailure string) ([]verificationRun, error) {
	if node.Verification == nil {
		return nil, nil
	}
	gates := make([]verificationRun, 0, len(node.Verification.Verifiers))
	for _, ref := range node.Verification.Verifiers {
		v, ok := wf.VerifierSnapshots[ref.VerifierID]
		if !ok {
			return nil, fmt.Errorf("节点「%s」缺少 Verifier %q 的运行版本", node.Name, ref.VerifierID)
		}
		gate := domain.VerificationGate{ID: v.ID, Name: v.Name, Mode: "command", Required: true, ExecutionScope: v.ExecutionScope}
		if v.HasScript() {
			gate.Commands = []string{"set -e\n" + v.Script}
		}
		gates = append(gates, verificationRun{
			gate:      gate,
			onFailure: resolveGateOnFailure(ref.OnFailure, defaultFailure),
			verifier:  &v,
		})
	}
	return gates, nil
}

// A round reads one node attempt. Collect all outcomes before making one
// routing decision, in configuration order for stable feedback.
func (e *Engine) runNodeVerifiers(ctx context.Context, run domain.Run, node domain.WorkflowNode, outputs nodeOutputs, verifiers []verificationRun, execute gateCommandExecutor, iteration int) (error, string) {
	results := make([]error, len(verifiers))
	sink := func(event domain.TelemetryEvent) { e.observe(run.ID, node.ID, event) }
	var running sync.WaitGroup
	for i, candidate := range verifiers {
		running.Go(func() {
			v := candidate.verifier
			var output string
			var err error
			if v.HasScript() {
				scriptCtx, cancel := context.WithTimeout(ctx, time.Duration(v.TimeoutSec)*time.Second)
				output, err = executeVerificationCommand(scriptCtx, run, node, candidate, execute, sink, iteration)
				cancel()
			}
			if err == nil && v.Review != nil && ctx.Err() == nil {
				err = e.reviewVerifier(ctx, run, node, *v, outputs, output, iteration)
			}
			if err != nil {
				results[i] = fmt.Errorf("%s: %w", v.Name, err)
			}
		})
	}
	running.Wait()
	if err := ctx.Err(); err != nil {
		return err, ""
	}
	var failures []error
	var ids []string
	actions := map[string]bool{}
	for i, err := range results {
		if err != nil {
			failures = append(failures, err)
			ids = append(ids, verifiers[i].verifier.ID)
			actions[verifiers[i].onFailure] = true
		}
	}
	if len(failures) == 0 {
		sink(domain.TelemetryEvent{
			Category: "verification", Name: "verifier.round.passed", Phase: "finish", Message: "本轮 Verifier 全部通过",
			Attributes: map[string]any{"iteration": iteration, "total": len(verifiers)},
		})
		return nil, ""
	}
	// Give a requested human decision precedence over automated routing.
	// Explicit failure takes precedence over another round of rework.
	for _, action := range []string{"human", "fail", "reject", "loop"} {
		if actions[action] {
			sink(domain.TelemetryEvent{
				Category: "verification", Name: "verifier.round.failed", Phase: "finish", Level: "error",
				Message:    "本轮 Verifier 未全部通过",
				Attributes: map[string]any{"iteration": iteration, "total": len(verifiers), "failedVerifierIds": ids, "onFailure": action},
			})
			return errors.Join(failures...), action
		}
	}
	return errors.Join(failures...), "fail"
}

type gateCommandExecutor func(context.Context, string, executionTarget, runtime.EventSink) (string, error)

func executeVerificationCommand(ctx context.Context, run domain.Run, node domain.WorkflowNode, candidate verificationRun, execute gateCommandExecutor, sink runtime.EventSink, iteration int) (string, error) {
	gate := candidate.gate
	command := joinGateCommands(gate.Commands, node.ArtifactOutputs)
	if command == "" {
		return "", nil
	}
	gateSink := func(event domain.TelemetryEvent) {
		event.Category = "verification"
		if event.Attributes == nil {
			event.Attributes = map[string]any{}
		}
		event.Attributes["gateId"] = gate.ID
		event.Attributes["gateName"] = gate.Name
		event.Attributes["iteration"] = iteration
		event.Attributes["command"] = command
		if candidate.verifier != nil {
			event.Attributes["verifierId"] = candidate.verifier.ID
			event.Attributes["verifierVersion"] = candidate.verifier.Version
		}
		switch event.Name {
		case "process.started":
			event.Message = "门禁开始：" + gate.Name
		case "process.finished":
			if event.Level == "error" {
				event.Message = "门禁未通过：" + gate.Name
			} else {
				event.Message = "门禁通过：" + gate.Name
			}
		}
		sink(event)
	}
	output, err := executeForTargets(run, gate.ExecutionScope, func(target executionTarget) (string, error) {
		return execute(ctx, command, target, executionTargetSink(gateSink, target))
	})
	if err == nil {
		return output, nil
	}
	detail := strings.TrimSpace(output)
	if len(detail) > 2_000 {
		detail = detail[len(detail)-2_000:]
	}
	if detail == "" {
		detail = "no command output"
	}
	if candidate.verifier != nil {
		return output, fmt.Errorf("脚本未通过：%w (%s)", err, detail)
	}
	return output, fmt.Errorf("verification %s command %q: %w (%s)", gate.Name, command, err, detail)
}

func collectVerifierEvidence(outputs nodeOutputs, run domain.Run, node domain.WorkflowNode, names []string, scriptOutput string) (VerifierEvidence, error) {
	evidence := VerifierEvidence{
		Task:         clipVerifierText(run.Input, 5000),
		ScriptOutput: clipVerifierText(scriptOutput, 10000),
	}
	if len(names) == 0 {
		return evidence, nil
	}
	targets, err := executionTargets(run, node.ExecutionScope)
	if err != nil {
		return evidence, err
	}
	evidence.Artifacts = make(map[string]string)
	remaining := 28000
	for _, name := range names {
		var path string
		for _, contract := range node.ArtifactOutputs {
			if contract.Name == name {
				path = contract.Path
				break
			}
		}
		if path == "" {
			return evidence, fmt.Errorf("当前节点未声明评审产物 %q", name)
		}
		for _, target := range targets {
			dir := target.directory()
			rel, ok := artifactRelPath(dir, path)
			if !ok || remaining <= 0 {
				return evidence, fmt.Errorf("评审产物路径无效或评审输入过长：%s", name)
			}
			raw, err := readVerifierText(outputs, rel)
			if err != nil {
				return evidence, fmt.Errorf("读取评审产物 %q：%w", rel, err)
			}
			if strings.ContainsRune(string(raw), 0) || !utf8.Valid(raw) {
				return evidence, fmt.Errorf("评审产物 %q 需要是 UTF-8 文本", name)
			}
			key := name
			if dir != "" {
				key = dir + "/" + key
			}
			evidence.Artifacts[key] = clipVerifierText(string(raw), min(6000, remaining))
			remaining -= len(evidence.Artifacts[key])
		}
	}

	return evidence, nil
}

func readVerifierText(outputs nodeOutputs, rel string) ([]byte, error) {
	workspace, ok := outputs.(workspaceOutputs)
	if !ok {
		return outputs.readFile(rel)
	}
	root, err := os.OpenRoot(string(workspace))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errOutputNotRegular
	}
	if info.Size() > maxArtifactBytes {
		return nil, errOutputTooLarge
	}
	return io.ReadAll(io.LimitReader(file, maxArtifactBytes))
}

func clipVerifierText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	for !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + "\n[内容已截断]"
}

func (e *Engine) reviewVerifier(ctx context.Context, run domain.Run, node domain.WorkflowNode, verifier domain.VerifierDefinition, outputs nodeOutputs, scriptOutput string, iteration int) error {
	review := *verifier.Review
	sink := func(event domain.TelemetryEvent) { e.observe(run.ID, node.ID, event) }
	metadata := map[string]any{"verifierId": verifier.ID, "verifierVersion": verifier.Version, "model": review.Model, "executorId": review.ExecutorID, "iteration": iteration}
	sink(domain.TelemetryEvent{Category: "verification", Name: "verifier.review.started", Phase: "start", Message: "Verifier 模型评审开始：" + verifier.Name, Attributes: metadata})
	verdict, err := e.verifierVerdict(ctx, run, node, outputs, review, scriptOutput)
	name, level, message := "verifier.review.passed", "", "Verifier 模型评审通过："+verifier.Name
	if err != nil {
		name, level, message = "verifier.review.error", "error", err.Error()
	} else if !verdict.Passed {
		name, level, message = "verifier.review.failed", "error", "Verifier 模型评审未通过："+verdict.Reason
		err = fmt.Errorf("%s", message)
	} else {
		message += " · " + verdict.Reason
	}
	sink(domain.TelemetryEvent{Category: "verification", Name: name, Phase: "finish", Level: level, Message: message, Attributes: metadata})
	if name == "verifier.review.error" {
		return err
	}
	nr := run.Nodes[node.ID]
	if nr == nil {
		return err
	}
	body, _ := json.MarshalIndent(struct {
		VerifierID string `json:"verifierId"`
		Version    int    `json:"version"`
		ExecutorID string `json:"executorId"`
		Model      string `json:"model"`
		VerifierVerdict
	}{verifier.ID, verifier.Version, review.ExecutorID, review.Model, verdict}, "", "  ")
	artifactName := fmt.Sprintf("verifier-%s-%d", verifier.ID, iteration)
	artifact := newArtifact(run.ID, node.ID, nr.Attempt, artifactName, artifactName+".json", "application/json", body)
	e.updateRun(run.ID, func(active *domain.Run) {
		if shouldCommitNodeOutcome(*active, active.Nodes[node.ID], run.Generation, nr.Attempt, true) {
			active.Artifacts = append(active.Artifacts, artifact)
		}
	})
	return err
}

func (e *Engine) verifierVerdict(ctx context.Context, run domain.Run, node domain.WorkflowNode, outputs nodeOutputs, review domain.VerifierReview, scriptOutput string) (VerifierVerdict, error) {
	if e.ReviewVerifier == nil {
		return VerifierVerdict{}, fmt.Errorf("本实例未配置 Verifier 模型评审执行能力")
	}
	evidence, err := collectVerifierEvidence(outputs, run, node, review.ArtifactNames, scriptOutput)
	if err != nil {
		return VerifierVerdict{}, err
	}
	modelCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	return e.ReviewVerifier(modelCtx, run, review, evidence)
}
