package relayclient

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestParseRuntimeListMultiAliasAndGrok(t *testing.T) {
	got, err := parseRuntimeList("cc, codex ,grok,claude")
	if err != nil {
		t.Fatal(err)
	}
	want := []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v (dedup/order)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if _, err := parseRuntimeList("nope"); err == nil || !strings.Contains(err.Error(), "unknown --runtime") {
		t.Fatalf("invalid runtime accepted: %v", err)
	}
	if _, err := parseRuntimeList(" , "); err == nil {
		t.Fatal("empty selection accepted")
	}
}

func TestRunInstallGrokUsesOwnHooks(t *testing.T) {
	IsolateNativeCaller(t)
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var out bytes.Buffer
	if err := runInstall(root, []model.RuntimeKind{model.RuntimeGrok}, &out); err != nil {
		t.Fatal(err)
	}
	// Grok is a real native runtime with its own hook path, not Claude Code's.
	if _, err := os.Stat(filepath.Join(root, ".grok", "hooks", "pairroom.json")); err != nil {
		t.Fatalf("grok did not install its own hooks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "settings.json")); err == nil {
		t.Fatal("grok must not write Claude Code's hook file")
	}
	if !strings.Contains(out.String(), `"installed":["grok"]`) {
		t.Fatalf("installed list wrong: %s", out.String())
	}
}

func TestRunInstallMultipleRuntimesDedupesAndKeepsOrder(t *testing.T) {
	IsolateNativeCaller(t)
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var out bytes.Buffer
	if err := runInstall(root, []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok, model.RuntimeClaude}, &out); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, ".claude", "settings.json"),
		filepath.Join(root, ".codex", "hooks.json"),
		filepath.Join(root, ".grok", "hooks", "pairroom.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	if !strings.Contains(out.String(), `"installed":["claude","codex","grok"]`) {
		t.Fatalf("installed list wrong (dedup/order): %s", out.String())
	}
}

func TestSelectInstallRuntimesNonTTYRequiresFlag(t *testing.T) {
	IsolateNativeCaller(t)
	// A bytes/strings reader is not a terminal, so a missing --runtime must fail
	// with the valid options rather than block an agent tool call.
	if _, err := selectInstallRuntimes("", strings.NewReader(""), io.Discard); err == nil || !strings.Contains(err.Error(), "--runtime") {
		t.Fatalf("non-interactive install without --runtime did not fail clearly: %v", err)
	}
	got, err := selectInstallRuntimes("codex,cc", strings.NewReader(""), io.Discard)
	if err != nil || len(got) != 2 || got[0] != model.RuntimeCodex || got[1] != model.RuntimeClaude {
		t.Fatalf("explicit list: %v %v", got, err)
	}
}

func TestPromptInstallRuntimesParsesNumbersAndNames(t *testing.T) {
	got, err := promptInstallRuntimes(strings.NewReader("1, cc\n"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != model.RuntimeCodex || got[1] != model.RuntimeClaude {
		t.Fatalf("parsed %v", got)
	}
	if _, err := promptInstallRuntimes(strings.NewReader("   \n"), io.Discard); err == nil {
		t.Fatal("empty selection accepted")
	}
	if _, err := promptInstallRuntimes(strings.NewReader("9\n"), io.Discard); err == nil {
		t.Fatal("out-of-range selection accepted")
	}
}
