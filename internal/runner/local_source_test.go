package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"agentflow/internal/domain"
)

func TestLocalSourceSnapshotKeepsWorkingTreeChangesAndFrozenBaseline(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	project := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git(project, "init", "-q")
	write(filepath.Join(project, ".gitignore"), "ignored.txt\n")
	write(filepath.Join(project, "tracked.txt"), "previous value")
	git(project, "add", ".")
	write(filepath.Join(project, "tracked.txt"), "edited value")
	write(filepath.Join(project, "new.txt"), "untracked value")
	write(filepath.Join(project, "ignored.txt"), "not in snapshot")
	workspaces := t.TempDir()
	workspace := filepath.Join(workspaces, "runs", "run-test", "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	daemon := Daemon{cfg: Config{LocalSourceRoots: []string{project}, DefaultWorkspaceRoot: workspaces}}
	inv := &domain.RunnerInvocation{LocalSourcePath: project}
	if err := daemon.checkoutInvocationWorkspace(context.Background(), workspace, inv); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(project, "tracked.txt"), "edited after start")
	if err := daemon.checkoutInvocationWorkspace(context.Background(), workspace, inv); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"tracked.txt": "edited value", "new.txt": "untracked value"} {
		bytes, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil || string(bytes) != want {
			t.Errorf("%s = %q, %v; want %q", name, bytes, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "ignored.txt")); !os.IsNotExist(err) {
		t.Errorf("ignored input was copied: %v", err)
	}
	git(workspace, "rev-parse", "HEAD")
}

func TestLocalSourceRejectsSymlinkOutsideSharedRoot(t *testing.T) {
	shared, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(shared, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	daemon := Daemon{cfg: Config{LocalSourceRoots: []string{shared}, DefaultWorkspaceRoot: t.TempDir()}}
	if result := daemon.readLocalSources(link); result.Status != 400 {
		t.Fatalf("path outside configured root was accepted: %+v", result)
	}
	destination := t.TempDir()
	if err := copyLocalSource(context.Background(), shared, destination); err == nil {
		t.Fatal("snapshot accepted a symlink outside its source")
	}
}

func TestLocalSourceSnapshotKeepsMultipleDirectoriesInOneBaseline(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	shared := t.TempDir()
	paths := []string{filepath.Join(shared, "first", "api"), filepath.Join(shared, "second", "api")}
	for index, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "file.txt"), []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			if err := os.Symlink(path, filepath.Join(shared, "also-first")); err != nil {
				t.Fatal(err)
			}
		}
	}
	workspaces := t.TempDir()
	daemon := Daemon{cfg: Config{LocalSourceRoots: []string{shared}, DefaultWorkspaceRoot: workspaces}}
	resolved, err := daemon.resolveLocalSources(append(append([]string{}, paths...), filepath.Join(shared, "also-first")))
	if err != nil || len(resolved) != 2 {
		t.Fatalf("canonical source selection: %v, %v", resolved, err)
	}
	workspace := filepath.Join(workspaces, "runs", "run-test", "workspace")
	invocation := &domain.RunnerInvocation{LocalSourcePaths: resolved}
	if err := daemon.checkoutInvocationWorkspace(context.Background(), workspace, invocation); err != nil {
		t.Fatal(err)
	}
	for index, rel := range []string{"sources/api/file.txt", "sources/api-2/file.txt"} {
		cmd := exec.Command("git", "show", "HEAD:"+rel)
		cmd.Dir = workspace
		content, err := cmd.Output()
		if err != nil || string(content) != paths[index] {
			t.Errorf("baseline %s = %q, %v", rel, content, err)
		}
	}
}

func TestLocalSourceAllowsGitRootContainingExcludedRunnerState(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(parent, "checkout")
	state := filepath.Join(source, "local_sop", ".local-sop", "runner", "workspaces")
	for _, dir := range []string{state, filepath.Join(source, ".git")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	d := Daemon{cfg: Config{DefaultWorkspaceRoot: state}}
	if err := d.validateSnapshotPath(source); err != nil {
		t.Fatalf("Git checkout with excluded state should remain selectable: %v", err)
	}
	for _, dir := range []string{parent, state, filepath.Join(source, "local_sop")} {
		if err := d.validateSnapshotPath(dir); err == nil {
			t.Errorf("accepted runner state or an unrestricted ancestor: %s", dir)
		}
	}
}
