//go:build windows

package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Both slots of a Room share a workspace, so another process may be reading a
// slot's state.json (without FILE_SHARE_DELETE) at the moment it is saved. The
// save must survive a transient reader instead of losing the Stop reply WAL.
func TestAtomicJSONSurvivesTransientForeignReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := AtomicJSON(path, map[string]int{"seq": 1}); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond) // well inside the one-second replace budget
		_ = reader.Close()
		close(released)
	}()
	if err := AtomicJSON(path, map[string]int{"seq": 2}); err != nil {
		t.Fatalf("AtomicJSON while a reader held the target: %v", err)
	}
	<-released
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"seq": 2`) {
		t.Fatalf("state after replace = %q, %v; want seq 2", data, err)
	}
	assertNoTemporaries(t, dir)
}

func TestAtomicJSONFailureRemovesTemporary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := AtomicJSON(path, map[string]int{"seq": 1}); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := AtomicJSON(path, map[string]int{"seq": 2}); err == nil {
		t.Fatal("AtomicJSON succeeded while a non-sharing reader stayed open")
	}
	_ = reader.Close()
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"seq": 1`) {
		t.Fatalf("state after failed replace = %q, %v; want unchanged seq 1", data, err)
	}
	assertNoTemporaries(t, dir)
}

func assertNoTemporaries(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("temporary %s left behind", entry.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("directory has %d entries, want only the state file", len(entries))
	}
}
