package relayclient

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

// ownedMarker is PairRoom-owned content (it carries a whitelisted heading) with
// a sentinel that must survive install: if PairRoom ever wrote through an
// external installer's symlink, the sentinel would be replaced by skillContent.
const ownedMarker = "# PairRoom relay\nDO_NOT_OVERWRITE_SENTINEL\n"

func writeSkill(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestInstallSkillLeafAcceptsExternalSymlinkWithoutWriting is the regression for
// `relay install` failing with "relay state path must not contain symlinks" when
// a skill installer (cc-switch, npx skills) already symlinked pairroom-relay into
// the host skills dir. The symlinked, PairRoom-owned skill is recognized as
// installed and its target is never written through.
func TestInstallSkillLeafAcceptsExternalSymlinkWithoutWriting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated Windows privileges")
	}
	parent := t.TempDir()
	target := t.TempDir()
	writeSkill(t, target, ownedMarker)
	link := filepath.Join(parent, "pairroom-relay")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := installSkillLeaf(parent, "pairroom-relay"); err != nil {
		t.Fatalf("symlinked PairRoom-owned skill rejected: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "DO_NOT_OVERWRITE_SENTINEL") {
		t.Fatalf("install wrote through the external symlink: %q", data)
	}
}

func TestInstallSkillLeafSymlinkEdgeCasesFailClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated Windows privileges")
	}

	t.Run("unrelated content", func(t *testing.T) {
		parent := t.TempDir()
		target := t.TempDir()
		writeSkill(t, target, "# some other skill\n")
		link := filepath.Join(parent, "pairroom-relay")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		err := installSkillLeaf(parent, "pairroom-relay")
		if err == nil || !strings.Contains(err.Error(), "unrelated pairroom-relay skill") {
			t.Fatalf("unrelated symlinked skill accepted: %v", err)
		}
	})

	t.Run("broken symlink", func(t *testing.T) {
		parent := t.TempDir()
		link := filepath.Join(parent, "pairroom-relay")
		if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), link); err != nil {
			t.Fatal(err)
		}
		err := installSkillLeaf(parent, "pairroom-relay")
		if err == nil || !strings.Contains(err.Error(), "broken symlink") {
			t.Fatalf("dangling symlink accepted: %v", err)
		}
	})

	t.Run("symlink to file", func(t *testing.T) {
		parent := t.TempDir()
		fileTarget := filepath.Join(t.TempDir(), "skill-file")
		if err := os.WriteFile(fileTarget, []byte(ownedMarker), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(parent, "pairroom-relay")
		if err := os.Symlink(fileTarget, link); err != nil {
			t.Fatal(err)
		}
		err := installSkillLeaf(parent, "pairroom-relay")
		if err == nil || !strings.Contains(err.Error(), "does not resolve to a directory") {
			t.Fatalf("symlink to file accepted: %v", err)
		}
	})

	t.Run("symlink without SKILL.md", func(t *testing.T) {
		parent := t.TempDir()
		target := t.TempDir()
		link := filepath.Join(parent, "pairroom-relay")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		err := installSkillLeaf(parent, "pairroom-relay")
		if err == nil || !strings.Contains(err.Error(), "no SKILL.md") {
			t.Fatalf("symlink to empty dir accepted: %v", err)
		}
	})
}

// TestInstallSkillLeafRealDirectory covers the non-symlink paths that must keep
// working: a fresh leaf is created and written, an owned leaf is upgraded, and a
// non-directory leaf fails closed.
func TestInstallSkillLeafRealDirectory(t *testing.T) {
	t.Run("fresh", func(t *testing.T) {
		parent := t.TempDir()
		if err := installSkillLeaf(parent, "pairroom-relay"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(parent, "pairroom-relay", "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != skillContent {
			t.Fatal("fresh leaf did not write the bundled skill")
		}
	})

	t.Run("upgrade owned", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "pairroom-relay")
		writeSkill(t, dir, ownedMarker)
		if err := installSkillLeaf(parent, "pairroom-relay"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != skillContent {
			t.Fatal("owned real leaf was not upgraded to the bundled skill")
		}
	})

	t.Run("refuse unrelated", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "pairroom-relay")
		writeSkill(t, dir, "# unrelated\n")
		err := installSkillLeaf(parent, "pairroom-relay")
		if err == nil || !strings.Contains(err.Error(), "unrelated pairroom-relay skill") {
			t.Fatalf("unrelated real skill overwritten: %v", err)
		}
	})

	t.Run("refuse non-directory leaf", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.WriteFile(filepath.Join(parent, "pairroom-relay"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := installSkillLeaf(parent, "pairroom-relay")
		if err == nil || !strings.Contains(err.Error(), "must be a directory") {
			t.Fatalf("regular-file leaf accepted: %v", err)
		}
	})
}

// TestInstallSkillSucceedsWithSymlinkedLeaf exercises the full installSkill flow
// (strict parents plus tolerant leaf) the way `relay install` invokes it.
func TestInstallSkillSucceedsWithSymlinkedLeaf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated Windows privileges")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	target := t.TempDir()
	writeSkill(t, target, ownedMarker)
	skills := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(skills, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(skills, "pairroom-relay")); err != nil {
		t.Fatal(err)
	}

	if err := installSkill(model.RuntimeClaude); err != nil {
		t.Fatalf("installSkill rejected an externally symlinked skill: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "DO_NOT_OVERWRITE_SENTINEL") {
		t.Fatalf("installSkill wrote through the symlink: %q", data)
	}
}
