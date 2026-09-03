package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sean2077/pairroom/internal/execx"
	"github.com/sean2077/pairroom/internal/model"
)

// Manager materializes an auditable reviewer snapshot from the live Git
// working tree. It intentionally remains independent from either vendor
// runtime so the same boundary can be applied to Claude Code and Codex.
type Manager struct {
	mu      sync.Mutex
	repo    string
	dataDir string
	root    string
}

func New(repo, dataDir string) (*Manager, error) {
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("repository and data directory are required")
	}
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return nil, fmt.Errorf("resolve repository: %w", err)
	}
	canonicalRepo, err = filepath.Abs(canonicalRepo)
	if err != nil {
		return nil, fmt.Errorf("resolve repository path: %w", err)
	}
	if info, err := os.Stat(canonicalRepo); err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return nil, fmt.Errorf("invalid repository: %w", err)
	}
	if out, err := runGit(context.Background(), canonicalRepo, nil, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(string(out)) != "true" {
		if err == nil {
			err = errors.New("not inside a Git working tree")
		}
		return nil, fmt.Errorf("reviewer snapshots require Git: %w", err)
	}
	absoluteData, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	return &Manager{
		repo:    filepath.Clean(canonicalRepo),
		dataDir: filepath.Clean(absoluteData),
		root:    filepath.Join(absoluteData, "runtime", "reviewer-worktree"),
	}, nil
}

func (m *Manager) DriverBoundary() model.WorkspaceBoundary {
	return model.WorkspaceBoundary{
		Kind:             "driver-live",
		Path:             m.repo,
		ReadOnly:         false,
		ReadOnlyEnforced: false,
	}
}

func (m *Manager) ReviewerPath() string { return m.root }

// Refresh recreates the reviewer snapshot from HEAD plus the complete dirty
// tracked diff and untracked regular files. It fails closed when a safe,
// self-contained snapshot cannot be produced.
func (m *Manager) Refresh(ctx context.Context) (model.WorkspaceBoundary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.removeLocked(ctx); err != nil {
		return model.WorkspaceBoundary{}, err
	}
	if err := os.MkdirAll(filepath.Dir(m.root), 0o700); err != nil {
		return model.WorkspaceBoundary{}, fmt.Errorf("create reviewer runtime directory: %w", err)
	}

	headBytes, err := runGit(ctx, m.repo, nil, "rev-parse", "HEAD")
	if err != nil {
		return model.WorkspaceBoundary{}, fmt.Errorf("resolve source HEAD: %w", err)
	}
	head := strings.TrimSpace(string(headBytes))
	if head == "" {
		return model.WorkspaceBoundary{}, errors.New("repository has no HEAD commit")
	}
	if _, err := runGit(ctx, m.repo, nil, "worktree", "add", "--detach", "--force", m.root, head); err != nil {
		return model.WorkspaceBoundary{}, fmt.Errorf("create reviewer worktree: %w", err)
	}
	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			_ = m.removeLocked(context.Background())
		}
	}()

	pathspec := m.snapshotPathspec()
	diffArgs := append([]string{"diff", "--binary", "--no-ext-diff", head, "--"}, pathspec...)
	patch, err := runGit(ctx, m.repo, nil, diffArgs...)
	if err != nil {
		return model.WorkspaceBoundary{}, fmt.Errorf("capture dirty tracked changes: %w", err)
	}
	if len(bytes.TrimSpace(patch)) > 0 {
		if _, err := runGit(ctx, m.root, patch, "apply", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return model.WorkspaceBoundary{}, fmt.Errorf("apply dirty tracked changes to reviewer snapshot: %w", err)
		}
	}

	untrackedArgs := append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--"}, pathspec...)
	untrackedRaw, err := runGit(ctx, m.repo, nil, untrackedArgs...)
	if err != nil {
		return model.WorkspaceBoundary{}, fmt.Errorf("enumerate untracked files: %w", err)
	}
	untracked := splitNUL(untrackedRaw)
	sort.Strings(untracked)
	hasher := sha256.New()
	_, _ = hasher.Write([]byte("pairroom-reviewer-snapshot-v1\x00"))
	_, _ = hasher.Write([]byte(head))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write(patch)
	for _, rel := range untracked {
		if err := validateRelativePath(rel); err != nil {
			return model.WorkspaceBoundary{}, fmt.Errorf("unsafe untracked path %q: %w", rel, err)
		}
		src := filepath.Join(m.repo, filepath.FromSlash(rel))
		dst := filepath.Join(m.root, filepath.FromSlash(rel))
		info, err := os.Lstat(src)
		if err != nil {
			return model.WorkspaceBoundary{}, fmt.Errorf("stat untracked file %q: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return model.WorkspaceBoundary{}, fmt.Errorf("untracked symlink %q is not allowed in a reviewer snapshot", rel)
		}
		if !info.Mode().IsRegular() {
			return model.WorkspaceBoundary{}, fmt.Errorf("untracked path %q is not a regular file", rel)
		}
		if err := copyRegularFile(src, dst, info.Mode()); err != nil {
			return model.WorkspaceBoundary{}, err
		}
		contentHash, err := fileSHA256(dst)
		if err != nil {
			return model.WorkspaceBoundary{}, err
		}
		_, _ = hasher.Write([]byte(rel))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(contentHash))
		_, _ = hasher.Write([]byte{0})
	}
	if err := verifyContainedSymlinks(m.root); err != nil {
		return model.WorkspaceBoundary{}, err
	}

	readOnlyEnforced := false
	warnings := []string(nil)
	if runtime.GOOS == "windows" {
		warnings = append(warnings, "filesystem read-only bits are advisory on Windows; native reviewer sandbox remains authoritative")
	} else if err := makeTreeReadOnly(m.root); err != nil {
		warnings = append(warnings, "could not make reviewer snapshot read-only: "+err.Error())
	} else {
		readOnlyEnforced = true
	}
	cleanupOnError = false
	return model.WorkspaceBoundary{
		Kind:             "reviewer-snapshot",
		Path:             m.root,
		SourceHead:       head,
		PatchSHA256:      hex.EncodeToString(hasher.Sum(nil)),
		Dirty:            len(bytes.TrimSpace(patch)) > 0 || len(untracked) > 0,
		UntrackedCount:   len(untracked),
		ReadOnly:         true,
		ReadOnlyEnforced: readOnlyEnforced,
		RefreshedAt:      time.Now().UTC(),
		Warnings:         warnings,
	}, nil
}

