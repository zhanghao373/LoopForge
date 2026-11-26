package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// StageRunnerLocalLogin exposes only the selected CLI's login to its private
// HOME. The runner retains ownership of its source login and all settings.
// The returned cleanup must run after the CLI and its descendants have exited.
func StageRunnerLocalLogin(ctx context.Context, provider, driver string, home AgentCLIHome) (fileAuth bool, cleanup func() error, err error) {
	host, err := os.UserHomeDir()
	if err != nil {
		return false, nil, err
	}
	var directory, file, source string
	switch provider {
	case "codex":
		directory, file = ".codex", "auth.json"
		source = localCLIConfigDir("CODEX_HOME", filepath.Join(host, directory))
	case "claude-code":
		directory, file = ".claude", ".credentials.json"
		source = localCLIConfigDir("CLAUDE_CONFIG_DIR", filepath.Join(host, directory))
	case AgentCLICursor:
		directory, file = ".cursor", "auth.json"
		source = filepath.Join(host, directory)
		if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); runtime.GOOS != "darwin" && xdg != "" {
			source = filepath.Join(xdg, "cursor")
		}
		source = localCLIConfigDir("CURSOR_CONFIG_DIR", source)
	default:
		return false, nil, fmt.Errorf("执行器 %s 不支持本机登录", provider)
	}

	hint := RunnerLocalLoginHint(provider)

	// Open the private config directory as a confined root. Workspace writes
	// can change its contents but cannot redirect these host-side operations.
	home.Rel = filepath.Join(home.Rel, directory)
	dest, err := openAgentCLIHome(home)
	if err != nil {
		return false, nil, err
	}
	remove := func() error {
		if err := dest.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	closeAndRemove := func() error {
		defer dest.Close()
		return remove()
	}
	if err := remove(); err != nil {
		dest.Close()
		return false, nil, err
	}

	data, err := readRunnerLoginFile(filepath.Join(source, file))
	if os.IsNotExist(err) && runtime.GOOS == "darwin" {
		data = runnerLoginFromMacKeychain(ctx, provider, source, host)
	}
	if err != nil && !os.IsNotExist(err) {
		dest.Close()
		return false, nil, fmt.Errorf("无法读取 %s 的本机登录状态，%s", AgentCLILabel(provider), hint)
	}
	if len(data) == 0 && provider == AgentCLICursor && driver != "docker" {
		// The native Cursor process can use its own OS keyring while keeping
		// CLI configuration and session files in the private HOME.
		return false, closeAndRemove, nil
	}
	var credential map[string]json.RawMessage
	if json.Unmarshal(data, &credential) != nil || len(credential) == 0 {
		dest.Close()
		if provider == AgentCLICursor && driver == "docker" {
			hint = "Docker 无法访问宿主机密钥环，请在执行机器上用 AGENT_CLI_CREDENTIAL_STORE=file agent login 保存文件登录态"
		}
		return false, nil, fmt.Errorf("%s 本机登录不可用。%s", AgentCLILabel(provider), hint)
	}
	f, err := dest.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, err = f.Write(data)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		_ = closeAndRemove()
		return false, nil, fmt.Errorf("无法为 %s 准备本机登录状态", AgentCLILabel(provider))
	}
	return true, closeAndRemove, nil
}

// RunnerLocalLoginHint describes the selected CLI's interactive login path.
func RunnerLocalLoginHint(provider string) string {
	switch provider {
	case "codex":
		return "请在执行机器上运行 codex login"
	case "claude-code":
		return "请在执行机器上运行 claude auth login"
	default:
		return "请在执行机器上运行 agent login"
	}
}

func localCLIConfigDir(key, fallback string) string {
	if path := strings.TrimSpace(os.Getenv(key)); path != "" {
		return path
	}
	return fallback
}

func readRunnerLoginFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("login file must be private and regular")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil || !os.SameFile(info, current) {
		return nil, fmt.Errorf("login file changed while opening")
	}
	const maxBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if len(data) > maxBytes {
		return nil, fmt.Errorf("login file exceeds size limit")
	}
	return data, err
}

func runnerLoginFromMacKeychain(ctx context.Context, provider, source, home string) []byte {
	var args []string
	switch provider {
	case "codex":
		path, err := filepath.EvalSymlinks(source)
		if err != nil {
			path = source
		}
		sum := sha256.Sum256([]byte(path))
		args = []string{"-s", "Codex Auth", "-a", fmt.Sprintf("cli|%x", sum[:8])}
	case "claude-code":
		// The default login uses this Keychain service. Custom config
		// directories can use their own file-based credential instead.
		if filepath.Clean(source) != filepath.Join(home, ".claude") {
			return nil
		}
		args = []string{"-s", "Claude Code-credentials"}
	default:
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	data, _ := exec.CommandContext(probeCtx, "/usr/bin/security", append([]string{"find-generic-password", "-w"}, args...)...).Output()
	return data
}

func cursorLoginEnvironment(home string, fileAuth bool) map[string]string {
	env := map[string]string{
		"CURSOR_CONFIG_DIR": filepath.Join(home, ".cursor"),
		"CURSOR_API_KEY":    "", "CURSOR_AUTH_TOKEN": "",
		"AGENT_CLI_CREDENTIAL_STORE": "",
	}
	if fileAuth {
		env["AGENT_CLI_CREDENTIAL_STORE"] = "file"
	}
	return env
}
