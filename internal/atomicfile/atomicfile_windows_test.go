//go:build windows

package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func writeFixture(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A plain os.Open handle omits FILE_SHARE_DELETE, as older CLIs, antivirus and
// indexers do. Replace must wait it out instead of failing the save.
func TestReplaceRetriesWhileForeignReaderHoldsTarget(t *testing.T) {
	dir := t.TempDir()
	path, tmp := filepath.Join(dir, "state.json"), filepath.Join(dir, ".state.json-1")
	writeFixture(t, path, "old")
	writeFixture(t, tmp, "new")
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := os.Rename(tmp, path); !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Fatalf("precondition: plain rename over a held file = %v, want access denied", err)
	}
	sleeps := 0
	sleep = func(time.Duration) {
		sleeps++
		if sleeps == 2 {
			_ = reader.Close() // the foreign reader finishes during backoff
		}
	}
	t.Cleanup(func() { sleep = time.Sleep })
	if err := Replace(tmp, path); err != nil {
		t.Fatalf("Replace with a transient foreign reader: %v", err)
	}
	if sleeps != 2 {
		t.Fatalf("Replace slept %d times, want 2 (retry until the reader closed)", sleeps)
	}
	if got := readText(t, path); got != "new" {
		t.Fatalf("target = %q, want new", got)
	}
	if _, err := os.Lstat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary still present after replace: %v", err)
	}
}

func TestReplaceGivesUpAfterBoundedBudget(t *testing.T) {
	dir := t.TempDir()
	path, tmp := filepath.Join(dir, "state.json"), filepath.Join(dir, ".state.json-1")
	writeFixture(t, path, "old")
	writeFixture(t, tmp, "new")
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var waited time.Duration
	sleep = func(d time.Duration) { waited += d; time.Sleep(d) }
	t.Cleanup(func() { sleep = time.Sleep })
	start := time.Now()
	err = Replace(tmp, path)
	if !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Fatalf("Replace under a persistent foreign reader = %v, want access denied", err)
	}
	if elapsed := time.Since(start); elapsed > 3*replaceBudget {
		t.Fatalf("Replace blocked %v, want about %v", elapsed, replaceBudget)
	}
	if waited == 0 {
		t.Fatal("Replace did not retry before giving up")
	}
	if got := readText(t, path); got != "old" {
		t.Fatalf("target = %q after failed replace, want old", got)
	}
}

// PairRoom's own readers must never be the reason a save fails: Replace
// succeeds on the first attempt while an Open handle is live, and that handle
// keeps reading the file it opened.
func TestOpenDoesNotBlockReplace(t *testing.T) {
	dir := t.TempDir()
	path, tmp := filepath.Join(dir, "state.json"), filepath.Join(dir, ".state.json-1")
	writeFixture(t, path, "old")
	writeFixture(t, tmp, "new")
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	sleep = func(time.Duration) { t.Fatal("Replace retried behind a shared-delete reader") }
	t.Cleanup(func() { sleep = time.Sleep })
	if err := Replace(tmp, path); err != nil {
		t.Fatalf("Replace with an Open reader: %v", err)
	}
	buf := make([]byte, 8)
	n, err := reader.Read(buf)
	if err != nil || string(buf[:n]) != "old" {
		t.Fatalf("open handle read %q, %v; want old", buf[:n], err)
	}
	if got := readText(t, path); got != "new" {
		t.Fatalf("target = %q, want new", got)
	}
}

func TestOpenMissingFileIsNotExist(t *testing.T) {
	_, err := ReadFile(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadFile(missing) = %v, want os.ErrNotExist", err)
	}
}
