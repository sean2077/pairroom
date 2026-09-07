package service

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedRuntimeDoesNotRecreateMissingRoomStore(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	project, _ := registry.Project(rooms[0].ProjectID)
	durable := rooms[0]
	durable.DataDir = filepath.Join(t.TempDir(), "missing-room")
	runtime, err := startEmbeddedRuntime(context.Background(), registry, project, durable, EmbeddedRuntimeConfig{Mock: true})
	if runtime != nil {
		_ = runtime.Close(context.Background())
	}
	if err == nil {
		t.Error("activation recreated a missing published Room store")
	}
	if _, err := os.Stat(durable.DataDir); !os.IsNotExist(err) {
		t.Errorf("activation touched the missing store: %v", err)
	}
}

func TestServiceRejectsReplacedRoomStoreBeforeRepairOrAppend(t *testing.T) {
	for _, operation := range []string{"activate", "lifecycle"} {
		t.Run(operation, func(t *testing.T) {
			registry, rooms := provisionRuntimeRooms(t, 2)
			project, _ := registry.Project(rooms[0].ProjectID)
			durable := rooms[0]
			durable.DataDir = rooms[1].DataDir
			path := filepath.Join(durable.DataDir, "events.jsonl")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			original = append(original, []byte(`{"seq":`)...) // valid foreign prefix plus crash-partial tail
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			if operation == "activate" {
				runtime, startErr := startEmbeddedRuntime(context.Background(), registry, project, durable, EmbeddedRuntimeConfig{Mock: true})
				if runtime != nil {
					_ = runtime.Close(context.Background())
				}
				err = startErr
			} else {
				err = appendServiceEvent(durable, EventRoomArchived, map[string]any{"room_id": durable.ID})
			}
			if err == nil {
				t.Error("a replacement store from a different Room was accepted")
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, original) {
				t.Error("foreign Room Event Log was repaired or appended before rejecting its identity")
			}
		})
	}
}
