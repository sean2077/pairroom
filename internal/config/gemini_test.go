package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestGeminiTemplatesAndSameRuntimeSlots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairroom.json")
	body := `{"runtimes":{"gemini":{"command":"/tools/gemini","args":[]}},"claude":{"runtime":"gemini","permission_mode":"","sandbox":""},"codex":{"runtime":"gemini","permission_mode":"plan","sandbox":"on"}}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtimes.For(model.RuntimeGemini).Command != "/tools/gemini" || cfg.Claude.RuntimeKind(model.ActorSlot1) != model.RuntimeGemini || cfg.Codex.RuntimeKind(model.ActorSlot2) != model.RuntimeGemini {
		t.Fatalf("Gemini configuration lost: %+v", cfg)
	}
	if cfg.Claude.PermissionMode != "" || cfg.Claude.Sandbox != "" || cfg.Codex.ApprovalPolicy != "" || cfg.Codex.Sandbox != "on" {
		t.Fatal("runtime change inherited another vendor's defaults")
	}
	for _, arg := range []string{"--acp", "--experimental-acp", "--model=other", "-m", "--approval-mode=yolo", "--sandbox=false", "--no-sandbox", "--no-yolo", "--no-acp", "--no-experimental-acp", "--session-file=saved.json", "--session-id=other", "--delete-session=1", "--list-sessions", "--worktree=other", "-w", "-o=json", "--resume", "--prompt=task"} {
		cfg := Defaults()
		cfg.Runtimes.Gemini.Args = []string{arg}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("global template bypassed Gemini selection policy: %s", arg)
		}
	}
}
