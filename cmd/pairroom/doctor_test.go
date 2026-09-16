package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRetiredRelaySlotDirectoryCountDoesNotUseCanonicalState(t *testing.T) {
	repo := t.TempDir()
	for _, slot := range []string{"claude", "codex", "slot1"} {
		dir := filepath.Join(repo, ".pairroom", "rooms", "room", "slots", slot)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("not inspected"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := retiredRelaySlotDirectoryCount(repo); got != 2 {
		t.Fatalf("retired relay directory count = %d, want 2", got)
	}
}
