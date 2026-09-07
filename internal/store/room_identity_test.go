package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestPublishedRoomIdentityPrecedesTailRepair(t *testing.T) {
	for _, name := range []string{"empty", "partial-only", "wrong-envelope", "wrong-payload", "missing-created", "duplicate-created", "mixed-room"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			initial, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := initial.Close(); err != nil {
				t.Fatal(err)
			}
			created := model.Event{Seq: 1, RoomID: "expected", Kind: "room.created", Data: json.RawMessage(`{"id":"expected"}`)}
			events := []model.Event{created}
			switch name {
			case "empty", "partial-only":
				events = nil
			case "wrong-envelope":
				events[0].RoomID = "other"
			case "wrong-payload":
				events[0].Data = json.RawMessage(`{"id":"other"}`)
			case "missing-created":
				events[0].Kind = "settings.updated"
			case "duplicate-created":
				next := created
				next.Seq = 2
				events = append(events, next)
			case "mixed-room":
				events = append(events, model.Event{Seq: 2, RoomID: "other", Kind: "settings.updated", Data: json.RawMessage(`{}`)})
			}
			var before []byte
			for _, event := range events {
				encoded, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				before = append(before, encoded...)
				before = append(before, '\n')
			}
			if name != "empty" {
				before = append(before, []byte(`{"seq":`)...)
			}
			path := filepath.Join(dir, "events.jsonl")
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			opened, err := OpenExistingForRoom(dir, "expected")
			if opened != nil {
				_ = opened.Close()
			}
			if err == nil {
				t.Error("ambiguous published identity accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("rejected identity mutated its Event Log")
			}
		})
	}
}

func TestPublishedRoomWriterRepairsOwnedTailAndRejectsForeignAppend(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := model.NewEvent("expected", "room.created", model.ActorSystem, model.RoomMeta{ID: "expected"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&created); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	damaged := append(append([]byte(nil), good...), []byte(`{"seq":`)...)
	if err := os.WriteFile(path, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	s, err = OpenExistingForRoom(dir, "expected")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, good) {
		t.Fatalf("owned tail not repaired: %v", err)
	}
	wrong, _ := model.NewEvent("other", "test", model.ActorSystem, nil)
	if err := s.Append(&wrong); err == nil || wrong.Seq != 0 {
		t.Fatal("foreign append accepted or consumed sequence")
	}
	next, _ := model.NewEvent("expected", "test", model.ActorSystem, nil)
	if err := s.Append(&next); err != nil || next.Seq != 2 {
		t.Fatalf("valid append rejected: %v", err)
	}
	if _, err := OpenExistingForRoom(filepath.Join(dir, "absent"), ""); err == nil {
		t.Fatal("empty expected identity accepted")
	}
}
