package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// The fixture is an excerpt of `claude --help` from Claude Code 2.1.283. It
// lists `--append-system-prompt <prompt>` as an option and mentions the file
// variant only as `--append-system-prompt[-file]`.
func TestClaudeUsesPromptFileWhenHelpUsesBracketNotation(t *testing.T) {
	help, err := filepath.Abs(filepath.Join("testdata", "claude-2.1.283-help-excerpt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("PAIRROOM_CLAUDE_SCRIPT", "default")
	t.Setenv("PAIRROOM_CLAUDE_SCRIPT_HELP_FILE", help)
	t.Setenv("PAIRROOM_HELPER_ARGS_FILE", argsFile)
	adapter := NewClaude(Config{Command: os.Args[0], Repo: t.TempDir(), DataDir: t.TempDir(), SystemPrompt: "pairroom system prompt marker"}, func(model.RuntimeEvent) {})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer stopWithin(adapter, 10*time.Second)
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	if !containsString(args, "--append-system-prompt-file") {
		t.Fatalf("prompt file transport not selected: %q", args)
	}
	for _, arg := range args {
		if arg == "--append-system-prompt" || strings.Contains(arg, "pairroom system prompt marker") {
			t.Fatalf("system prompt was passed through argv: %q", args)
		}
	}
}

func TestHelpAdvertisesBracketedFlagVariants(t *testing.T) {
	help := "  --append-system-prompt <prompt>\n  via: --system-prompt[-file],\n  --append-system-prompt[-file], --add-dir"
	for flag, want := range map[string]bool{
		"--append-system-prompt":      true,
		"--append-system-prompt-file": true,
		"--system-prompt-file":        true,
		"--add-dir":                   true,
		"--add-dir-file":              false,
		"--resume":                    false,
	} {
		if got := helpAdvertisesFlag(help, flag); got != want {
			t.Fatalf("helpAdvertisesFlag(%q)=%v, want %v", flag, got, want)
		}
	}
}

func TestWindowsCommandLineBoundRejectsArgvPrompt(t *testing.T) {
	prompt := strings.Repeat("p", 40000)
	args := []string{"-p", "--append-system-prompt", prompt}
	err := checkWindowsCommandLine("windows", "Claude Code", `claude.exe`, args)
	if err == nil || !strings.Contains(err.Error(), "32767") {
		t.Fatalf("over-long Windows command line accepted: %v", err)
	}
	if err := checkWindowsCommandLine("linux", "Claude Code", "claude", args); err != nil {
		t.Fatalf("non-Windows command line rejected: %v", err)
	}
	short := []string{"-p", "--append-system-prompt", strings.Repeat("p", 7000)}
	if err := checkWindowsCommandLine("windows", "Claude Code", `claude.exe`, short); err != nil {
		t.Fatalf("short command line rejected: %v", err)
	}
	if err := checkWindowsCommandLine("windows", "Claude Code", `claude.cmd`, []string{strings.Repeat("p", 9000)}); err == nil || !strings.Contains(err.Error(), "8191") {
		t.Fatalf("batch launcher limit not applied: %v", err)
	}
}
