package runtime

import "os"

func PersonalExecutorsOnly() bool {
	return os.Getenv("AGENTFLOW_DEPLOYMENT_MODE") == "local"
}

func IsPersonalExecutor(id string) bool {
	switch id {
	case "codex", "cursor", "claude", "claude-code":
		return true
	default:
		return false
	}
}
