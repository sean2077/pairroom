package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func submoduleGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	args = append([]string{"-c", "user.name=Fixture", "-c", "user.email=test@example.invalid"}, args...)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
}

func submoduleFixture(t *testing.T) (string, string) {
	t.Helper()
	root, origin := fixture(t), fixture(t)
	// A space in the path checks that status metadata is not parsed as a
	// whitespace-delimited filename. The transport is local to the fixture.
	const name = "sub module"
	submoduleGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "--name", "child", origin, name)
	submoduleGit(t, root, "commit", "-qam", "add child")
	return root, filepath.Join(root, name)
}

func TestAnchorRejectsDirtySubmoduleEvidence(t *testing.T) {
	for _, ignore := range []string{"none", "untracked", "dirty", "all"} {
		for _, change := range []string{"tracked", "staged", "untracked"} {
			t.Run(ignore+"/"+change, func(t *testing.T) {
				root, child := submoduleFixture(t)
				submoduleGit(t, root, "config", "submodule.child.ignore", ignore)
				submoduleGit(t, root, "config", "diff.ignoreSubmodules", "all")
				ctx := context.Background()
				clean, err := Capture(ctx, root, "")
				if err != nil {
					t.Fatal(err)
				}
				name := "tracked.txt"
				if change == "untracked" {
					name = "new.txt"
				}
				for _, body := range []string{"first change\n", "different contents\n"} {
					if err := os.WriteFile(filepath.Join(child, name), []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
					if change == "staged" {
						submoduleGit(t, child, "add", name)
					}
					if _, err := Capture(ctx, root, ""); !errors.Is(err, ErrUnavailable) {
						t.Fatalf("dirty submodule received a lossy anchor: %v", err)
					}
					if got := Check(ctx, root, clean); got != "unverified" {
						t.Fatalf("dirty submodule comparison = %q; want unverified", got)
					}
				}
			})
		}
	}
}

func TestAnchorTracksSubmoduleCommitsDespiteIgnoreConfiguration(t *testing.T) {
	root, child := submoduleFixture(t)
	submoduleGit(t, root, "config", "submodule.child.ignore", "all")
	submoduleGit(t, root, "config", "diff.ignoreSubmodules", "all")
	submoduleGit(t, root, "config", "diff.submodule", "log")
	ctx := context.Background()
	clean, err := Capture(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := Check(ctx, root, clean); got != "unchanged_observation" {
		t.Fatalf("clean submodule comparison = %q", got)
	}
	if err := os.WriteFile(filepath.Join(child, "tracked.txt"), []byte("committed change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	submoduleGit(t, child, "commit", "-qam", "child change")
	if got := Check(ctx, root, clean); got != "stale" {
		t.Fatalf("unrecorded submodule commit was hidden: %q", got)
	}
	changed, err := Capture(ctx, root, "")
	if err != nil {
		t.Fatalf("clean changed submodule commit must remain capturable: %v", err)
	}
	submoduleGit(t, root, "add", "sub module")
	if got := Check(ctx, root, changed); got != "stale" {
		t.Fatalf("staged gitlink difference was hidden: %q", got)
	}
}

func TestAnchorStatusDoesNotParseFilenameAsSubmoduleMetadata(t *testing.T) {
	root := fixture(t)
	const name = "1 .M S.M. not-a-status-record"
	if err := os.WriteFile(filepath.Join(root, name), []byte("ordinary file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(context.Background(), root, ""); err != nil {
		t.Fatalf("untracked filename was parsed as status metadata: %v", err)
	}
	submoduleGit(t, root, "add", name)
	submoduleGit(t, root, "commit", "-qm", "add ordinary file")
	submoduleGit(t, root, "mv", name, "renamed.txt")
	if _, err := Capture(context.Background(), root, ""); err != nil {
		t.Fatalf("rename source was parsed as a separate status record: %v", err)
	}
}
