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
	if err := runInstall(root, []model.RuntimeKind{model.RuntimeGrok}, strings.NewReader(""), &out, io.Discard); err != nil {
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
	if err := runInstall(root, []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok, model.RuntimeClaude}, strings.NewReader(""), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, ".claude", "settings.json"),
		filepath.Join(root, ".codex", "hooks.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".grok", "hooks", "pairroom.json")); err == nil {
		t.Fatal("claude+grok install wrote a second Grok hook file")
	}
	if !strings.Contains(out.String(), `"installed":["claude","codex","grok"]`) || !strings.Contains(out.String(), `"hooks_skipped":["grok"]`) {
		t.Fatalf("installed list wrong (dedup/order/skip): %s", out.String())
	}
}

func TestRunInstallGrokSkipsHooksWhenClaudeAlreadyPresent(t *testing.T) {
	IsolateNativeCaller(t)
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := editHooks(root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInstall(root, []model.RuntimeKind{model.RuntimeGrok}, strings.NewReader(""), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".grok", "hooks", "pairroom.json")); err == nil {
		t.Fatal("grok install wrote hooks despite an existing Claude PairRoom hook")
	}
	if !strings.Contains(out.String(), `"hooks_skipped":["grok"]`) {
		t.Fatalf("missing skip: %s", out.String())
	}
	if err := installed(root, model.RuntimeGrok); err != nil {
		t.Fatalf("shared Claude hook should satisfy Grok bind: %v", err)
	}
}

func TestRunInstallWritesGrokHooksWhenClaudeCompatDisabled(t *testing.T) {
	IsolateNativeCaller(t)
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GROK_CLAUDE_HOOKS_ENABLED", "false")
	if err := editHooks(root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInstall(root, []model.RuntimeKind{model.RuntimeGrok}, strings.NewReader(""), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".grok", "hooks", "pairroom.json")); err != nil {
		t.Fatalf("compat-off grok install must write its own hooks: %v", err)
	}
	if strings.Contains(out.String(), `"hooks_skipped"`) {
		t.Fatalf("skipped grok hooks while compatibility was off: %s", out.String())
	}
}

func TestRunInstallClaudeLeavesRedundantGrokHooksWithoutTTY(t *testing.T) {
	IsolateNativeCaller(t)
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := editHooks(root, model.RuntimeGrok, false); err != nil {
		t.Fatal(err)
	}
	var out, diagnostic bytes.Buffer
	if err := runInstall(root, []model.RuntimeKind{model.RuntimeClaude}, strings.NewReader("y\n"), &out, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".grok", "hooks", "pairroom.json")); err != nil {
		t.Fatal("non-TTY install removed Grok hooks without confirmation")
	}
	if !strings.Contains(diagnostic.String(), "second Grok hook file") && !strings.Contains(diagnostic.String(), "reuses Claude Code") {
		t.Fatalf("missing cleanup guidance: %s", diagnostic.String())
	}
	if strings.Contains(out.String(), `"hooks_removed"`) {
		t.Fatalf("non-TTY claimed a removal: %s", out.String())
	}
}

func TestPromptRemoveRedundantGrokHooksAcceptsYes(t *testing.T) {
	if !promptRemoveRedundantGrokHooks(strings.NewReader("yes\n"), io.Discard) {
		t.Fatal("yes should confirm removal")
	}
	if promptRemoveRedundantGrokHooks(strings.NewReader("n\n"), io.Discard) {
		t.Fatal("n should keep Grok hooks")
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
