package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/version"
)

func TestAppendLoadAndSequence(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := model.NewEvent("room-1", "one", model.ActorUser, map[string]string{"v": "a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := model.NewEvent("room-1", "two", model.ActorClaude, map[string]string{"v": "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(&first); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(&second); err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("unexpected sequences: %d, %d", first.Seq, second.Seq)
	}

	events, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "one" || events[1].Kind != "two" {
		t.Fatalf("unexpected replay: %#v", events)
	}
}

func TestOpenExistingDoesNotCreateMissingStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	if _, err := OpenExisting(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExisting missing directory error=%v want os.ErrNotExist", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExisting recreated missing directory: %v", err)
	}

	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExisting(empty); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExisting missing event log error=%v want os.ErrNotExist", err)
	}
	for _, name := range []string{"events.jsonl", "metadata.json"} {
		if _, err := os.Lstat(filepath.Join(empty, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("OpenExisting created %s in an unpublished store: %v", name, err)
		}
	}
}

func TestOpenExistingReopensPublishedStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	created, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := model.NewEvent("room-1", "first", model.ActorUser, map[string]string{"ok": "yes"})
	if err := created.Append(&first); err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenExisting(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second, _ := model.NewEvent("room-1", "second", model.ActorCodex, map[string]string{"ok": "yes"})
	if err := reopened.Append(&second); err != nil {
		t.Fatal(err)
	}
	events, err := reopened.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "first" || events[1].Kind != "second" || events[1].Seq != 2 {
		t.Fatalf("unexpected OpenExisting replay: %#v", events)
	}
}

func TestOpenExistingRejectsMissingMetadataEvenForEmptyEventLog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExisting(dir); err == nil || !strings.Contains(err.Error(), "metadata is missing") {
		t.Fatalf("OpenExisting accepted an unmarked empty store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "metadata.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExisting created metadata while rejecting the store: %v", err)
	}
}

func TestOpenExistingRejectsSymlinkedStorePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated Windows privileges")
	}
	t.Parallel()

	targetDir := t.TempDir()
	created, err := Open(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(t.TempDir(), "store-link")
	if err := os.Symlink(targetDir, dirLink); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExisting(dirLink); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("OpenExisting symlinked directory error=%v want real-directory rejection", err)
	}

	eventDir := t.TempDir()
	eventTarget := filepath.Join(t.TempDir(), "events-target.jsonl")
	if err := os.WriteFile(eventTarget, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(eventTarget, filepath.Join(eventDir, "events.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExisting(eventDir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("OpenExisting symlinked event log error=%v want regular-file rejection", err)
	}
}

func TestLoadIgnoresOnlyPartialFinalLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	event, _ := model.NewEvent("room-1", "valid", model.ActorUser, map[string]string{"ok": "yes"})
	if err := store.Append(&event); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.OpenFile(store.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"seq":2,"kind":"partial"`); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events, err := reopened.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "valid" {
		t.Fatalf("partial final line should be ignored, got %#v", events)
	}
}

func TestReopenRepairsPartialTailBeforeNextAppend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	firstStore, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := model.NewEvent("room-1", "first", model.ActorUser, map[string]string{"ok": "yes"})
	if err := firstStore.Append(&first); err != nil {
		t.Fatal(err)
	}
	path := firstStore.Path()
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"seq":2,"kind":"partial"`); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	repaired, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer repaired.Close()
	second, _ := model.NewEvent("room-1", "second", model.ActorCodex, map[string]string{"ok": "yes"})
	if err := repaired.Append(&second); err != nil {
		t.Fatal(err)
	}
	events, err := repaired.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "first" || events[1].Kind != "second" || events[1].Seq != 2 {
		t.Fatalf("unexpected repaired replay: %#v", events)
	}
}

func TestOpenCreatesMetadata(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var metadata struct {
		Format        string `json:"format"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := store.LoadJSON("metadata.json", &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Format != "pairroom-jsonl" || metadata.SchemaVersion != version.StoreSchema {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
}

func TestSaveAndLoadJSON(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	input := map[string]any{"name": "pairroom", "enabled": true}
	if err := store.SaveJSON("metadata.json", input); err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := store.LoadJSON("metadata.json", &output); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(output)
	want, _ := json.Marshal(input)
	if string(got) != string(want) {
		t.Fatalf("metadata mismatch: got %s want %s", got, want)
	}
	if err := store.SaveJSON("../escape.json", input); err == nil {
		t.Fatal("expected traversal metadata name to fail")
	}
}

func TestOpenRejectsEveryNonCurrentSchemaWithoutMigration(t *testing.T) {
	for _, schema := range []int{1, version.StoreSchema - 1, version.StoreSchema + 1, 999} {
		t.Run(fmt.Sprintf("schema-%d", schema), func(t *testing.T) {
			dir := t.TempDir()
			metadata := fmt.Sprintf(`{"format":"pairroom-jsonl","schema_version":%d,"app_version":"0.1.0"}`, schema)
			if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(metadata), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "provides no migration") {
				t.Fatalf("non-current schema should be rejected without migration, got %v", err)
			}
		})
	}
}

func TestOpenRejectsOldSchemaBeforeRepairOrReplay(t *testing.T) {
	dir := t.TempDir()
	metadata := fmt.Sprintf(`{"format":"pairroom-jsonl","schema_version":%d}`, version.StoreSchema-1)
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	const brokenTail = `{"old":"event"`
	eventPath := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(eventPath, []byte(brokenTail), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "provides no migration") {
		t.Fatalf("old schema was not rejected before replay: %v", err)
	}
	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != brokenTail {
		t.Fatalf("old event log was repaired before schema rejection: %q", data)
	}
}
