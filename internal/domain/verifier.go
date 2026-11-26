package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// VerifierDefinition is a project-owned, published verification recipe.
// Updating it advances Version. A Run stores the exact version it resolved.
type VerifierDefinition struct {
	ID             string          `json:"id"`
	ProjectID      string          `json:"projectId"`
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Version        int             `json:"version"`
	Script         string          `json:"script"`
	ExecutionScope string          `json:"executionScope"`
	TimeoutSec     int             `json:"timeoutSec"`
	Review         *VerifierReview `json:"review,omitempty"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

type VerifierReview struct {
	ExecutorID    string   `json:"executorId"`
	Model         string   `json:"model"`
	Instructions  string   `json:"instructions"`
	ArtifactNames []string `json:"artifactNames,omitempty"`
}

type VerifierReference struct {
	VerifierID string `json:"verifierId"`
	OnFailure  string `json:"onFailure,omitempty"`
}

// A nil NodeVerification inherits legacy role gates. A non-nil empty list
// explicitly disables the additional gates for this node.
type NodeVerification struct {
	Verifiers []VerifierReference `json:"verifiers"`
}

var verifierIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func (v VerifierDefinition) HasScript() bool {
	return strings.TrimSpace(v.Script) != ""
}

func (v VerifierDefinition) Validate() error {
	if !verifierIDPattern.MatchString(v.ID) || strings.TrimSpace(v.ProjectID) == "" || strings.TrimSpace(v.Name) == "" || len(v.Name) > 255 {
		return fmt.Errorf("Verifier 需要项目、名称和有效标识（最多 64 个字母、数字、连字符或下划线）")
	}
	if !v.HasScript() && v.Review == nil {
		return fmt.Errorf("Verifier 至少需要启用验收脚本或模型评审")
	}
	if v.HasScript() {
		if len(v.Script) > 64<<10 || strings.ContainsRune(v.Script, 0) {
			return fmt.Errorf("请输入有效的 Shell 脚本（最多 64 KB）")
		}
		if v.ExecutionScope != ExecutionScopeRun && v.ExecutionScope != ExecutionScopeRepositories {
			return fmt.Errorf("请选择 Verifier 的执行范围")
		}
		if v.TimeoutSec < 1 || v.TimeoutSec > 3600 {
			return fmt.Errorf("脚本超时须在 1 至 3600 秒之间")
		}
	}
	if review := v.Review; review != nil {
		if review.ExecutorID != "codex" && review.ExecutorID != "claude-code" {
			return fmt.Errorf("模型评审请选择 Codex 或 Claude Code")
		}
		if strings.TrimSpace(review.Model) == "" || len(review.Model) > 256 || strings.ContainsAny(review.Model, "\r\n\x00") {
			return fmt.Errorf("请输入有效的评审模型 ID")
		}
		if strings.TrimSpace(review.Instructions) == "" || len(review.Instructions) > 12000 {
			return fmt.Errorf("模型评审需要评审标准（最多 12000 字节）")
		}
		if len(review.ArtifactNames) > 8 {
			return fmt.Errorf("模型评审最多可读取 8 种产物")
		}
		seen := map[string]bool{}
		for _, name := range review.ArtifactNames {
			if name == "" || name != strings.TrimSpace(name) || len(name) > 255 || seen[name] {
				return fmt.Errorf("存在无效或重复的评审产物名 %q", name)
			}
			seen[name] = true
		}
	}
	return nil
}

func (n WorkflowNode) ValidateVerification() error {
	if n.Verification == nil {
		return nil
	}
	switch n.Type {
	case "agent", "script", "command":
	default:
		return fmt.Errorf("节点「%s」无法引用 Verifier", n.Name)
	}
	seen := map[string]bool{}
	for _, ref := range n.Verification.Verifiers {
		if !verifierIDPattern.MatchString(ref.VerifierID) || seen[ref.VerifierID] {
			return fmt.Errorf("节点「%s」存在无效或重复的 Verifier 引用", n.Name)
		}
		seen[ref.VerifierID] = true
		switch ref.OnFailure {
		case "", "fail", "human", "loop":
		case "reject":
			if n.RejectTo == "" {
				return fmt.Errorf("节点「%s」的 Verifier 选择了打回上游，请配置返工目标", n.Name)
			}
		default:
			return fmt.Errorf("节点「%s」的 Verifier 失败策略无效", n.Name)
		}
	}
	return nil
}
