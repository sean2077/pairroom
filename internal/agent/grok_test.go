package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestGrokCommandOmitsUnsetOverridesAndKeepsPromptOutOfArgv(t *testing.T) {
	adapter := NewGrok(Config{
		Actor:                  model.ActorClaude,
		Command:                "grok",
		Repo:                   "/repo",
		DataDir:                t.TempDir(),
		AdditionalInstructions: "Never mention secrets.",
	}, func(model.RuntimeEvent) {})
	args := adapter.buildArgs("/tmp/prompt.txt", "")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--prompt-file /tmp/prompt.txt") {
		t.Fatalf("expected prompt file: %s", joined)
	}
	for _, arg := range args {
		if arg == "-p" || arg == "--single" || strings.Contains(arg, "Never mention") {
			t.Fatalf("prompt/instructions leaked into argv: %s", joined)
		}
	}
	for _, flag := range []string{"--model", "--effort", "--yolo", "--resume", "--sandbox"} {
		if strings.Contains(joined, flag) {
			t.Fatalf("unset override %s was synthesized: %s", flag, joined)
		}
	}
}

func TestGrokCommandPassesExplicitOverrides(t *testing.T) {
	adapter := NewGrok(Config{
		Actor:          model.ActorCodex,
		Command:        "grok",
		Repo:           "/repo",
		Model:          "grok-4.6",
		Effort:         "high",
		PermissionMode: "auto",
		Sandbox:        "workspace-write",
	}, func(model.RuntimeEvent) {})
	args := adapter.buildArgs("/tmp/prompt.txt", "session-1")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--model grok-4.6", "--effort high", "--permission-mode auto", "--sandbox workspace", "--resume session-1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
}

func TestGrokPromptFileContainsInstructionsNotArgv(t *testing.T) {
	dir := t.TempDir()
	adapter := NewGrok(Config{
		Actor:                  model.ActorClaude,
		Command:                "grok",
		DataDir:                dir,
		AdditionalInstructions: "Prefer tests.",
		SystemPrompt:           "PAIRROOM-BOOTSTRAP",
	}, func(model.RuntimeEvent) {})
	path, err := adapter.writePromptFile("envelope body")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "PAIRROOM-BOOTSTRAP") || !strings.Contains(text, "Prefer tests.") || !strings.Contains(text, "envelope body") {
		t.Fatalf("prompt file missing expected sections: %s", text)
	}
	if filepath.Dir(path) == dir {
		t.Fatal("prompt file should be isolated under the slot runtime directory")
	}
}

func TestGrokFactoryEmitsConfiguredSlotActor(t *testing.T) {
	adapter := GrokFactory(Config{Actor: model.ActorCodex}, func(model.RuntimeEvent) {})
	if adapter.Actor() != model.ActorCodex {
		t.Fatalf("Grok factory actor = %s", adapter.Actor())
	}
}
