package projectsetup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectSetupGlobalOverrideThroughAncestorAlias(t *testing.T) {
	// Reproduce macOS /var -> /private/var on every platform with symlinks.
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.MkdirAll(filepath.Join(real, "project"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, path := range []string{real, alias} {
		root := filepath.Join(path, "project")
		t.Run(filepath.Base(path), func(t *testing.T) {
			// The missing global directory must still resolve through its parent.
			t.Setenv("CODEX_HOME", filepath.Join(alias, "project", ".codex"))
			item := setupItem(t, root, "codex", "hook")
			if item.State != "blocked" || item.CanInstall || item.Reason != "global_path" {
				t.Fatalf("global override alias accepted: %+v", item)
			}
			if _, err := Install(root, Request{Runtime: "codex", Component: "hook", Revision: item.Revision, ConfirmReplace: true}); err == nil {
				t.Fatal("global alias was installed")
			}
			if _, err := os.Stat(filepath.Join(real, "project", ".codex")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("global directory was created")
			}
			// Aliases of an unrelated missing global root must not block a project.
			t.Setenv("CODEX_HOME", filepath.Join(alias, "other", ".codex"))
			if item := setupItem(t, root, "codex", "hook"); item.State != "missing" || !item.CanInstall {
				t.Fatalf("unrelated global directory blocks installation: %+v", item)
			}
		})
	}
}

func TestProjectSetupDanglingGlobalAliasFailsClosed(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "global")
	if err := os.Symlink(filepath.Join(root, ".codex"), alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("CODEX_HOME", alias)
	item := setupItem(t, root, "codex", "hook")
	if item.State != "blocked" || item.CanInstall {
		t.Fatalf("dangling global alias accepted: %+v", item)
	}
	if _, err := os.Stat(filepath.Join(root, ".codex")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inspection created global target")
	}
}
