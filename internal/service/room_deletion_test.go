package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

type roomDeletionTestProvisioner struct{ suffix string }

func (p roomDeletionTestProvisioner) Provision(_ context.Context, _ Project, actor model.ActorID, spec BindingSpec, _ string) (Binding, func(context.Context) error, error) {
	return Binding{
		Agent: actor, Mode: spec.Mode,
		SessionID: fmt.Sprintf("%s-%s", p.suffix, actor),
		BoundAt:   time.Now().UTC(),
	}, func(context.Context) error { return nil }, nil
}

func roomDeletionTestSpecs() map[model.ActorID]BindingSpec {
	return map[model.ActorID]BindingSpec{
		model.ActorClaude: {Mode: BindingNew},
		model.ActorCodex:  {Mode: BindingNew},
	}
}

func roomDeletionTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	command := exec.Command("git", "init", "-q")
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	return dir
}

func ensureRoomDeletionTestQuarantine(t *testing.T, registry *Registry) {
	t.Helper()
	if _, err := registry.roomDeletionQuarantine(true); err != nil {
		t.Fatalf("create Room deletion quarantine: %v", err)
	}
}

func roomDeletionTestRegistry(t *testing.T, suffix string) (*Registry, Project, Room) {
	t.Helper()
	ctx := context.Background()
	registry, err := OpenRegistry(ctx, RegistryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(ctx, roomDeletionTestGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	room, err := registry.ProvisionRoom(ctx, ProvisionRequest{
		ProjectID: project.ID,
		Name:      "Deletion test Room",
		Bindings:  roomDeletionTestSpecs(),
	}, roomDeletionTestProvisioner{suffix: suffix})
	if err != nil {
		t.Fatal(err)
	}
	return registry, project, room
}

func TestRoomDeletionQuarantineIsCreatedLazily(t *testing.T) {
	ctx := context.Background()
	registry, _, room := roomDeletionTestRegistry(t, "lazy-quarantine")
	if _, err := os.Lstat(registry.deletedRoomsRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh Registry created deletion quarantine: %v", err)
	}
	if _, err := registry.ArchiveRoom(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RemoveRoom(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(registry.deletedRoomsRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("first permanent removal did not create a real quarantine directory: info=%v err=%v", info, err)
	}
}

func TestRemoveRoomRequiresArchivedLifecycle(t *testing.T) {
	registry, _, room := roomDeletionTestRegistry(t, "active")
	_, err := registry.RemoveRoom(context.Background(), room.ID)
	if !errors.Is(err, ErrRoomNotArchived) {
		t.Fatalf("RemoveRoom error=%v want ErrRoomNotArchived", err)
	}
	var lifecycle *RoomNotArchivedError
	if !errors.As(err, &lifecycle) || lifecycle.RoomID != room.ID || lifecycle.Lifecycle != RoomActive {
		t.Fatalf("unexpected lifecycle diagnostic: %#v", lifecycle)
	}
	if _, ok := registry.Room(room.ID); !ok {
		t.Fatal("active Room was removed")
	}
	if _, err := os.Stat(room.DataDir); err != nil {
		t.Fatalf("active Room data changed: %v", err)
	}
}

func TestRemoveManagedRoomDeletesDataReleasesBindingsAndUnblocksProject(t *testing.T) {
	ctx := context.Background()
	registry, project, room := roomDeletionTestRegistry(t, "managed")
	marker := filepath.Join(room.DataDir, "attachments", "proof.txt")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("delete me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archived, err := registry.ArchiveRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range archived.Bindings {
		if owner, ok := registry.BindingOwner(binding.Key()); !ok || owner != room.ID {
			t.Fatalf("binding %s owner=%q ok=%v", binding.Agent, owner, ok)
		}
	}

	result, err := registry.RemoveRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.RoomID != room.ID || result.ProjectID != project.ID || result.DataDisposition != RoomDataDeleted || result.CleanupDiagnostic != "" {
		t.Fatalf("unexpected removal result: %#v", result)
	}
	if _, ok := registry.Room(room.ID); ok {
		t.Fatal("removed Room remained indexed")
	}
	if _, err := os.Stat(room.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed Room data still exists: %v", err)
	}
	for _, binding := range archived.Bindings {
		if owner, ok := registry.BindingOwner(binding.Key()); ok {
			t.Fatalf("binding %s remained owned by %s", binding.Agent, owner)
		}
	}
	if maintenance := registry.RoomDeletionMaintenance(); maintenance.PendingCleanup != 0 || maintenance.Diagnostic != "" {
		t.Fatalf("unexpected deletion maintenance: %#v", maintenance)
	}
	if _, err := registry.RemoveProject(ctx, project.ID); err != nil {
		t.Fatalf("last Room did not unblock Project removal: %v", err)
	}
	if _, err := os.Stat(project.Root); err != nil {
		t.Fatalf("Project worktree was touched: %v", err)
	}

	reopened, err := OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Room(room.ID); ok {
		t.Fatal("removed Room returned after restart")
	}
	if _, ok := reopened.Project(project.ID); ok {
		t.Fatal("removed Project returned after restart")
	}
}

func TestRemoveRoomRollsBackDataAndIndexesBeforeCheckpointPublication(t *testing.T) {
	ctx := context.Background()
	registry, _, room := roomDeletionTestRegistry(t, "rollback")
	archived, err := registry.ArchiveRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(registry.Root(), "service-registry.json")
	if err := os.Remove(checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(checkpoint, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err = registry.RemoveRoom(ctx, room.ID)
	if err == nil || !strings.Contains(err.Error(), "persist Room") {
		t.Fatalf("RemoveRoom error=%v want checkpoint publication failure", err)
	}
	projected, ok := registry.Room(room.ID)
	if !ok || projected.Lifecycle != RoomArchived {
		t.Fatalf("Room projection was not restored: %#v ok=%v", projected, ok)
	}
	if _, err := os.Stat(room.DataDir); err != nil {
		t.Fatalf("Room data was not restored: %v", err)
	}
	for _, binding := range archived.Bindings {
		if owner, ok := registry.BindingOwner(binding.Key()); !ok || owner != room.ID {
			t.Fatalf("binding %s was not restored: owner=%q ok=%v", binding.Agent, owner, ok)
		}
	}
	if err := registry.Healthy(); err != nil {
		t.Fatalf("pre-publication failure poisoned Registry: %v", err)
	}
	if maintenance := registry.RoomDeletionMaintenance(); maintenance.PendingCleanup != 0 {
		t.Fatalf("rollback left quarantined data: %#v", maintenance)
	}

	if err := os.Remove(checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RemoveRoom(ctx, room.ID); err != nil {
		t.Fatalf("removal did not recover after checkpoint repair: %v", err)
	}
}

func TestRemoveRoomRejectsSwappedManagedDirectoryBeforeCheckpoint(t *testing.T) {
	ctx := context.Background()
	registry, project, room := roomDeletionTestRegistry(t, "swap-primary")
	archived, err := registry.ArchiveRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := registry.ProvisionRoom(ctx, ProvisionRequest{
		ProjectID: project.ID,
		Name:      "Swap sentinel Room",
		Bindings:  roomDeletionTestSpecs(),
	}, roomDeletionTestProvisioner{suffix: "swap-sentinel"})
	if err != nil {
		t.Fatal(err)
	}
	other, err = registry.ArchiveRoom(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}

	originalRename := registry.roomDeletionFS.rename
	held := filepath.Join(registry.roomsRoot, ".swap-held-"+room.ID)
	swapped := false
	registry.roomDeletionFS.rename = func(oldPath, newPath string) error {
		if !swapped && oldPath == room.DataDir && filepath.Base(newPath) == roomDeletionDataName {
			swapped = true
			if err := os.Rename(room.DataDir, held); err != nil {
				return err
			}
			if err := os.Rename(other.DataDir, room.DataDir); err != nil {
				_ = os.Rename(held, room.DataDir)
				return err
			}
		}
		return originalRename(oldPath, newPath)
	}

	_, removeErr := registry.RemoveRoom(ctx, room.ID)
	registry.roomDeletionFS.rename = originalRename
	if removeErr == nil || !strings.Contains(removeErr.Error(), "verify staged Room") || !strings.Contains(removeErr.Error(), "does not match intent") {
		t.Fatalf("RemoveRoom error=%v want post-rename identity verification failure", removeErr)
	}
	if !swapped {
		t.Fatal("rename race was not injected")
	}
	if _, ok := registry.Room(room.ID); !ok {
		t.Fatal("identity mismatch changed the primary Room index")
	}
	if _, ok := registry.Room(other.ID); !ok {
		t.Fatal("identity mismatch changed the sentinel Room index")
	}
	for _, binding := range archived.Bindings {
		if owner, ok := registry.BindingOwner(binding.Key()); !ok || owner != room.ID {
			t.Fatalf("primary binding ownership changed: owner=%q ok=%v", owner, ok)
		}
	}
	if maintenance := registry.RoomDeletionMaintenance(); maintenance.PendingCleanup != 0 {
		t.Fatalf("identity mismatch left deletion quarantine: %#v", maintenance)
	}

	// Undo only the adversarial filesystem swap so test cleanup sees the same
	// durable state that existed before the injected race.
	if err := os.Rename(room.DataDir, other.DataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(held, room.DataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(room.DataDir, "events.jsonl")); err != nil {
		t.Fatalf("primary Room data was not preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other.DataDir, "events.jsonl")); err != nil {
		t.Fatalf("sentinel Room data was not preserved: %v", err)
	}
}

func TestRemoveRoomCommitsLogicalDeletionWhenPhysicalCleanupIsPending(t *testing.T) {
	ctx := context.Background()
	registry, _, room := roomDeletionTestRegistry(t, "cleanup")
	if _, err := registry.ArchiveRoom(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	originalRemoveAll := registry.roomDeletionFS.removeAll
	registry.roomDeletionFS.removeAll = func(path string) error {
		if filepath.Dir(path) == registry.deletedRoomsRoot {
			return errors.New("injected cleanup failure")
		}
		return originalRemoveAll(path)
	}

	result, err := registry.RemoveRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.DataDisposition != RoomDataCleanupPending || !strings.Contains(result.CleanupDiagnostic, "injected cleanup failure") {
		t.Fatalf("unexpected cleanup-pending result: %#v", result)
	}
	if _, ok := registry.Room(room.ID); ok {
		t.Fatal("logical deletion did not commit")
	}
	if _, err := os.Stat(room.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original Room path remained discoverable: %v", err)
	}
	if maintenance := registry.RoomDeletionMaintenance(); maintenance.PendingCleanup != 1 {
		t.Fatalf("cleanup quarantine not observable: %#v", maintenance)
	}

	registry.roomDeletionFS.removeAll = originalRemoveAll
	reopened, err := OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Room(room.ID); ok {
		t.Fatal("cleanup-pending Room returned after restart")
	}
	if maintenance := reopened.RoomDeletionMaintenance(); maintenance.PendingCleanup != 0 || maintenance.Diagnostic != "" {
		t.Fatalf("startup did not retry quarantine cleanup: %#v", maintenance)
	}
}

func TestRetryRoomDeletionCleanupPreservesPreparedDataWhileRegistryIsFailClosed(t *testing.T) {
	ctx := context.Background()
	registry, _, room := roomDeletionTestRegistry(t, "fail-closed-retry")
	archived, err := registry.ArchiveRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	staged := stageRoomDeletionCrash(t, registry, archived)

	registry.mu.Lock()
	delete(registry.rooms, room.ID)
	registry.poisoned = errors.New("injected checkpoint durability uncertainty")
	registry.mu.Unlock()

	// Exercise the internal retry defensively as well as the public health gate.
	registry.retryRoomDeletionCleanup(ctx)
	if _, err := os.Stat(filepath.Join(staged.data, "events.jsonl")); err != nil {
		t.Fatalf("fail-closed retry erased prepared data: %v", err)
	}
	if maintenance := registry.RoomDeletionMaintenance(); maintenance.PendingCleanup != 1 || !strings.Contains(maintenance.Diagnostic, "fail-closed") {
		t.Fatalf("prepared fail-closed deletion was not observable: %#v", maintenance)
	}
	if _, err := registry.RetryRoomDeletionCleanup(ctx); !errors.Is(err, ErrRegistryFailClosed) {
		t.Fatalf("RetryRoomDeletionCleanup error=%v want ErrRegistryFailClosed", err)
	}

	// The durable checkpoint still owns the Room, so startup must restore it.
	reopened, err := OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	if restored, ok := reopened.Room(room.ID); !ok || !restored.Archived() {
		t.Fatalf("startup did not restore prepared Room: %#v ok=%v", restored, ok)
	}
	if _, err := os.Stat(room.DataDir); err != nil {
		t.Fatalf("startup did not restore Room data: %v", err)
	}
}

func TestManagementRetryRoomDeletionCleanupClearsCommittedQuarantine(t *testing.T) {
	ctx := context.Background()
	registry, _, room := roomDeletionTestRegistry(t, "management-retry")
	if _, err := registry.ArchiveRoom(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	originalRemoveAll := registry.roomDeletionFS.removeAll
	registry.roomDeletionFS.removeAll = func(path string) error {
		if filepath.Dir(path) == registry.deletedRoomsRoot {
			return errors.New("injected cleanup failure")
		}
		return originalRemoveAll(path)
	}
	result, err := registry.RemoveRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.DataDisposition != RoomDataCleanupPending {
		t.Fatalf("removal disposition=%q want cleanup_pending", result.DataDisposition)
	}
	registry.roomDeletionFS.removeAll = originalRemoveAll

	manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) {
		return nil, errors.New("unexpected runtime activation")
	}, RuntimeManagerConfig{PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown Runtime Manager: %v", err)
		}
	})
	const token = "room-deletion-maintenance-secret"
	server, err := NewManagementServer(ManagementServerConfig{
		Registry: registry, Runtimes: manager,
		Provisioner: roomDeletionTestProvisioner{suffix: "management-retry"}, Token: token,
	})
	if err != nil {
		t.Fatal(err)
	}

	before := roomDeletionManagementRequest(t, server.Handler(), http.MethodGet, "/api/v1/service", token, nil)
	if before.Code != http.StatusOK {
		t.Fatalf("GET service status=%d body=%s", before.Code, before.Body.String())
	}
	var snapshot ServiceSnapshot
	if err := json.Unmarshal(before.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Maintenance.PendingCleanup != 1 || snapshot.Summary.PendingRoomCleanup != 1 || snapshot.Summary.AttentionItems < 1 {
		t.Fatalf("pending cleanup was not surfaced: %#v %#v", snapshot.Maintenance, snapshot.Summary)
	}

	retry := roomDeletionManagementRequest(t, server.Handler(), http.MethodPost, "/api/v1/maintenance/room-deletions/retry", token, nil)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry cleanup status=%d body=%s", retry.Code, retry.Body.String())
	}
	var maintenance RoomDeletionMaintenance
	if err := json.Unmarshal(retry.Body.Bytes(), &maintenance); err != nil {
		t.Fatal(err)
	}
	if maintenance.PendingCleanup != 0 || maintenance.Diagnostic != "" {
		t.Fatalf("retry cleanup remained pending: %#v", maintenance)
	}
}

func TestRemoveImportedRoomRetainsExternalDirectory(t *testing.T) {
	ctx := context.Background()
	source, project, room := roomDeletionTestRegistry(t, "external")
	if _, err := source.ArchiveRoom(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	externalParent := t.TempDir()
	externalDir := filepath.Join(externalParent, "imported-room")
	if err := os.Rename(room.DataDir, externalDir); err != nil {
		t.Fatal(err)
	}

	registry, err := OpenRegistry(ctx, RegistryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := registry.ImportLegacy(ctx, externalDir)
	if err != nil {
		t.Fatal(err)
	}
	if imported.ID != room.ID || !imported.Legacy || imported.DataDir != externalDir {
		t.Fatalf("unexpected imported Room: %#v", imported)
	}
	result, err := registry.RemoveRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.DataDisposition != RoomDataRetainedExternal {
		t.Fatalf("data disposition=%q want %q", result.DataDisposition, RoomDataRetainedExternal)
	}
	if _, err := os.Stat(filepath.Join(externalDir, "events.jsonl")); err != nil {
		t.Fatalf("external Room data was deleted: %v", err)
	}
	if _, err := registry.RemoveProject(ctx, project.ID); err != nil {
		t.Fatalf("external Room unregister did not unblock Project: %v", err)
	}
	reopened, err := OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Room(room.ID); ok {
		t.Fatal("unregistered external Room returned after restart")
	}
	if _, err := os.Stat(externalDir); err != nil {
		t.Fatalf("external directory did not survive restart: %v", err)
	}
}

func stageRoomDeletionCrash(t *testing.T, registry *Registry, room Room) *stagedManagedRoom {
	t.Helper()
	staged, disposition, err := registry.stageManagedRoom(context.Background(), room)
	if err != nil {
		t.Fatal(err)
	}
	if disposition != RoomDataDeleted || staged == nil {
		t.Fatalf("unexpected staging result: disposition=%q staged=%#v", disposition, staged)
	}
	return staged
}

func commitRoomRemovalCheckpointForTest(t *testing.T, registry *Registry, room Room) {
	t.Helper()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	delete(registry.rooms, room.ID)
	for _, binding := range room.Bindings {
		if binding.OwnsIdentity() {
			delete(registry.bindingOwners, binding.Key().String())
		}
	}
	published, err := registry.writeCheckpointLocked()
	if err != nil || !published {
		t.Fatalf("commit removal checkpoint: published=%v err=%v", published, err)
	}
}

func TestOpenRegistryRestoresPreparedRoomWhenCheckpointStillOwnsIt(t *testing.T) {
	ctx := context.Background()
	registry, project, room := roomDeletionTestRegistry(t, "crash-before-checkpoint")
	archived, err := registry.ArchiveRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	staged := stageRoomDeletionCrash(t, registry, archived)

	reopened, err := OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	restored, ok := reopened.Room(room.ID)
	if !ok || !restored.Archived() {
		t.Fatalf("prepared Room was not restored from the durable checkpoint: %#v ok=%v", restored, ok)
	}
	if _, err := os.Stat(room.DataDir); err != nil {
		t.Fatalf("prepared Room data was not restored: %v", err)
	}
	if _, err := os.Stat(staged.container); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared deletion metadata remained after restore: %v", err)
	}
	if _, err := reopened.RemoveProject(ctx, project.ID); !errors.Is(err, ErrProjectHasRooms) {
		t.Fatalf("restored Room did not keep Project deletion blocked: %v", err)
	}
}

func TestOpenRegistryCompletesPreparedRoomWhenCheckpointOmittedIt(t *testing.T) {
	ctx := context.Background()
	registry, project, room := roomDeletionTestRegistry(t, "crash-after-checkpoint")
	archived, err := registry.ArchiveRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	staged := stageRoomDeletionCrash(t, registry, archived)
	commitRoomRemovalCheckpointForTest(t, registry, archived)

	reopened, err := OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Room(room.ID); ok {
		t.Fatal("checkpoint-committed Room returned after restart")
	}
	if _, err := os.Stat(room.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original Room path returned after committed deletion: %v", err)
	}
	if _, err := os.Stat(staged.container); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed deletion quarantine remained: %v", err)
	}
	if _, err := reopened.RemoveProject(ctx, project.ID); err != nil {
		t.Fatalf("completed Room deletion did not unblock Project: %v", err)
	}
}

func TestOpenRegistryHonorsCommittedMarkerWhenCheckpointBecomesCorrupt(t *testing.T) {
	ctx := context.Background()
	registry, _, room := roomDeletionTestRegistry(t, "committed-marker")
	archived, err := registry.ArchiveRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	staged := stageRoomDeletionCrash(t, registry, archived)
	commitRoomRemovalCheckpointForTest(t, registry, archived)
	if err := registry.markStagedManagedRoomCommitted(staged); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registry.checkpoint, []byte("{corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Room(room.ID); ok {
		t.Fatal("committed marker allowed a Room to return after checkpoint corruption")
	}
	if _, err := os.Stat(staged.container); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed quarantine remained: %v", err)
	}
}

func TestOpenRegistryRefusesUnknownNonDirectoryQuarantineEntryAndPreservesIt(t *testing.T) {
	ctx := context.Background()
	registry, _, _ := roomDeletionTestRegistry(t, "unknown-file")
	ensureRoomDeletionTestQuarantine(t, registry)
	unknown := filepath.Join(registry.deletedRoomsRoot, "unexpected.txt")
	if err := os.WriteFile(unknown, []byte("preserve me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
	if err == nil || !strings.Contains(err.Error(), "unrecognized non-directory quarantine entry") {
		t.Fatalf("OpenRegistry error=%v want non-directory quarantine refusal", err)
	}
	contents, readErr := os.ReadFile(unknown)
	if readErr != nil || string(contents) != "preserve me\n" {
		t.Fatalf("unknown quarantine file was not preserved: contents=%q err=%v", contents, readErr)
	}
}

func TestOpenRegistryRefusesUnknownQuarantinedRoomData(t *testing.T) {
	ctx := context.Background()
	registry, _, room := roomDeletionTestRegistry(t, "unknown-quarantine")
	if _, err := registry.ArchiveRoom(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	ensureRoomDeletionTestQuarantine(t, registry)
	container, err := os.MkdirTemp(registry.deletedRoomsRoot, ".pending-")
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(container, roomDeletionDataName)
	if err := os.Rename(room.DataDir, data); err != nil {
		t.Fatal(err)
	}

	_, err = OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
	if err == nil || !strings.Contains(err.Error(), "no deletion intent") {
		t.Fatalf("OpenRegistry error=%v want unknown-quarantine refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(data, "events.jsonl")); statErr != nil {
		t.Fatalf("unknown quarantined data was not preserved: %v", statErr)
	}
}

type roomDeletionTestRuntime struct {
	busy     atomic.Bool
	closed   atomic.Int32
	draining atomic.Bool
}

func (*roomDeletionTestRuntime) URL() string  { return "http://127.0.0.1:1/" }
func (r *roomDeletionTestRuntime) Busy() bool { return r.busy.Load() }
func (r *roomDeletionTestRuntime) Close(context.Context) error {
	r.closed.Add(1)
	return nil
}
func (r *roomDeletionTestRuntime) SetDraining(value bool) { r.draining.Store(value) }

func TestRuntimeDeletionGateClosesIdleRuntimeAndRejectsActivation(t *testing.T) {
	registry, _, room := roomDeletionTestRegistry(t, "runtime-idle")
	if _, err := registry.ArchiveRoom(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) {
		return nil, errors.New("unexpected factory call")
	}, RuntimeManagerConfig{PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown Runtime Manager: %v", err)
		}
	})
	runtime := &roomDeletionTestRuntime{}
	manager.mu.Lock()
	manager.entries[room.ID] = &runtimeEntry{phase: RuntimeActive, runtime: runtime, lastUsed: time.Now()}
	manager.mu.Unlock()

	if err := manager.PrepareRoomDeletion(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	if runtime.closed.Load() != 1 || !runtime.draining.Load() {
		t.Fatalf("idle Runtime was not drained and closed: closed=%d draining=%v", runtime.closed.Load(), runtime.draining.Load())
	}
	if _, err := manager.RequestActivation(room.ID); !errors.Is(err, ErrRuntimeRoomDeleting) {
		t.Fatalf("activation error=%v want ErrRuntimeRoomDeleting", err)
	}
	manager.CommitRoomDeletion(room.ID)
	if status := manager.Status(room.ID); status.Phase != RuntimeSuspended {
		t.Fatalf("committed Runtime bookkeeping remained: %#v", status)
	}
}

func TestRuntimeDeletionGateRejectsBusyRuntimeAndReopensAdmission(t *testing.T) {
	registry, _, room := roomDeletionTestRegistry(t, "runtime-busy")
	if _, err := registry.ArchiveRoom(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) {
		return nil, errors.New("unexpected factory call")
	}, RuntimeManagerConfig{PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &roomDeletionTestRuntime{}
	t.Cleanup(func() {
		runtime.busy.Store(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown Runtime Manager: %v", err)
		}
	})
	runtime.busy.Store(true)
	manager.mu.Lock()
	manager.entries[room.ID] = &runtimeEntry{phase: RuntimeActive, runtime: runtime, lastUsed: time.Now()}
	manager.mu.Unlock()

	if err := manager.PrepareRoomDeletion(context.Background(), room.ID); !errors.Is(err, ErrRuntimeBusy) {
		t.Fatalf("PrepareRoomDeletion error=%v want ErrRuntimeBusy", err)
	}
	manager.mu.Lock()
	_, deleting := manager.deleting[room.ID]
	manager.mu.Unlock()
	if deleting || runtime.draining.Load() {
		t.Fatalf("failed deletion left admission closed: deleting=%v draining=%v", deleting, runtime.draining.Load())
	}
	if runtime.closed.Load() != 0 {
		t.Fatalf("busy Runtime was interrupted: closes=%d", runtime.closed.Load())
	}
}

func TestRoomLockSetReclaimsEntriesAfterPermanentRemoval(t *testing.T) {
	var locks roomLockSet
	unlock := locks.Lock("room-test")
	unlock()
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.entries) != 0 {
		t.Fatalf("Room lock entry leaked: %#v", locks.entries)
	}
}

func roomDeletionManagementRequest(t *testing.T, handler http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, &payload)
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestManagementRoomRemovalUsesExplicitAcknowledgementAndCompletesProjectCleanup(t *testing.T) {
	ctx := context.Background()
	registry, project, room := roomDeletionTestRegistry(t, "api")
	manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) {
		return nil, errors.New("unexpected runtime activation")
	}, RuntimeManagerConfig{PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown Runtime Manager: %v", err)
		}
	})
	const token = "room-deletion-api-secret"
	server, err := NewManagementServer(ManagementServerConfig{
		Registry: registry, Runtimes: manager,
		Provisioner: roomDeletionTestProvisioner{suffix: "api"}, Token: token,
	})
	if err != nil {
		t.Fatal(err)
	}

	service := roomDeletionManagementRequest(t, server.Handler(), http.MethodGet, "/api/v1/service", token, nil)
	if service.Code != http.StatusOK {
		t.Fatalf("GET service status=%d body=%s", service.Code, service.Body.String())
	}
	var snapshot ServiceSnapshot
	if err := json.Unmarshal(service.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Capabilities.RoomDeletion {
		t.Fatal("room_deletion capability is false")
	}

	missingAcknowledgement := roomDeletionManagementRequest(t, server.Handler(), http.MethodDelete, "/api/v1/rooms/"+room.ID, token, map[string]any{})
	if missingAcknowledgement.Code != http.StatusBadRequest || !strings.Contains(missingAcknowledgement.Body.String(), "acknowledge_data_loss") {
		t.Fatalf("missing acknowledgement status=%d body=%s", missingAcknowledgement.Code, missingAcknowledgement.Body.String())
	}

	active := roomDeletionManagementRequest(t, server.Handler(), http.MethodDelete, "/api/v1/rooms/"+room.ID, token, map[string]any{
		"acknowledge_data_loss": true,
	})
	if active.Code != http.StatusConflict {
		t.Fatalf("active removal status=%d body=%s", active.Code, active.Body.String())
	}
	var conflict struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(active.Body.Bytes(), &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Code != "room_not_archived" {
		t.Fatalf("active removal code=%q body=%s", conflict.Code, active.Body.String())
	}
	if _, ok := registry.Room(room.ID); !ok {
		t.Fatal("active API removal changed the Room")
	}

	if _, err := registry.ArchiveRoom(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	removed := roomDeletionManagementRequest(t, server.Handler(), http.MethodDelete, "/api/v1/rooms/"+room.ID, token, map[string]any{
		"acknowledge_data_loss": true,
	})
	if removed.Code != http.StatusOK {
		t.Fatalf("archived removal status=%d body=%s", removed.Code, removed.Body.String())
	}
	var result RoomRemovalResult
	if err := json.Unmarshal(removed.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.RoomID != room.ID || result.DataDisposition != RoomDataDeleted {
		t.Fatalf("unexpected API result: %#v", result)
	}

	projectRemoved := roomDeletionManagementRequest(t, server.Handler(), http.MethodDelete, "/api/v1/projects/"+project.ID, token, map[string]string{
		"confirm_project_id": project.ID,
	})
	if projectRemoved.Code != http.StatusNoContent {
		t.Fatalf("Project cleanup status=%d body=%s", projectRemoved.Code, projectRemoved.Body.String())
	}
}

func TestManagementRoomRemovalRefusesBusyRuntimeWithoutTouchingData(t *testing.T) {
	registry, _, room := roomDeletionTestRegistry(t, "api-busy")
	if _, err := registry.ArchiveRoom(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) {
		return nil, errors.New("unexpected runtime activation")
	}, RuntimeManagerConfig{PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &roomDeletionTestRuntime{}
	runtime.busy.Store(true)
	manager.mu.Lock()
	manager.entries[room.ID] = &runtimeEntry{phase: RuntimeActive, runtime: runtime, lastUsed: time.Now()}
	manager.mu.Unlock()
	t.Cleanup(func() {
		runtime.busy.Store(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown Runtime Manager: %v", err)
		}
	})
	const token = "room-deletion-busy-secret"
	server, err := NewManagementServer(ManagementServerConfig{
		Registry: registry, Runtimes: manager,
		Provisioner: roomDeletionTestProvisioner{suffix: "api-busy"}, Token: token,
	})
	if err != nil {
		t.Fatal(err)
	}

	response := roomDeletionManagementRequest(t, server.Handler(), http.MethodDelete, "/api/v1/rooms/"+room.ID, token, map[string]any{
		"acknowledge_data_loss": true,
	})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), ErrRuntimeBusy.Error()) {
		t.Fatalf("busy deletion status=%d body=%s", response.Code, response.Body.String())
	}
	if _, ok := registry.Room(room.ID); !ok {
		t.Fatal("busy Runtime deletion removed Registry state")
	}
	if _, err := os.Stat(room.DataDir); err != nil {
		t.Fatalf("busy Runtime deletion changed data: %v", err)
	}
	if runtime.closed.Load() != 0 {
		t.Fatalf("busy Runtime was interrupted: closes=%d", runtime.closed.Load())
	}
}

func provisionRoomDeletionTestRoom(t *testing.T, registry *Registry, project Project, name, suffix string) Room {
	t.Helper()
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID,
		Name:      name,
		Bindings:  roomDeletionTestSpecs(),
	}, roomDeletionTestProvisioner{suffix: suffix})
	if err != nil {
		t.Fatal(err)
	}
	return room
}

func newRoomDeletionManagementServer(t *testing.T, registry *Registry, suffix, token string) (*ManagementServer, *RuntimeManager) {
	t.Helper()
	manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) {
		return nil, errors.New("unexpected runtime activation")
	}, RuntimeManagerConfig{PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown Runtime Manager: %v", err)
		}
	})
	server, err := NewManagementServer(ManagementServerConfig{
		Registry: registry, Runtimes: manager,
		Provisioner: roomDeletionTestProvisioner{suffix: suffix}, Token: token,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server, manager
}

func TestManagementBatchRoomRemovalDeduplicatesAndUnblocksProject(t *testing.T) {
	ctx := context.Background()
	registry, project, first := roomDeletionTestRegistry(t, "batch-first")
	second := provisionRoomDeletionTestRoom(t, registry, project, "Second archived Room", "batch-second")
	for _, roomID := range []string{first.ID, second.ID} {
		if _, err := registry.ArchiveRoom(ctx, roomID); err != nil {
			t.Fatal(err)
		}
	}
	const token = "room-deletion-batch-secret"
	server, _ := newRoomDeletionManagementServer(t, registry, "batch", token)

	response := roomDeletionManagementRequest(t, server.Handler(), http.MethodPost, "/api/v1/rooms/batch-delete", token, map[string]any{
		"room_ids":              []string{first.ID, second.ID, first.ID},
		"acknowledge_data_loss": true,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("batch deletion status=%d body=%s", response.Code, response.Body.String())
	}
	var result RoomBatchRemovalResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Submitted != 3 || result.Processed != 2 || result.DuplicatesIgnored != 1 || result.Succeeded != 2 || result.Failed != 0 {
		t.Fatalf("unexpected batch summary: %#v", result)
	}
	if len(result.Results) != 2 || result.Results[0].RoomID != first.ID || result.Results[1].RoomID != second.ID {
		t.Fatalf("batch result order changed: %#v", result.Results)
	}
	for _, item := range result.Results {
		if item.Status != "deleted" || item.Removal == nil || item.Removal.RoomID != item.RoomID {
			t.Fatalf("unexpected successful item: %#v", item)
		}
		if _, ok := registry.Room(item.RoomID); ok {
			t.Fatalf("Room %s remained indexed", item.RoomID)
		}
	}
	if _, err := registry.RemoveProject(ctx, project.ID); err != nil {
		t.Fatalf("batch deletion did not unblock Project removal: %v", err)
	}
}

func TestManagementBatchRoomRemovalReportsPartialFailureWithoutRollback(t *testing.T) {
	ctx := context.Background()
	registry, project, archived := roomDeletionTestRegistry(t, "batch-partial-archived")
	active := provisionRoomDeletionTestRoom(t, registry, project, "Active Room", "batch-partial-active")
	if _, err := registry.ArchiveRoom(ctx, archived.ID); err != nil {
		t.Fatal(err)
	}
	const token = "room-deletion-batch-partial-secret"
	server, _ := newRoomDeletionManagementServer(t, registry, "batch-partial", token)
	missingID := "room-does-not-exist"

	response := roomDeletionManagementRequest(t, server.Handler(), http.MethodPost, "/api/v1/rooms/batch-delete", token, map[string]any{
		"room_ids":              []string{archived.ID, active.ID, missingID},
		"acknowledge_data_loss": true,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("partial batch status=%d body=%s", response.Code, response.Body.String())
	}
	var result RoomBatchRemovalResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Processed != 3 || result.Succeeded != 1 || result.Failed != 2 || len(result.Results) != 3 {
		t.Fatalf("unexpected partial batch summary: %#v", result)
	}
	if result.Results[0].Status != "deleted" || result.Results[0].RoomID != archived.ID {
		t.Fatalf("archived Room did not succeed first: %#v", result.Results)
	}
	if result.Results[1].Status != "failed" || result.Results[1].Code != "room_not_archived" || result.Results[1].RoomID != active.ID {
		t.Fatalf("active Room failure was not structured: %#v", result.Results[1])
	}
	if result.Results[2].Status != "failed" || result.Results[2].Code != "room_not_found" || result.Results[2].RoomID != missingID {
		t.Fatalf("missing Room failure was not structured: %#v", result.Results[2])
	}
	if _, ok := registry.Room(archived.ID); ok {
		t.Fatal("successful deletion was rolled back after another item failed")
	}
	if projected, ok := registry.Room(active.ID); !ok || projected.Archived() {
		t.Fatalf("failed active Room changed: %#v ok=%v", projected, ok)
	}
}

func TestManagementBatchRoomRemovalValidatesBeforeMutation(t *testing.T) {
	registry, _, room := roomDeletionTestRegistry(t, "batch-validation")
	if _, err := registry.ArchiveRoom(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	const token = "room-deletion-batch-validation-secret"
	server, _ := newRoomDeletionManagementServer(t, registry, "batch-validation", token)
	overLimit := make([]string, maxRoomBatchSize+1)
	for index := range overLimit {
		overLimit[index] = room.ID
	}
	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "missing acknowledgement", body: map[string]any{"room_ids": []string{room.ID}}},
		{name: "empty batch", body: map[string]any{"room_ids": []string{}, "acknowledge_data_loss": true}},
		{name: "blank ID", body: map[string]any{"room_ids": []string{""}, "acknowledge_data_loss": true}},
		{name: "surrounding whitespace", body: map[string]any{"room_ids": []string{" " + room.ID}, "acknowledge_data_loss": true}},
		{name: "over limit", body: map[string]any{"room_ids": overLimit, "acknowledge_data_loss": true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := roomDeletionManagementRequest(t, server.Handler(), http.MethodPost, "/api/v1/rooms/batch-delete", token, test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if _, ok := registry.Room(room.ID); !ok {
				t.Fatal("invalid batch mutated Room state")
			}
			if _, err := os.Stat(room.DataDir); err != nil {
				t.Fatalf("invalid batch mutated Room data: %v", err)
			}
		})
	}
}

func TestManagementBatchRoomRemovalKeepsBusyFailureAndDeletesOtherRoom(t *testing.T) {
	ctx := context.Background()
	registry, project, busyRoom := roomDeletionTestRegistry(t, "batch-busy")
	deletableRoom := provisionRoomDeletionTestRoom(t, registry, project, "Deletable Room", "batch-deletable")
	for _, roomID := range []string{busyRoom.ID, deletableRoom.ID} {
		if _, err := registry.ArchiveRoom(ctx, roomID); err != nil {
			t.Fatal(err)
		}
	}
	const token = "room-deletion-batch-busy-secret"
	server, manager := newRoomDeletionManagementServer(t, registry, "batch-busy", token)
	runtime := &roomDeletionTestRuntime{}
	runtime.busy.Store(true)
	manager.mu.Lock()
	manager.entries[busyRoom.ID] = &runtimeEntry{phase: RuntimeActive, runtime: runtime, lastUsed: time.Now()}
	manager.mu.Unlock()
	t.Cleanup(func() { runtime.busy.Store(false) })

	response := roomDeletionManagementRequest(t, server.Handler(), http.MethodPost, "/api/v1/rooms/batch-delete", token, map[string]any{
		"room_ids":              []string{busyRoom.ID, deletableRoom.ID},
		"acknowledge_data_loss": true,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("busy partial batch status=%d body=%s", response.Code, response.Body.String())
	}
	var result RoomBatchRemovalResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Succeeded != 1 || result.Failed != 1 || len(result.Results) != 2 {
		t.Fatalf("unexpected busy partial result: %#v", result)
	}
	if result.Results[0].RoomID != busyRoom.ID || result.Results[0].Code != "runtime_busy" {
		t.Fatalf("busy failure was not preserved: %#v", result.Results[0])
	}
	if result.Results[1].RoomID != deletableRoom.ID || result.Results[1].Status != "deleted" {
		t.Fatalf("deletable Room did not complete: %#v", result.Results[1])
	}
	if _, ok := registry.Room(busyRoom.ID); !ok {
		t.Fatal("busy Room was removed")
	}
	if _, ok := registry.Room(deletableRoom.ID); ok {
		t.Fatal("safe Room remained after partial batch")
	}
	if runtime.closed.Load() != 0 {
		t.Fatalf("busy Runtime was interrupted: closes=%d", runtime.closed.Load())
	}
}

func TestManagementBatchRoomArchiveDeduplicatesAndTreatsArchivedAsSuccess(t *testing.T) {
	ctx := context.Background()
	registry, project, first := roomDeletionTestRegistry(t, "batch-archive-first")
	second := provisionRoomDeletionTestRoom(t, registry, project, "Already archived Room", "batch-archive-second")
	if _, err := registry.ArchiveRoom(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	const token = "room-batch-archive-secret"
	server, _ := newRoomDeletionManagementServer(t, registry, "batch-archive", token)

	response := roomDeletionManagementRequest(t, server.Handler(), http.MethodPost, "/api/v1/rooms/batch-archive", token, map[string]any{
		"room_ids": []string{first.ID, second.ID, first.ID},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("batch archive status=%d body=%s", response.Code, response.Body.String())
	}
	var result RoomBatchArchiveResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Submitted != 3 || result.Processed != 2 || result.DuplicatesIgnored != 1 || result.Succeeded != 2 || result.Failed != 0 || result.AlreadyArchived != 1 {
		t.Fatalf("unexpected batch archive summary: %#v", result)
	}
	if len(result.Results) != 2 || result.Results[0].RoomID != first.ID || result.Results[0].Status != "archived" || result.Results[1].RoomID != second.ID || result.Results[1].Status != "already_archived" {
		t.Fatalf("unexpected batch archive results: %#v", result.Results)
	}
	for _, roomID := range []string{first.ID, second.ID} {
		projected, ok := registry.Room(roomID)
		if !ok || !projected.Archived() {
			t.Fatalf("Room %s was not archived: %#v ok=%v", roomID, projected, ok)
		}
	}
	if _, err := registry.RemoveProject(ctx, project.ID); err == nil {
		t.Fatal("archived Rooms unexpectedly stopped blocking Project removal")
	}
}

func TestManagementBatchRoomArchiveReportsBusyAndMissingWithoutBlockingOtherRooms(t *testing.T) {
	registry, project, busyRoom := roomDeletionTestRegistry(t, "batch-archive-busy")
	archiveable := provisionRoomDeletionTestRoom(t, registry, project, "Archiveable Room", "batch-archive-safe")
	const token = "room-batch-archive-partial-secret"
	server, manager := newRoomDeletionManagementServer(t, registry, "batch-archive-partial", token)
	runtime := &roomDeletionTestRuntime{}
	runtime.busy.Store(true)
	manager.mu.Lock()
	manager.entries[busyRoom.ID] = &runtimeEntry{phase: RuntimeActive, runtime: runtime, lastUsed: time.Now()}
	manager.mu.Unlock()
	t.Cleanup(func() { runtime.busy.Store(false) })
	missingID := "room-batch-archive-missing"

	response := roomDeletionManagementRequest(t, server.Handler(), http.MethodPost, "/api/v1/rooms/batch-archive", token, map[string]any{
		"room_ids": []string{busyRoom.ID, archiveable.ID, missingID},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("partial batch archive status=%d body=%s", response.Code, response.Body.String())
	}
	var result RoomBatchArchiveResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Succeeded != 1 || result.Failed != 2 || len(result.Results) != 3 {
		t.Fatalf("unexpected partial batch archive result: %#v", result)
	}
	if result.Results[0].RoomID != busyRoom.ID || result.Results[0].Status != "failed" || result.Results[0].Code != "runtime_busy" {
		t.Fatalf("busy failure was not structured: %#v", result.Results[0])
	}
	if result.Results[1].RoomID != archiveable.ID || result.Results[1].Status != "archived" {
		t.Fatalf("safe Room was not archived: %#v", result.Results[1])
	}
	if result.Results[2].RoomID != missingID || result.Results[2].Status != "failed" || result.Results[2].Code != "room_not_found" {
		t.Fatalf("missing failure was not structured: %#v", result.Results[2])
	}
	if projected, ok := registry.Room(busyRoom.ID); !ok || projected.Archived() {
		t.Fatalf("busy Room changed: %#v ok=%v", projected, ok)
	}
	if projected, ok := registry.Room(archiveable.ID); !ok || !projected.Archived() {
		t.Fatalf("archiveable Room did not change: %#v ok=%v", projected, ok)
	}
	if runtime.closed.Load() != 0 {
		t.Fatalf("busy Runtime was interrupted: closes=%d", runtime.closed.Load())
	}
}

func TestManagementBatchRoomArchiveValidatesBeforeMutation(t *testing.T) {
	registry, _, room := roomDeletionTestRegistry(t, "batch-archive-validation")
	const token = "room-batch-archive-validation-secret"
	server, _ := newRoomDeletionManagementServer(t, registry, "batch-archive-validation", token)
	overLimit := make([]string, maxRoomBatchSize+1)
	for index := range overLimit {
		overLimit[index] = room.ID
	}
	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "empty batch", body: map[string]any{"room_ids": []string{}}},
		{name: "blank ID", body: map[string]any{"room_ids": []string{""}}},
		{name: "surrounding whitespace", body: map[string]any{"room_ids": []string{" " + room.ID}}},
		{name: "over limit", body: map[string]any{"room_ids": overLimit}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := roomDeletionManagementRequest(t, server.Handler(), http.MethodPost, "/api/v1/rooms/batch-archive", token, test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			projected, ok := registry.Room(room.ID)
			if !ok || projected.Archived() {
				t.Fatalf("invalid batch mutated Room state: %#v ok=%v", projected, ok)
			}
		})
	}
}
