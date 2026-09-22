package relayclient

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

// A Stop hook resolves its binding from the session locator and then matches
// candidates by reading the recorded state. A binding that disappears in that
// window (for example a concurrent local unbind) is gone, not an error: the
// hook must stay inert instead of failing the harness Stop with a raw
// file-not-found error. A present but unreadable binding still fails closed.
func TestHookCandidatesTreatVanishedBindingAsInert(t *testing.T) {
	isolateCaller(t)
	dir := filepath.Join(t.TempDir(), ".pairroom", "rooms", "room", "slots", "slot1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "state.json")
	if candidates, err := boundHookCandidates([]string{missing}, model.RuntimeClaude, "session"); err != nil || len(candidates) != 0 {
		t.Fatalf("vanished binding was not inert: %+v %v", candidates, err)
	}
	if err := os.WriteFile(missing, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := boundHookCandidates([]string{missing}, model.RuntimeClaude, "session"); err == nil {
		t.Fatal("unreadable binding no longer fails closed")
	}
}
