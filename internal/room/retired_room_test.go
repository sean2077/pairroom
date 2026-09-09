package room

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func TestCurrentRoomDefaultsAndStoredInstructionsSurviveRestart(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			dir := t.TempDir()
			log, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			var collaboration *model.Collaboration
			if version != 0 {
				spec, err := (model.Collaboration{Version: version}).ForCreation()
				if err != nil {
					t.Fatal(err)
				}
				collaboration = &spec
			}
			e, err := New(Config{Repo: t.TempDir(), Store: log, Collaboration: collaboration})
			if err != nil {
				t.Fatal(err)
			}
			before := e.Snapshot()
			if before.Meta.Collaboration == nil {
				t.Fatal("new Room has no collaboration")
			}
			for _, actor := range model.SlotActors() {
				p := before.Participants[actor]
				if p.Role != model.RolePeer || p.PermissionProfile != model.PermissionConfigured {
					t.Fatalf("new Room acquired role-bound policy: %+v", p)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			log, err = store.OpenExisting(dir)
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := New(Config{Repo: before.Meta.Repo, Store: log})
			if err != nil {
				_ = log.Close()
				t.Fatal(err)
			}
			defer restarted.Close()
			after := restarted.Snapshot()
			if after.Meta.ID != before.Meta.ID || *after.Meta.Collaboration != *before.Meta.Collaboration {
				t.Fatalf("restart rewrote durable collaboration: before=%+v after=%+v", before.Meta, after.Meta)
			}
		})
	}
}

func TestRetiredRoomReplayFailsWithoutAppendingOrRewriting(t *testing.T) {
	for _, defect := range []string{"missing collaboration", "driver role", "missing permission", "participants.batch.updated", "service.room.bindings.completed", "service.legacy.imported"} {
		t.Run(defect, func(t *testing.T) {
			dir := t.TempDir()
			log, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			e, err := New(Config{Repo: t.TempDir(), Store: log})
			if err != nil {
				t.Fatal(err)
			}
			events, err := log.Load()
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(defect, ".") {
				event, err := model.NewEvent(events[0].RoomID, defect, model.ActorSystem, map[string]any{})
				if err != nil {
					t.Fatal(err)
				}
				event.Seq = uint64(len(events) + 1)
				events = append(events, event)
			} else {
				for i := range events {
					var fields map[string]any
					if err := json.Unmarshal(events[i].Data, &fields); err != nil {
						t.Fatal(err)
					}
					if defect == "missing collaboration" && events[i].Kind == EventRoomCreated {
						delete(fields, "collaboration")
					}
					if events[i].Kind == EventParticipantUpdated {
						if defect == "driver role" {
							fields["role"] = "driver"
						}
						if defect == "missing permission" {
							delete(fields, "permission_profile")
						}
					}
					events[i].Data, err = json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			var data bytes.Buffer
			for _, event := range events {
				if err := json.NewEncoder(&data).Encode(event); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, "events.jsonl")
			if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			log, err = store.OpenExisting(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			if _, err := New(Config{Store: log}); err == nil {
				t.Fatalf("accepted %s", defect)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, data.Bytes()) {
				t.Fatalf("rejected replay changed bytes: %v", err)
			}
		})
	}
}

func TestPublishedSchemaTenWorkspaceStillActivatesOnLiveWorktree(t *testing.T) {
	dir, repo := t.TempDir(), t.TempDir()
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{Repo: repo, Store: log})
	if err != nil {
		t.Fatal(err)
	}
	meta := e.SnapshotMeta()
	events, err := log.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	for _, event := range events {
		if event.Kind == EventParticipantUpdated {
			var fields map[string]any
			if err := json.Unmarshal(event.Data, &fields); err != nil {
				t.Fatal(err)
			}
			// v3.4.0 current Rooms emitted these zero-valued fields, including
			// time.Time despite omitempty. They are not a Legacy Room signal.
			fields["workspace"] = map[string]any{"kind": "driver-live", "path": repo,
				"dirty": false, "read_only": false, "read_only_enforced": false,
				"refreshed_at": "0001-01-01T00:00:00Z"}
			event.Data, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := json.NewEncoder(&data).Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err = store.OpenExisting(dir)
	if err != nil {
		t.Fatal(err)
	}
	captures := &configurationCapture{}
	restored, err := New(Config{Repo: repo, Store: log, ClaudeFactory: captures.factory, CodexFactory: captures.factory})
	if err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := restored.SnapshotMeta(); got.ID != meta.ID || *got.Collaboration != *meta.Collaboration {
		t.Fatal("existing Room identity/policy changed")
	}
	for _, actor := range model.SlotActors() {
		p := restored.Snapshot().Participants[actor]
		if p.Workspace.Kind != "live" || p.Workspace.Path != repo || p.Role != model.RolePeer {
			t.Fatalf("workspace=%+v", p)
		}
	}
}
