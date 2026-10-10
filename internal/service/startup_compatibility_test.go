package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/version"
)

func TestRegistryRejectsFutureFormatsBeforeRecovery(t *testing.T) {
	for _, kind := range []string{"pair-profile", "published-room", "staged-room", "quarantined-room"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			tempMarker := filepath.Join(root, ".service-registry-future.tmp")
			if err := os.WriteFile(tempMarker, []byte("preserve before compatibility validation"), 0600); err != nil {
				t.Fatal(err)
			}
			staging := filepath.Join(root, "rooms", ".provision-audit")
			if err := os.MkdirAll(staging, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(staging, "marker")
			if err := os.WriteFile(marker, []byte("preserve until the root is known compatible"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "pair-profile":
				if err := os.WriteFile(filepath.Join(root, agentPairProfilesFile), []byte(`{"schema":3,"profiles":[]}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "published-room", "staged-room", "quarantined-room":
				dir := staging
				if kind == "published-room" {
					dir = filepath.Join(root, "rooms", "future-room")
				} else if kind == "quarantined-room" {
					dir = filepath.Join(root, "rooms", roomDeletionQuarantineName, ".pending-future", "data")
				}
				if dir != staging {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, "metadata.json"), mustJSON(t, map[string]any{"format": "pairroom-jsonl", "schema_version": version.StoreSchema + 1}), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := OpenRegistry(context.Background(), RegistryConfig{Root: root})
			if err == nil || !strings.Contains(err.Error(), "newer PairRoom") {
				t.Errorf("future format did not fail at the version boundary: %v", err)
			}
			if _, err := os.Stat(tempMarker); err != nil {
				t.Errorf("root cleanup ran before rejecting future format: %v", err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Errorf("recovery changed root before rejecting future format: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "service-registry.json")); !os.IsNotExist(err) {
				t.Errorf("incompatible root gained checkpoint: %v", err)
			}
		})
	}
}

func TestRegistryRejectsIncompatibleProvisioningBeforeRecovery(t *testing.T) {
	for _, schema := range []int{1, 2, 7} {
		for _, location := range []string{"published", "staged", "quarantined"} {
			t.Run(fmt.Sprintf("%s-schema-%d", location, schema), func(t *testing.T) {
				registry, project := testRegistry(t, testGitRepo(t))
				created, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "Compatibility", Bindings: specs(BindingNew, BindingNew, "prefix")}, SyntheticProvisioner{})
				if err != nil {
					t.Fatal(err)
				}
				dir := created.DataDir
				if location == "staged" {
					dir = filepath.Join(registry.RoomsRoot(), ".provision-future")
				}
				if location == "quarantined" {
					dir = filepath.Join(registry.RoomsRoot(), roomDeletionQuarantineName, ".pending-future", "data")
					if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if dir != created.DataDir {
					if err := os.Rename(created.DataDir, dir); err != nil {
						t.Fatal(err)
					}
				}
				eventPath := filepath.Join(dir, "events.jsonl")
				data, err := os.ReadFile(eventPath)
				if err != nil {
					t.Fatal(err)
				}
				lines := bytes.Split(data, []byte{'\n'})
				var event model.Event
				if err := json.Unmarshal(lines[1], &event); err != nil {
					t.Fatal(err)
				}
				if event.Kind != EventRoomProvisioned {
					t.Fatalf("second record = %s", event.Kind)
				}
				var payload roomProvisionedPayload
				if err := json.Unmarshal(event.Data, &payload); err != nil {
					t.Fatal(err)
				}
				payload.Schema = schema
				event.Data = mustJSON(t, payload)
				lines[1] = mustJSON(t, event)
				if err := os.WriteFile(eventPath, bytes.Join(lines, []byte{'\n'}), 0600); err != nil {
					t.Fatal(err)
				}
				tempMarker := filepath.Join(registry.Root(), ".service-registry-prefix.tmp")
				stageMarker := filepath.Join(registry.RoomsRoot(), ".provision-prefix-marker", "marker")
				if err := os.MkdirAll(filepath.Dir(stageMarker), 0700); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{tempMarker, stageMarker} {
					if err := os.WriteFile(path, []byte("preserve before schema validation"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				before := map[string][]byte{}
				for _, path := range []string{eventPath, registry.checkpoint, tempMarker, stageMarker} {
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					before[path] = data
				}
				_, err = OpenRegistry(context.Background(), RegistryConfig{Root: registry.Root()})
				want := "unsupported room provisioning schema"
				if schema < 5 {
					want = "retired room provisioning schema"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("provisioning boundary = %v, want %s", err, want)
				}
				for path, want := range before {
					if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, want) {
						t.Errorf("startup changed %s before rejecting provisioning: %v", path, err)
					}
				}
			})
		}
	}
}

func TestRegistryStillCleansInterruptedProvisioning(t *testing.T) {
	for _, contents := range []string{"missing", "", `{"seq":`, "{\"seq\":1,\"kind\":\"room.created\"}\n", "{\"seq\":1,\"kind\":\"room.created\"}\n{\"seq\":2,"} {
		t.Run(contents, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "rooms", ".provision-interrupted")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "metadata.json"), mustJSON(t, map[string]any{"format": "pairroom-jsonl", "schema_version": version.StoreSchema}), 0600); err != nil {
				t.Fatal(err)
			}
			if contents != "missing" {
				if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: root}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("interrupted stage was not cleaned: %v", err)
			}
		})
	}
}

func TestProvisioningPreflightReadsOnlyBoundedCreationPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, contents string
		wantError      bool
	}{
		{"valid prefix without scanning later data", "{\"kind\":\"room.created\"}\n{\"kind\":\"service.room.provisioned\",\"data\":{\"schema\":5}}\nnot a valid later event\n", false},
		{"malformed complete initial record", "{broken}\n", true},
		{"oversized initial record", strings.Repeat("x", (64<<20)+1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(tc.contents), 0600); err != nil {
				t.Fatal(err)
			}
			err := preflightRoomProvisioning(dir)
			if (err != nil) != tc.wantError {
				t.Fatalf("prefix validation = %v, want error=%v", err, tc.wantError)
			}
		})
	}
}

func TestRegistryRejectsLostPublishedHistory(t *testing.T) {
	for _, archived := range []bool{false, true} {
		for _, loss := range []string{"missing-log", "empty-log", "partial-only-log", "empty-directory", "missing-active-directory"} {
			if archived && loss == "missing-active-directory" {
				continue
			}
			t.Run(fmt.Sprintf("archived-%v/%s", archived, loss), func(t *testing.T) {
				registry, project := testRegistry(t, testGitRepo(t))
				created, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{HostMode: model.HostEmbedded, ProjectID: project.ID, Name: "Published", Bindings: specs(BindingExisting, BindingExisting, "published")}, SyntheticProvisioner{})
				if err != nil {
					t.Fatal(err)
				}
				if archived {
					created, err = registry.ArchiveRoom(context.Background(), created.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				eventPath := filepath.Join(created.DataDir, "events.jsonl")
				switch loss {
				case "missing-log":
					err = os.Remove(eventPath)
				case "empty-log":
					err = os.WriteFile(eventPath, nil, 0600)
				case "partial-only-log":
					err = os.WriteFile(eventPath, []byte(`{"seq":1,`), 0600)
				case "empty-directory", "missing-active-directory":
					err = os.RemoveAll(created.DataDir)
					if err == nil && loss == "empty-directory" {
						err = os.Mkdir(created.DataDir, 0700)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(registry.checkpoint)
				if err != nil {
					t.Fatal(err)
				}
				tempMarker := filepath.Join(registry.Root(), ".service-registry-preserve.tmp")
				stageMarker := filepath.Join(registry.RoomsRoot(), ".provision-preserve", "marker")
				if err := os.MkdirAll(filepath.Dir(stageMarker), 0700); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{tempMarker, stageMarker} {
					if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: registry.Root()}); err == nil {
					t.Fatal("lost published history was silently dropped")
				}
				if after, err := os.ReadFile(registry.checkpoint); err != nil || !bytes.Equal(after, before) {
					t.Fatalf("checkpoint changed after failed recovery: %v", err)
				}
				for _, path := range []string{tempMarker, stageMarker} {
					if after, err := os.ReadFile(path); err != nil || string(after) != "preserve" {
						t.Errorf("startup changed recovery marker %s: %v", path, err)
					}
				}
				// Failed startup must not release this known Room's exact identities.
				for _, binding := range created.Bindings {
					if owner, ok := registry.BindingOwner(binding.Key()); !ok || owner != created.ID {
						t.Fatalf("Binding owner changed: %q, %v", owner, ok)
					}
				}
				if loss == "partial-only-log" {
					if after, err := os.ReadFile(eventPath); err != nil || string(after) != `{"seq":1,` {
						t.Fatalf("partial history was repaired: %q, %v", after, err)
					}
				}
			})
		}
	}
}

func TestRegistryRejectsLostLogDuringCheckpointRebuild(t *testing.T) {
	for _, contents := range []string{"missing", "", `{"seq":1,`} {
		t.Run(contents, func(t *testing.T) {
			registry, project := testRegistry(t, testGitRepo(t))
			created, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{HostMode: model.HostEmbedded, ProjectID: project.ID, Name: "Published", Bindings: specs(BindingExisting, BindingExisting, "rebuild")}, SyntheticProvisioner{})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(created.DataDir, "events.jsonl")
			if contents == "missing" {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte(contents), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(registry.checkpoint); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: registry.Root()}); err == nil {
				t.Fatal("lost published history was ignored during checkpoint rebuild")
			}
			if _, err := os.Stat(registry.checkpoint); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected rebuild published a checkpoint: %v", err)
			}
		})
	}
}

func TestRegistryStillRecoversWhollyMissingArchivedDirectory(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	created, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{HostMode: model.HostEmbedded, ProjectID: project.ID, Name: "Archived", Bindings: specs(BindingExisting, BindingExisting, "archived")}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	created, err = registry.ArchiveRoom(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(created.DataDir); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRegistry(context.Background(), RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Room(created.ID); !ok || !got.Archived() {
		t.Fatalf("missing archived identity was lost: %+v, %v", got, ok)
	}
	for _, binding := range created.Bindings {
		if owner, ok := reopened.BindingOwner(binding.Key()); !ok || owner != created.ID {
			t.Fatalf("archived Binding reservation lost: %q, %v", owner, ok)
		}
	}
	if _, err := reopened.RemoveRoom(context.Background(), created.ID); err != nil {
		t.Fatalf("explicit missing-directory cleanup failed: %v", err)
	}
}
