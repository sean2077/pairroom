package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		if v, e := c.CombinedOutput(); e != nil {
			t.Fatalf("git: %s %v", v, e)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "tracked.txt")
	run("-c", "user.name=Fixture", "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture")
	return dir
}
func TestAnchorTracksCommittedStagedUnstagedAndNewEvidence(t *testing.T) {
	dir := fixture(t)
	ctx := context.Background()
	a, err := Capture(ctx, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if Check(ctx, dir, a) != "unchanged_observation" {
		t.Fatal("clean observation changed")
	}
	path := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(path, []byte("edited\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if Check(ctx, dir, a) != "stale" {
		t.Fatal("unstaged edit ignored")
	}
	b, err := Capture(ctx, dir, a.Head)
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("git", "add", "tracked.txt")
	c.Dir = dir
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	if Check(ctx, dir, b) != "stale" {
		t.Fatal("staging difference ignored")
	}
	d, err := Capture(ctx, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if Check(ctx, dir, d) != "stale" {
		t.Fatal("new file ignored")
	}
	if strings.Contains(a.Envelope(), "original") {
		t.Fatal("file content leaked")
	}
	if Check(ctx, fixture(t), a) == "unchanged_observation" {
		t.Fatal("different checkout accepted")
	}
	if _, err := Capture(ctx, dir, "--evil"); err == nil {
		t.Fatal("flag-like ref accepted")
	}
}
func TestAnchorBoundsAndDoesNotReadSymlinkTargets(t *testing.T) {
	dir := fixture(t)
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("secret-one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link")); err == nil {
		a, err := Capture(context.Background(), dir, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("secret-two"), 0600); err != nil {
			t.Fatal(err)
		}
		if Check(context.Background(), dir, a) != "unchanged_observation" {
			t.Fatal("followed external symlink")
		}
	}
	file, err := os.Create(filepath.Join(dir, "oversized"))
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(maxEvidence + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := Capture(context.Background(), dir, ""); err == nil {
		t.Fatal("unbounded evidence read")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Capture(ctx, dir, ""); err == nil {
		t.Fatal("ignored cancellation")
	}
}
