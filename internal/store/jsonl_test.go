package store

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
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
	if metadata.Format != "pairroom-jsonl" || metadata.SchemaVersion != 1 {
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
