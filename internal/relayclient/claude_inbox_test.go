package relayclient

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sean2077/pairroom/internal/claudewake"
	"github.com/sean2077/pairroom/internal/model"
)

func TestClaudeInboxCaptureDoesNotReuseOtherRuntimeEnvironment(t *testing.T) {
	IsolateNativeCaller(t)
	address := "/tmp/test-inbox.sock"
	if runtime.GOOS == "windows" {
		address = `\\.\pipe\pairroom-unit-inbox`
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", address)
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "private-token")
	dir := t.TempDir()
	state := State{Runtime: model.RuntimeClaude, BindID: "b", Generation: 1, SessionID: "s"}
	id := claudewake.Identity{BindID: "b", Generation: 1, SessionID: "s"}
	if err := captureClaudeInbox(dir, state); err != nil {
		t.Fatal(err)
	}
	if _, err := claudewake.Prepare(dir, id); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []model.RuntimeKind{model.RuntimeGrok, model.RuntimeCodex} {
		state.Runtime = kind
		if err := captureClaudeInbox(dir, state); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, claudewake.FileName)); !os.IsNotExist(err) {
			t.Fatal("non-Claude inherited capability")
		}
	}
	state.Runtime = model.RuntimeClaude
	if err := captureClaudeInbox(dir, state); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "")
	if err := captureClaudeInbox(dir, state); err != nil {
		t.Fatal(err)
	}
	if _, err := claudewake.Prepare(dir, id); err == nil {
		t.Fatal("stale environment retained")
	}
}

func TestClaudeInboxCrashTempCleanupKeepsCommittedCapability(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".claude-inbox-crash", claudewake.FileName, "unrelated"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cleanupAtomicTemps(dir)
	if _, err := os.Stat(filepath.Join(dir, ".claude-inbox-crash")); !os.IsNotExist(err) {
		t.Fatal("crash secret retained")
	}
	for _, name := range []string{claudewake.FileName, "unrelated"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal("cleanup removed committed or unrelated file")
		}
	}
}
