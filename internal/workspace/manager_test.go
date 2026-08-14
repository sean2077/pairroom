package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRefreshIncludesDirtyAndUntrackedState(t *testing.T) {
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new", "untracked.txt"), []byte("new\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	manager, err := New(repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Cleanup(context.Background()) })
	boundary, err := manager.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if boundary.Kind != "reviewer-snapshot" || boundary.SourceHead == "" || boundary.PatchSHA256 == "" {
		t.Fatalf("unexpected boundary: %+v", boundary)
	}
	if !boundary.Dirty || boundary.UntrackedCount != 1 || !boundary.ReadOnly {
		t.Fatalf("dirty metadata mismatch: %+v", boundary)
	}
	assertFile(t, filepath.Join(boundary.Path, "tracked.txt"), "dirty\n")
	assertFile(t, filepath.Join(boundary.Path, "new", "untracked.txt"), "new\n")
	assertFile(t, filepath.Join(repo, "tracked.txt"), "dirty\n")
	if runtime.GOOS != "windows" && !boundary.ReadOnlyEnforced {
		t.Fatalf("expected POSIX read-only enforcement: %+v", boundary)
	}
}

func TestRefreshRejectsUntrackedSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated Windows privileges")
	}
	repo := initRepo(t)
	if err := os.Symlink("tracked.txt", filepath.Join(repo, "link.txt")); err != nil {
		t.Fatal(err)
	}
	manager, err := New(repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "untracked symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestRefreshReplacesPreviousSnapshot(t *testing.T) {
	repo := initRepo(t)
	manager, err := New(repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Cleanup(context.Background()) })
	first, err := manager.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Path != second.Path || first.PatchSHA256 == second.PatchSHA256 {
		t.Fatalf("snapshot was not deterministically replaced: first=%+v second=%+v", first, second)
	}
	assertFile(t, second.Path+string(filepath.Separator)+"tracked.txt", "second\n")
}

func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	run(t, repo, "git", "config", "core.autocrlf", "false")
	run(t, repo, "git", "config", "user.name", "PairRoom Test")
	run(t, repo, "git", "config", "user.email", "pairroom@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", "tracked.txt")
	run(t, repo, "git", "commit", "-q", "-m", "base")
	return repo
}

func run(t *testing.T, dir, command string, args ...string) {
	t.Helper()
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", command, args, err, out)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