func (m *Manager) Cleanup(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.removeLocked(ctx)
}

func (m *Manager) removeLocked(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Archive interrupts the active Agent Turn and then closes the Runtime,
	// which removes the reviewer worktree. On Windows a just-killed Agent
	// process can briefly still hold the working-directory handle inside that
	// worktree, so the removal can transiently fail with "in use". Retry for a
	// short grace window so the interrupt path does not fail closed on
	// asynchronous handle release; non-transient errors still fail immediately.
	var lastErr error
	for attempt := 0; attempt <= workspaceRemoveAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return errors.Join(lastErr, err)
			}
			return err
		}
		if _, err := os.Stat(m.root); errors.Is(err, os.ErrNotExist) {
			lastErr = nil
			break
		}
		_ = makeTreeWritable(m.root)
		_, worktreeErr := runGit(ctx, m.repo, nil, "worktree", "remove", "--force", m.root)
		removeErr := os.RemoveAll(m.root)
		if removeErr == nil {
			lastErr = nil
			break
		}
		if worktreeErr != nil {
			// A stale registration or interrupted creation may leave a directory
			// that Git no longer recognizes; the direct removal error is the one
			// to surface or retry.
			lastErr = fmt.Errorf("remove stale reviewer worktree: %w", removeErr)
		} else {
			lastErr = fmt.Errorf("remove reviewer worktree directory: %w", removeErr)
		}
		if attempt == workspaceRemoveAttempts || !isTransientRemoveError(removeErr) {
			return lastErr
		}
		select {
		case <-time.After(workspaceRemoveBackoff):
		case <-ctx.Done():
			return errors.Join(lastErr, ctx.Err())
		}
	}
	_, _ = runGit(ctx, m.repo, nil, "worktree", "prune")
	if err := os.RemoveAll(m.root); err != nil {
		return fmt.Errorf("remove reviewer worktree directory: %w", err)
	}
	return nil
}

const (
	// workspaceRemoveAttempts bounds how many times reviewer worktree removal
	// is retried while the native Agent process releases its handles.
	workspaceRemoveAttempts = 30
	workspaceRemoveBackoff  = 100 * time.Millisecond
)

// isTransientRemoveError reports whether a reviewer worktree removal failure is
// likely to clear on its own. On Windows, a just-killed Agent process can
// briefly hold the directory while the OS releases its handles; other platforms
// never see this because unlink does not require exclusive access.
func isTransientRemoveError(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case 5, 32: // ERROR_ACCESS_DENIED, ERROR_SHARING_VIOLATION
		return true
	}
	return false
}

func (m *Manager) snapshotPathspec() []string {
	paths := []string{"."}
	if rel, err := filepath.Rel(m.repo, m.dataDir); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
		paths = append(paths, ":(exclude)"+filepath.ToSlash(rel), ":(exclude)"+filepath.ToSlash(rel)+"/**")
	}
	return paths
}

func runGit(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	execx.NoConsole(cmd)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, errors.New(detail)
	}
	return stdout.Bytes(), nil
}

func splitNUL(data []byte) []string {
	parts := bytes.Split(data, []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		value := string(part)
		if value != "" {
			out = append(out, filepath.ToSlash(value))
		}
	}
	return out
}

func validateRelativePath(value string) error {
	if value == "" || filepath.IsAbs(value) {
		return errors.New("path must be relative")
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return errors.New("path escapes repository")
	}
	return nil
}

func copyRegularFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("create reviewer snapshot directory: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open untracked file %q: %w", src, err)
	}
	defer in.Close()
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o600
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("create reviewer snapshot file %q: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy untracked file %q: %w", src, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("sync reviewer snapshot file %q: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close reviewer snapshot file %q: %w", dst, err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open file for hash %q: %w", path, err)
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", fmt.Errorf("hash file %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func verifyContainedSymlinks(root string) error {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve reviewer snapshot root: %w", err)
	}
	canonicalRoot, err = filepath.Abs(canonicalRoot)
	if err != nil {
		return fmt.Errorf("resolve reviewer snapshot root: %w", err)
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve snapshot symlink %q: %w", path, err)
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return err
		}
		if !within(canonicalRoot, resolved) {
			return fmt.Errorf("snapshot symlink %q escapes reviewer workspace", path)
		}
		return nil
	})
}

func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func makeTreeReadOnly(root string) error {
	var paths []string
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return err
	}
	// Children first keeps traversal possible while permissions are changed.
	for i := len(paths) - 1; i >= 0; i-- {
		info, err := os.Lstat(paths[i])
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		perm := info.Mode().Perm()
		perm &^= 0o222
		if info.IsDir() {
			perm |= 0o555
		}
		if err := os.Chmod(paths[i], perm); err != nil {
			return err
		}
	}
	return nil
}

func makeTreeWritable(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		perm := info.Mode().Perm()
		if info.IsDir() {
			perm |= 0o700
		} else {
			perm |= 0o600
		}
		return os.Chmod(path, perm)
	})
}
