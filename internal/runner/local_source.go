package runner

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agentflow/internal/domain"
)

func (d *Daemon) localSourceRoots() []string {
	var roots []string
	for _, configured := range d.cfg.LocalSourceRoots {
		if !filepath.IsAbs(configured) {
			continue
		}
		root, err := filepath.EvalSymlinks(configured)
		if err != nil {
			continue
		}
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			roots = append(roots, root)
		}
	}
	return roots
}

func (d *Daemon) localSourcePath(path string, roots []string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("请选择执行机器上的绝对目录")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("无法打开源目录")
	}
	for _, root := range roots {
		if withinDirectory(root, resolved) {
			info, err := os.Stat(resolved)
			if err != nil || !info.IsDir() {
				break
			}
			return resolved, nil
		}
	}
	return "", fmt.Errorf("目录不在这台执行机器已开放的路径内")
}

func withinDirectory(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(relative)
}

func (d *Daemon) validateSnapshotPath(path string) error {
	workspace, err := filepath.Abs(d.cfg.DefaultWorkspaceRoot)
	if err != nil {
		return err
	}
	if canonical, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = canonical
	}
	if withinDirectory(workspace, path) {
		return fmt.Errorf("请选择项目目录，执行机的管理目录不能作为来源")
	}
	if relative, err := filepath.Rel(path, workspace); err == nil && filepath.IsLocal(relative) {
		// A Git checkout can host the local deployment beneath an excluded
		// directory. copyLocalSource skips that subtree even if tracked.
		_, gitErr := os.Lstat(filepath.Join(path, ".git"))
		if gitErr != nil || !ignoredSourcePath(filepath.ToSlash(relative)) {
			return fmt.Errorf("请选择项目目录，执行机的管理目录不能作为来源")
		}
	}
	for _, name := range strings.Split(path, string(filepath.Separator)) {
		if ignoredSourceDirectory(name) {
			return fmt.Errorf("执行机的管理目录不能作为来源")
		}
	}
	return nil
}

func (d *Daemon) readLocalSources(path string) domain.RunnerWorkspaceResponse {
	roots := d.localSourceRoots()
	if len(roots) == 0 {
		return domain.RunnerWorkspaceResponse{Status: 409, Error: "执行机器尚未开放本机项目目录"}
	}
	if path == "" {
		return runnerWorkspacePayload(domain.RunnerLocalSources{Directories: roots})
	}
	resolved, err := d.localSourcePath(path, roots)
	if err != nil {
		return domain.RunnerWorkspaceResponse{Status: 400, Error: err.Error()}
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return domain.RunnerWorkspaceResponse{Status: 400, Error: "无法读取此目录"}
	}
	result := domain.RunnerLocalSources{Path: resolved, Selectable: d.validateSnapshotPath(resolved) == nil, Directories: []string{}}
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && !ignoredSourceDirectory(entry.Name()) {
			result.Directories = append(result.Directories, filepath.Join(resolved, entry.Name()))
		}
	}
	if parent := filepath.Dir(resolved); parent != resolved {
		for _, root := range roots {
			if withinDirectory(root, parent) {
				result.Parent = parent
				break
			}
		}
	}
	return runnerWorkspacePayload(result)
}

func ignoredSourceDirectory(name string) bool {
	switch name {
	case ".git", ".agentflow", ".local-sop", "node_modules", ".venv", "__pycache__":
		return true
	}
	return false
}

func invocationSourcePaths(invocation *domain.RunnerInvocation) []string {
	if invocation == nil {
		return nil
	}
	return domain.LocalSourceSelection(invocation.LocalSourcePath, invocation.LocalSourcePaths)
}

func (d *Daemon) resolveLocalSources(paths []string) ([]string, error) {
	roots := d.localSourceRoots()
	if len(roots) == 0 {
		return nil, fmt.Errorf("执行机器尚未开放本机项目目录")
	}
	resolved := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		canonical, err := d.localSourcePath(path, roots)
		if err != nil {
			return nil, fmt.Errorf("%s：%w", path, err)
		}
		if err := d.validateSnapshotPath(canonical); err != nil {
			return nil, fmt.Errorf("%s：%w", path, err)
		}
		if !seen[canonical] {
			seen[canonical] = true
			resolved = append(resolved, canonical)
		}
	}
	return resolved, nil
}

