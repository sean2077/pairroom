package room

import (
	"context"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func TestRuntimeNamingTracksDurableRoomAfterPermissionsAndRename(t *testing.T) {
	e, captures := newCollaborationEngine(t, "")
	before := e.SnapshotMeta()
	for _, actor := range model.SlotActors() {
		cfg := captures.latest(actor)
		if cfg.RoomID != before.ID || cfg.RoomName != before.Name {
			t.Fatalf("missing initial name identity: %+v", cfg)
		}
	}
	e.updateParticipant(model.ActorCodex, func(p *model.ParticipantSnapshot) { p.SessionID = "session-b" })
	if err := e.SetPermissions(context.Background(), model.ActorCodex, model.PermissionReadOnly); err != nil {
		t.Fatal(err)
	}
	cfg := captures.latest(model.ActorCodex)
	if cfg.RoomID != before.ID || cfg.RoomName != before.Name || cfg.SessionID != "session-b" {
		t.Fatal("permission restart lost name/session identity")
	}
	config := e.cfg
	dir := config.Store.Dir()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	eventStore, err := store.OpenExisting(dir)
	if err != nil {
		t.Fatal(err)
	}
	event, err := model.NewEvent(before.ID, eventServiceRoomRenamed, model.ActorSystem, map[string]any{"name": "Renamed while suspended", "updated_at": time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err = eventStore.Append(&event); err != nil {
		t.Fatal(err)
	}
	config.Store = eventStore
	restored, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = restored.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, actor := range model.SlotActors() {
		cfg := captures.latest(actor)
		if cfg.RoomID != before.ID || cfg.RoomName != "Renamed while suspended" {
			t.Fatalf("stale runtime name: %+v", cfg)
		}
	}
	after := restored.SnapshotMeta()
	if after.ID != before.ID || *after.Collaboration != *before.Collaboration {
		t.Fatal("rename changed Room identity or collaboration")
	}
}
