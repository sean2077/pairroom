package service

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func TestLANArchiveUnavailableProjectDoesNotRestoreGuestAdmission(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	client, pending, _ := f.join(t)
	f.accept(t, client, pending)
	ctx := context.Background()
	if err := f.management.runtimes.Suspend(ctx, f.room.ID); err != nil {
		t.Fatal(err)
	}
	project, ok := f.management.registry.Project(f.room.ProjectID)
	if !ok {
		t.Fatal("missing fixture Project")
	}
	moved := project.Root + "-unavailable"
	if err := os.Rename(project.Root, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(moved, project.Root) })
	if project, err := f.management.registry.RefreshProject(ctx, project.ID); err != nil || project.Available {
		t.Fatalf("Project remained available: %+v %v", project, err)
	}
	if status := f.localCall(t, "/api/v1/rooms/"+f.room.ID+"/archive", nil, nil, f.management.Token()); status != http.StatusOK {
		t.Fatalf("unavailable Project blocked archive: HTTP %d", status)
	}
	if err := lanshare.Call(ctx, client, f.invite, "status", nil, nil); err == nil {
		t.Fatal("archived Room admitted a member")
	}
	if err := os.Rename(moved, project.Root); err != nil {
		t.Fatal(err)
	}
	if project, err := f.management.registry.RefreshProject(ctx, project.ID); err != nil || !project.Available {
		t.Fatalf("Project did not recover: %+v %v", project, err)
	}
	if status := f.localCall(t, "/api/v1/rooms/"+f.room.ID+"/restore", nil, nil, f.management.Token()); status != http.StatusOK {
		t.Fatalf("restore: HTTP %d", status)
	}
	if err := lanshare.Call(ctx, client, f.invite, "status", nil, nil); err == nil {
		t.Fatal("restoring archived Room resurrected the admitted guest certificate")
	}
	if status := f.management.runtimes.Status(f.room.ID); status.Phase != RuntimeSuspended || status.OccupiesCapacity {
		t.Fatalf("old certificate activated the restored Room: %+v", status)
	}
	var old lanshare.JoinResponse
	if err := lanshare.Call(ctx, client, f.invite, "join-status", lanshare.JoinStatusRequest{RequestID: "remote-request"}, &old); err != nil || old.Status != "revoked" {
		t.Fatalf("archive did not durably revoke the old admission: %+v %v", old, err)
	}
}

func TestLANArchiveUnavailableRuntimePreservesLogAndCheckpointFailures(t *testing.T) {
	for _, failure := range []string{"missing_log", "corrupt_log", "checkpoint_failure"} {
		t.Run(failure, func(t *testing.T) {
			relayclient.IsolateNativeCaller(t)
			f := newLANHostFixture(t)
			client, pending, _ := f.join(t)
			f.accept(t, client, pending)
			ctx := context.Background()
			if err := f.management.runtimes.Suspend(ctx, f.room.ID); err != nil {
				t.Fatal(err)
			}
			project, ok := f.management.registry.Project(f.room.ProjectID)
			if !ok {
				t.Fatal("missing fixture Project")
			}
			moved := project.Root + "-unavailable"
			if err := os.Rename(project.Root, moved); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Rename(moved, project.Root) })
			if _, err := f.management.registry.RefreshProject(ctx, project.ID); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(f.room.DataDir, "events.jsonl")
			if failure == "checkpoint_failure" {
				checkpoint := filepath.Join(f.management.registry.Root(), "service-registry.json")
				if err := os.Remove(checkpoint); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(checkpoint, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				saved, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.WriteFile(logPath, saved, 0o600) })
				if failure == "missing_log" {
					err = os.Remove(logPath)
				} else {
					err = os.WriteFile(logPath, []byte("not an event\n"), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if status := f.localCall(t, "/api/v1/rooms/"+f.room.ID+"/archive", nil, nil, f.management.Token()); status < 400 {
				t.Fatalf("archive ignored %s: HTTP %d", failure, status)
			}
			if room, ok := f.management.registry.Room(f.room.ID); !ok || room.Archived() {
				t.Fatalf("failed revocation reported an archived Room: %+v", room)
			}
			if failure == "missing_log" {
				if _, err := os.Stat(logPath); !os.IsNotExist(err) {
					t.Fatalf("archive recreated missing evidence: %v", err)
				}
			}
		})
	}
}