func (d *Daemon) checkoutInvocationWorkspace(ctx context.Context, workspace string, invocation *domain.RunnerInvocation, refreshClean ...bool) error {
	if paths := invocationSourcePaths(invocation); len(paths) > 0 {
		if err := d.seedLocalSource(ctx, workspace, invocation, paths); err != nil {
			return fmt.Errorf("准备本机目录副本: %w", err)
		}
	} else if err := os.MkdirAll(workspace, 0o755); err != nil {
		return err
	}
	return checkoutInvocationRepositories(ctx, workspace, invocation, refreshClean...)
}

func (d *Daemon) seedLocalSource(ctx context.Context, workspace string, invocation *domain.RunnerInvocation, paths []string) error {
	if !withinDirectory(d.cfg.DefaultWorkspaceRoot, workspace) || workspace == d.cfg.DefaultWorkspaceRoot {
		return fmt.Errorf("无效的运行工作区")
	}
	unlock := lockInvocationWorkspace(workspace)
	defer unlock()
	stamp := filepath.Join(workspace, ".git", "loopforge-local-source")
	if _, err := os.Stat(stamp); err == nil {
		return nil
	}
	sources, err := d.resolveLocalSources(paths)
	if err != nil {
		return err
	}
	if len(sources) != len(paths) {
		return fmt.Errorf("本机来源目录已变更，请重新选择")
	}
	directories := domain.LocalSourceDirectories(paths)
	parent := filepath.Dir(workspace)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	staged, err := os.MkdirTemp(parent, ".local-source-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staged)
	for i, source := range sources {
		destination := filepath.Join(staged, filepath.FromSlash(directories[i]))
		if err := os.MkdirAll(destination, 0o700); err != nil {
			return err
		}
		if err := copyLocalSource(ctx, source, destination); err != nil {
			return fmt.Errorf("%s：%w", directories[i], err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "--template=", "-b", "agentflow-base"},
		{"add", "-f", "-A"},
		{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.email=loopforge@local", "-c", "user.name=LoopForge", "commit", "-q", "--allow-empty", "-m", "LoopForge local source"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = staged
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("create baseline: %w (%s)", err, strings.TrimSpace(string(output)))
		}
	}
	if err := configureRepositoryGitIdentity(ctx, staged, invocation.GitUserName, invocation.GitUserEmail); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staged, ".git", "loopforge-local-source"), nil, 0o600); err != nil {
		return err
	}
	// A preflight may have created the workspace directory. Remove only an
	// empty directory, so a restored checkout or another writer is preserved.
	if err := os.Remove(workspace); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("workspace is not empty: %w", err)
	}
	if err := os.Rename(staged, workspace); err != nil {
		return fmt.Errorf("publish source snapshot: %w", err)
	}
	return nil
}

func copyLocalSource(ctx context.Context, source, destination string) error {
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	copyEntry := func(path string, entry fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(path))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if ignoredSourceDirectory(entry.Name()) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := root.Readlink(filepath.FromSlash(path))
			if err != nil {
				return err
			}
			relative := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(path)), link))
			if filepath.IsAbs(link) || !filepath.IsLocal(relative) {
				return fmt.Errorf("符号链接 %s 指向源目录外", path)
			}
			return os.Symlink(link, target)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		input, err := root.Open(filepath.FromSlash(path))
		if err != nil {
			return err
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("源文件 %s 已变更", path)
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600|info.Mode().Perm()&0o111)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	// Git supplies tracked and non-ignored untracked files, preserving working
	// tree edits without copying Git config, history, or ignored dependencies.
	if _, err := root.Lstat(".git"); err == nil {
		cmd := exec.CommandContext(ctx, "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		cmd.Dir = source
		files, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("读取源仓库文件: %w", err)
		}
		seen := map[string]bool{}
		for _, path := range strings.Split(string(files), "\x00") {
			if path == "" || seen[path] || !fs.ValidPath(path) || ignoredSourcePath(path) {
				continue
			}
			seen[path] = true
			info, err := root.Lstat(filepath.FromSlash(path))
			if os.IsNotExist(err) {
				continue // a tracked file deleted in the working tree
			}
			if err != nil {
				return err
			}
			if err := copyEntry(path, fs.FileInfoToDirEntry(info)); err != nil {
				return err
			}
		}
		return nil
	}
	return fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == "." {
			return err
		}
		if entry.IsDir() && ignoredSourceDirectory(entry.Name()) {
			return fs.SkipDir
		}
		return copyEntry(path, entry)
	})
}

func ignoredSourcePath(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if ignoredSourceDirectory(part) {
			return true
		}
	}
	return false
}
