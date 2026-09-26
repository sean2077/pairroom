//go:build windows

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

const claudeHelpWithNameAndModel = "--input-format --output-format --session-id --resume --verbose --name --model --effort"

func startClaudeThroughShim(t *testing.T, cfg Config) ([]string, error) {
	t.Helper()
	argsFile := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("PAIRROOM_CLAUDE_SCRIPT", "default")
	t.Setenv("PAIRROOM_CLAUDE_SCRIPT_HELP", claudeHelpWithNameAndModel)
	t.Setenv("PAIRROOM_HELPER_ARGS_FILE", argsFile)
	cfg.Command = writeBatchShim(t, "claude")
	// The shim re-executes this test binary, which can be slow to start while
	// the rest of the module's tests run in parallel.
	saved := probeCommandTimeout
	probeCommandTimeout = 30 * time.Second
	t.Cleanup(func() { probeCommandTimeout = saved })
	adapter := NewClaude(cfg, func(model.RuntimeEvent) {})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	err := adapter.Start(ctx)
	defer stopWithin(adapter, 10*time.Second)
	var args []string
	if raw, readErr := os.ReadFile(argsFile); readErr == nil {
		_ = json.Unmarshal(raw, &args)
	}
	return args, err
}

func TestClaudeRoomNameCannotInjectThroughBatchShim(t *testing.T) {
	repo := t.TempDir()
	cfg := Config{
		Actor: model.ActorSlot1, Runtime: model.RuntimeClaude, PeerRuntime: model.RuntimeCodex,
		RoomID: "room-0123456789abcdef01234567", RoomName: `x" & echo.>pairroom-injected & echo "`,
		Repo: repo, DataDir: t.TempDir(),
	}
	args, err := startClaudeThroughShim(t, cfg)
	if err != nil {
		t.Fatalf("a Room name is display metadata and must not prevent startup: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(repo, "pairroom-injected")); statErr == nil {
		t.Fatal("Room name ran a command through the .cmd shim")
	}
	if !containsString(args, "--input-format") {
		t.Fatalf("cmd.exe truncated the command line: %q", args)
	}
	named := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "--name=") {
			named = strings.Contains(arg, "echo.＞pairroom-injected") && strings.Contains(arg, "＆")
		}
	}
	if !named {
		t.Fatalf("session name did not arrive as one neutralized argument: %q", args)
	}
}

func TestClaudeModelWithCmdMetacharacterFailsClosedThroughBatchShim(t *testing.T) {
	repo := t.TempDir()
	_, err := startClaudeThroughShim(t, Config{Model: "opus&echo.>pairroom-injected", Repo: repo, DataDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "batch file") {
		t.Fatalf("model with a cmd.exe metacharacter was not rejected: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(repo, "pairroom-injected")); statErr == nil {
		t.Fatal("model value ran a command through the .cmd shim")
	}
}

func TestCodexProviderArgumentWithCmdMetacharacterFailsClosedThroughBatchShim(t *testing.T) {
	t.Setenv("PAIRROOM_CODEX_HELPER", "1")
	repo := t.TempDir()
	adapter := NewCodex(Config{
		Command: writeBatchShim(t, "codex"), Repo: repo,
		CommandArgs: []string{"-c", `model_providers.p.base_url="https://proxy.invalid/v1?a=1&b=2"`},
	}, func(model.RuntimeEvent) {})
	defer stopWithin(adapter, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	err := adapter.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "batch file") {
		t.Fatalf("CC Switch argument with a cmd.exe metacharacter was not rejected: %v", err)
	}
}

func TestClaudeArgvSystemPromptOverWindowsLimitFailsClearly(t *testing.T) {
	t.Setenv("PAIRROOM_CLAUDE_SCRIPT", "default")
	t.Setenv("PAIRROOM_CLAUDE_SCRIPT_HELP", "--input-format --output-format --session-id --resume --verbose --append-system-prompt")
	adapter := NewClaude(Config{Command: os.Args[0], Repo: t.TempDir(), DataDir: t.TempDir(), SystemPrompt: strings.Repeat("p", 40000)}, func(model.RuntimeEvent) {})
	defer stopWithin(adapter, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	err := adapter.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "--append-system-prompt-file") || !strings.Contains(err.Error(), "Windows limit") {
		t.Fatalf("over-long argv prompt did not fail with a clear error: %v", err)
	}
}
