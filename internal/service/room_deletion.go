package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/version"
)

var (
	ErrRoomNotArchived = errors.New("room must be archived before permanent removal")
)

const (
	roomDeletionManifestSchema = 1
	roomDeletionIntentName     = "intent.json"
	roomDeletionCommittedName  = "committed.json"
	roomDeletionDataName       = "data"
	roomArchiveStubMaxLogSize  = 1 << 20
	roomArchiveStubMaxEvents   = 128
)

// RoomNotArchivedError reports the final lifecycle precondition for a
// destructive Room removal. Archive remains a reversible retention state;
// removal is intentionally a separate, explicit operation.
type RoomNotArchivedError struct {
	RoomID    string
	Lifecycle RoomLifecycle
}

func (e *RoomNotArchivedError) Error() string {
	if e == nil {
		return ErrRoomNotArchived.Error()
	}
	return fmt.Sprintf("%v: room %s is %s", ErrRoomNotArchived, e.RoomID, e.Lifecycle)
}

func (e *RoomNotArchivedError) Unwrap() error { return ErrRoomNotArchived }

type RoomDataDisposition string

const (
	// RoomDataDeleted means PairRoom-owned data was removed from the managed
	// rooms root after the Registry checkpoint committed.
	RoomDataDeleted RoomDataDisposition = "deleted"
	// RoomDataAlreadyMissing means the managed directory had already gone away;
	// the durable Registry and binding ownership were still removed.
	RoomDataAlreadyMissing RoomDataDisposition = "already_missing"
	// RoomDataRetainedExternal means an explicitly imported directory was only
	// unregistered. PairRoom never recursively deletes an external data path.
	RoomDataRetainedExternal RoomDataDisposition = "retained_external"
	// RoomDataCleanupPending means logical removal committed, while the staged
	// managed data remains in the non-discoverable deletion quarantine. Startup
	// and later removals retry best-effort cleanup.
	RoomDataCleanupPending RoomDataDisposition = "cleanup_pending"
)

type RoomRemovalResult struct {
	RoomID            string              `json:"room_id"`
	ProjectID         string              `json:"project_id"`
	DataDisposition   RoomDataDisposition `json:"data_disposition"`
	CleanupDiagnostic string              `json:"cleanup_diagnostic,omitempty"`
}

type RoomDeletionMaintenance struct {
	PendingCleanup int    `json:"pending_cleanup"`
	Diagnostic     string `json:"diagnostic,omitempty"`
}

type roomDeletionFS struct {
	rename    func(string, string) error
	removeAll func(string) error
	lstat     func(string) (os.FileInfo, error)
	readDir   func(string) ([]os.DirEntry, error)
	mkdir     func(string, os.FileMode) error
	mkdirTemp func(string, string) (string, error)
}

func defaultRoomDeletionFS() roomDeletionFS {
	return roomDeletionFS{
		rename: os.Rename, removeAll: os.RemoveAll, lstat: os.Lstat,
		readDir: os.ReadDir, mkdir: os.Mkdir, mkdirTemp: os.MkdirTemp,
	}
}

// roomDeletionQuarantine validates the recovery-journal directory without
// following symlinks. The directory is created lazily on the first managed
// Room deletion so ordinary Registry/provisioning workflows preserve their
// existing on-disk shape. The returned bool reports whether the directory
// exists after the call.
func (r *Registry) roomDeletionQuarantine(create bool) (bool, error) {
	info, err := r.roomDeletionFS.lstat(r.deletedRoomsRoot)
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return false, nil
		}
		if err := r.roomDeletionFS.mkdir(r.deletedRoomsRoot, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return false, fmt.Errorf("create Room deletion quarantine: %w", err)
		}
		info, err = r.roomDeletionFS.lstat(r.deletedRoomsRoot)
		if err != nil {
			return false, fmt.Errorf("inspect created Room deletion quarantine: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, fmt.Errorf("Room deletion quarantine is not a real directory: %s", r.deletedRoomsRoot)
		}
		if err := syncDir(r.roomsRoot); err != nil {
			return true, fmt.Errorf("sync Room data root after creating deletion quarantine: %w", err)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Room deletion quarantine: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("Room deletion quarantine is not a real directory: %s", r.deletedRoomsRoot)
	}
	return true, nil
}

type roomDeletionIntent struct {
	Schema     int       `json:"schema"`
	RoomID     string    `json:"room_id"`
	ProjectID  string    `json:"project_id"`
	SourceBase string    `json:"source_base"`
	CreatedAt  time.Time `json:"created_at"`
}

type roomDeletionCommit struct {
	Schema      int       `json:"schema"`
	RoomID      string    `json:"room_id"`
	CommittedAt time.Time `json:"committed_at"`
}

type stagedManagedRoom struct {
	source    string
	container string
	data      string
	intent    roomDeletionIntent
}

type inspectedDeletionEntry struct {
	name      string
	path      string
	data      string
	intent    roomDeletionIntent
	committed bool
	hasData   bool
}

// RemoveRoom permanently unregisters one archived Room and releases its Agent
// binding ownership. PairRoom-managed data is first atomically moved out of the
// discovery root, then the Registry checkpoint is committed, and only then is
// the quarantined directory recursively removed. Explicitly imported external
// directories are never deleted; they are only forgotten by the Service.
func (r *Registry) RemoveRoom(ctx context.Context, roomID string) (RoomRemovalResult, error) {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return RoomRemovalResult{}, errors.New("room ID is required")
	}

	r.provisionMu.Lock()
	defer r.provisionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return RoomRemovalResult{}, err
	}

	// Never let best-effort cleanup operate through a fail-closed boundary.
	// In particular, an uncommitted quarantine entry may represent a checkpoint
	// rename whose directory-sync durability is unknown; startup recovery must
	// compare the actual checkpoint before deciding whether to restore or erase.
	r.mu.RLock()
	healthErr := r.healthyLocked()
	r.mu.RUnlock()
	if healthErr != nil {
		return RoomRemovalResult{}, healthErr
	}

	// Retrying cleanup from a previously committed logical deletion is
	// deliberately best-effort. It must not block a different Room removal.
	r.retryRoomDeletionCleanup(ctx)

	r.mu.Lock()
	if err := r.healthyLocked(); err != nil {
		r.mu.Unlock()
		return RoomRemovalResult{}, err
	}
	room, ok := r.rooms[roomID]
	if !ok {
		r.mu.Unlock()
		return RoomRemovalResult{}, ErrRoomNotFound
	}
	room = cloneRoom(room)
	if !room.Archived() {
		r.mu.Unlock()
		return RoomRemovalResult{}, &RoomNotArchivedError{RoomID: room.ID, Lifecycle: room.Lifecycle}
	}

	managed, err := r.managedRoomPath(room.DataDir)
	if err != nil {
		r.mu.Unlock()
		return RoomRemovalResult{}, err
	}
	if !managed {
		if _, ok := r.importedDirs[filepath.Clean(room.DataDir)]; !ok {
			err := r.poisonLocked(fmt.Errorf("external Room %s is missing its imported-directory index for %s", room.ID, room.DataDir))
			r.mu.Unlock()
			return RoomRemovalResult{}, err
		}
	}
	for _, binding := range room.Bindings {
		if !binding.OwnsIdentity() {
			continue
		}
		owner, ok := r.bindingOwners[binding.Key().String()]
		if !ok || owner != room.ID {
			err := r.poisonLocked(fmt.Errorf("binding ownership index is inconsistent for Room %s %s session %q", room.ID, binding.Agent, binding.SessionID))
			r.mu.Unlock()
			return RoomRemovalResult{}, err
		}
	}
	// No filesystem operation is performed while the Registry RW lock is held.
	// provisionMu keeps the projection stable across staging and checkpointing.
	r.mu.Unlock()

	var staged *stagedManagedRoom
	disposition := RoomDataRetainedExternal
	if managed {
		staged, disposition, err = r.stageManagedRoom(ctx, room)
		if err != nil {
			return RoomRemovalResult{}, err
		}
	}

	r.mu.Lock()
	if err := r.healthyLocked(); err != nil {
		r.mu.Unlock()
		if rollbackErr := r.rollbackStagedManagedRoom(staged); rollbackErr != nil {
			r.mu.Lock()
			err = r.poisonLocked(errors.Join(err, fmt.Errorf("restore staged Room %s: %w", room.ID, rollbackErr)))
			r.mu.Unlock()
		}
		return RoomRemovalResult{}, err
	}
	current, ok := r.rooms[room.ID]
	if !ok || current.DataDir != room.DataDir || current.Lifecycle != room.Lifecycle {
		err := r.poisonLocked(fmt.Errorf("Room %s changed while permanent removal was staged", room.ID))
		r.mu.Unlock()
		if rollbackErr := r.rollbackStagedManagedRoom(staged); rollbackErr != nil {
			r.mu.Lock()
			err = r.poisonLocked(errors.Join(err, fmt.Errorf("restore staged Room %s: %w", room.ID, rollbackErr)))
			r.mu.Unlock()
		}
		return RoomRemovalResult{}, err
	}

	delete(r.rooms, room.ID)
	for _, binding := range room.Bindings {
		if binding.OwnsIdentity() {
			delete(r.bindingOwners, binding.Key().String())
		}
	}
	if !managed {
		delete(r.importedDirs, filepath.Clean(room.DataDir))
	}
	published, checkpointErr := r.writeCheckpointLocked()
	if checkpointErr != nil {
		if published {
			err := r.poisonLocked(fmt.Errorf("Room %s removal checkpoint was replaced but directory sync failed: %w", room.ID, checkpointErr))
			r.mu.Unlock()
			// The checkpoint rename is visible but its crash durability is
			// uncertain. Keep the prepared manifest: startup compares the actual
			// checkpoint with the intent and either restores or completes cleanup.
			if staged != nil {
				r.setRoomDeletionCleanupDiagnostic(err.Error())
			}
			return RoomRemovalResult{}, err
		}
		r.rooms[room.ID] = cloneRoom(room)
		for _, binding := range room.Bindings {
			if binding.OwnsIdentity() {
				r.bindingOwners[binding.Key().String()] = room.ID
			}
		}
		if !managed {
			r.importedDirs[filepath.Clean(room.DataDir)] = struct{}{}
		}
		r.mu.Unlock()
		if rollbackErr := r.rollbackStagedManagedRoom(staged); rollbackErr != nil {
			r.mu.Lock()
			err := r.poisonLocked(errors.Join(
				fmt.Errorf("persist Room %s removal: %w", room.ID, checkpointErr),
				fmt.Errorf("restore staged Room data: %w", rollbackErr),
			))
			r.mu.Unlock()
			return RoomRemovalResult{}, err
		}
		return RoomRemovalResult{}, fmt.Errorf("persist Room %s removal: %w", room.ID, checkpointErr)
	}
	r.mu.Unlock()

	result := RoomRemovalResult{RoomID: room.ID, ProjectID: room.ProjectID, DataDisposition: disposition}
	if staged != nil {
		markerErr := r.markStagedManagedRoomCommitted(staged)
		cleanupErr := r.deleteStagedManagedRoom(staged)
		if cleanupErr != nil {
			result.DataDisposition = RoomDataCleanupPending
			result.CleanupDiagnostic = errors.Join(markerErr, cleanupErr).Error()
			r.setRoomDeletionCleanupDiagnostic(result.CleanupDiagnostic)
		} else {
			if markerErr != nil {
				// Physical deletion completed, so the failed marker cannot leave a
				// recovery ambiguity. Surface the warning without reporting pending
				// data cleanup.
				result.CleanupDiagnostic = markerErr.Error()
			}
			r.refreshRoomDeletionCleanupDiagnostic()
		}
	}
	return result, nil
}

func (r *Registry) managedRoomPath(dataDir string) (bool, error) {
	clean := filepath.Clean(strings.TrimSpace(dataDir))
	if clean == "." || !filepath.IsAbs(clean) {
		return false, fmt.Errorf("Room data directory is not an absolute path: %q", dataDir)
	}
	relative, err := filepath.Rel(r.roomsRoot, clean)
	if err != nil {
		return false, fmt.Errorf("classify Room data directory: %w", err)
	}
	if relative == "." {
		return false, errors.New("Room data directory resolves to the managed rooms root")
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false, nil
	}
	if filepath.Dir(relative) != "." || relative == roomDeletionQuarantineName {
		return false, fmt.Errorf("Room data directory is not a direct managed Room directory: %s", clean)
	}
	return true, nil
}

func (r *Registry) stageManagedRoom(ctx context.Context, room Room) (*stagedManagedRoom, RoomDataDisposition, error) {
	source := filepath.Clean(room.DataDir)
	info, err := r.roomDeletionFS.lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil, RoomDataAlreadyMissing, nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("inspect managed Room %s data: %w", room.ID, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, "", fmt.Errorf("managed Room %s data path is not a real directory: %s", room.ID, source)
	}
	if _, err := r.roomDeletionQuarantine(true); err != nil {
		return nil, "", err
	}
	container, err := r.roomDeletionFS.mkdirTemp(r.deletedRoomsRoot, ".pending-")
	if err != nil {
		return nil, "", fmt.Errorf("create Room deletion quarantine: %w", err)
	}
	staged := &stagedManagedRoom{
		source: source, container: container, data: filepath.Join(container, roomDeletionDataName),
		intent: roomDeletionIntent{
			Schema: roomDeletionManifestSchema, RoomID: room.ID, ProjectID: room.ProjectID,
			SourceBase: filepath.Base(source), CreatedAt: r.now(),
		},
	}
	if err := writeDurableJSONExclusive(filepath.Join(container, roomDeletionIntentName), staged.intent); err != nil {
		_ = r.roomDeletionFS.removeAll(container)
		return nil, "", fmt.Errorf("write Room %s deletion intent: %w", room.ID, err)
	}
	if err := r.roomDeletionFS.rename(source, staged.data); err != nil {
		_ = r.roomDeletionFS.removeAll(container)
		return nil, "", fmt.Errorf("stage managed Room %s for permanent removal: %w", room.ID, err)
	}
	if err := errors.Join(syncDir(r.roomsRoot), syncDir(container), syncDir(r.deletedRoomsRoot)); err != nil {
		return nil, "", r.rollbackFailedRoomStaging(staged, fmt.Errorf("sync staged Room %s removal: %w", room.ID, err))
	}

	// Re-materialize the directory after the atomic rename and before changing
	// Registry ownership. This closes the lstat-to-rename race: a same-user
	// process that replaces the source path can at most make removal fail; it
	// cannot trick PairRoom into checkpointing deletion of unrelated data.
	state := inspectedDeletionEntry{
		name: filepath.Base(container), path: container, data: staged.data,
		intent: staged.intent, hasData: true,
	}
	if err := ctx.Err(); err != nil {
		return nil, "", r.rollbackFailedRoomStaging(staged, err)
	}
	disposition, err := r.classifyQuarantinedRoomFacts(ctx, state)
	if err != nil {
		return nil, "", r.rollbackFailedRoomStaging(staged, fmt.Errorf("verify staged Room %s data: %w", room.ID, err))
	}
	return staged, disposition, nil
}

func (r *Registry) rollbackFailedRoomStaging(staged *stagedManagedRoom, cause error) error {
	if rollbackErr := r.rollbackStagedManagedRoom(staged); rollbackErr != nil {
		r.mu.Lock()
		poisoned := r.poisonLocked(errors.Join(
			cause,
			fmt.Errorf("restore staged Room data: %w", rollbackErr),
		))
		r.mu.Unlock()
		return poisoned
	}
	return cause
}

func (r *Registry) markStagedManagedRoomCommitted(staged *stagedManagedRoom) error {
	if staged == nil {
		return nil
	}
	marker := roomDeletionCommit{
		Schema: roomDeletionManifestSchema, RoomID: staged.intent.RoomID, CommittedAt: r.now(),
	}
	path := filepath.Join(staged.container, roomDeletionCommittedName)
	if err := writeDurableJSONExclusive(path, marker); err != nil {
		return fmt.Errorf("write committed marker for Room %s deletion: %w", staged.intent.RoomID, err)
	}
	return nil
}

func (r *Registry) rollbackStagedManagedRoom(staged *stagedManagedRoom) error {
	if staged == nil {
		return nil
	}
	if _, err := r.roomDeletionFS.lstat(staged.data); errors.Is(err, os.ErrNotExist) {
		return r.removeMetadataOnlyDeletionContainer(staged.container)
	} else if err != nil {
		return fmt.Errorf("inspect quarantined Room data: %w", err)
	}
	if _, err := r.roomDeletionFS.lstat(staged.source); err == nil {
		return fmt.Errorf("original Room data path already exists: %s", staged.source)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect original Room data path: %w", err)
	}
	if err := r.roomDeletionFS.rename(staged.data, staged.source); err != nil {
		return fmt.Errorf("restore quarantined Room data: %w", err)
	}
	result := errors.Join(syncDir(r.roomsRoot), syncDir(staged.container))
	if err := r.roomDeletionFS.removeAll(staged.container); err != nil {
		result = errors.Join(result, fmt.Errorf("remove empty deletion quarantine: %w", err))
	}
	if err := syncDir(r.deletedRoomsRoot); err != nil {
		result = errors.Join(result, err)
	}
	return result
}

func (r *Registry) deleteStagedManagedRoom(staged *stagedManagedRoom) error {
	if staged == nil {
		return nil
	}
	if err := r.roomDeletionFS.removeAll(staged.container); err != nil {
		return fmt.Errorf("remove quarantined Room data: %w", err)
	}
	if err := syncDir(r.deletedRoomsRoot); err != nil {
		return fmt.Errorf("sync Room deletion quarantine: %w", err)
	}
	return nil
}

func (r *Registry) removeMetadataOnlyDeletionContainer(path string) error {
	if err := r.roomDeletionFS.removeAll(path); err != nil {
		return fmt.Errorf("remove metadata-only Room deletion quarantine: %w", err)
	}
	return syncDir(r.deletedRoomsRoot)
}

func writeDurableJSONExclusive(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := os.Lstat(path); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	file, err := os.CreateTemp(parent, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := file.Name()
	defer os.Remove(tmpName)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if err := syncDir(parent); err != nil {
		return err
	}
	return nil
}

func decodeStrictJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeStrictJSONBytes(data, value)
}

func decodeStrictJSONBytes(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("contains multiple JSON values")
		}
		return err
	}
	return nil
}

func validateDeletionIntent(intent roomDeletionIntent) error {
	if intent.Schema != roomDeletionManifestSchema {
		return fmt.Errorf("unsupported schema %d", intent.Schema)
	}
	if strings.TrimSpace(intent.RoomID) == "" || intent.RoomID == "." || intent.RoomID == ".." || strings.ContainsAny(intent.RoomID, "/\\\r\n\x00") {
		return errors.New("invalid Room ID")
	}
	if strings.TrimSpace(intent.ProjectID) == "" {
		return errors.New("Project ID is required")
	}
	if err := validateManagedRoomSourceBase(intent.SourceBase); err != nil {
		return err
	}
	if intent.CreatedAt.IsZero() {
		return errors.New("creation time is required")
	}
	return nil
}

func validateManagedRoomSourceBase(sourceBase string) error {
	if sourceBase == "" || sourceBase != strings.TrimSpace(sourceBase) || sourceBase == "." || sourceBase == ".." || filepath.Base(sourceBase) != sourceBase || sourceBase == roomDeletionQuarantineName || strings.ContainsAny(sourceBase, "/\\") || strings.IndexFunc(sourceBase, unicode.IsControl) >= 0 {
		return errors.New("source basename is not a safe managed Room directory name")
	}
	return nil
}

func validateDeletionCommit(commit roomDeletionCommit, intent roomDeletionIntent) error {
	if commit.Schema != roomDeletionManifestSchema {
		return fmt.Errorf("unsupported schema %d", commit.Schema)
	}
	if commit.RoomID != intent.RoomID {
		return fmt.Errorf("Room ID %q does not match intent %q", commit.RoomID, intent.RoomID)
	}
	if commit.CommittedAt.IsZero() {
		return errors.New("commit time is required")
	}
	return nil
}

func isRoomDeletionMetadataTemp(name string) bool {
	return (strings.HasPrefix(name, "."+roomDeletionIntentName+"-") ||
		strings.HasPrefix(name, "."+roomDeletionCommittedName+"-")) && strings.HasSuffix(name, ".tmp")
}

func (r *Registry) inspectDeletionEntry(entry os.DirEntry) (inspectedDeletionEntry, error) {
	state := inspectedDeletionEntry{
		name: entry.Name(), path: filepath.Join(r.deletedRoomsRoot, entry.Name()),
	}
	info, err := r.roomDeletionFS.lstat(state.path)
	if err != nil {
		return state, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		// The quarantine is an internal recovery journal. Unknown files or
		// symlinks are preserved and fail startup rather than being treated as
		// disposable debris; deleting them would cross the recovery trust boundary.
		return state, errors.New("unrecognized non-directory quarantine entry")
	}
	if !strings.HasPrefix(state.name, ".pending-") {
		return state, errors.New("unrecognized quarantine directory name")
	}

	children, err := r.roomDeletionFS.readDir(state.path)
	if err != nil {
		return state, fmt.Errorf("read quarantine directory: %w", err)
	}
	known := map[string]bool{
		roomDeletionIntentName: false, roomDeletionCommittedName: false, roomDeletionDataName: false,
	}
	for _, child := range children {
		if isRoomDeletionMetadataTemp(child.Name()) {
			// A crash can leave only a fully-written temporary manifest. It is
			// never authoritative until renamed to the stable manifest name.
			if err := r.roomDeletionFS.removeAll(filepath.Join(state.path, child.Name())); err != nil {
				return state, fmt.Errorf("remove stale manifest temporary file %q: %w", child.Name(), err)
			}
			continue
		}
		if _, ok := known[child.Name()]; !ok {
			return state, fmt.Errorf("contains unrecognized entry %q", child.Name())
		}
		known[child.Name()] = true
	}
	state.data = filepath.Join(state.path, roomDeletionDataName)
	if !known[roomDeletionDataName] {
		return state, nil
	}
	dataInfo, err := r.roomDeletionFS.lstat(state.data)
	if err != nil {
		return state, fmt.Errorf("inspect quarantined data: %w", err)
	}
	if dataInfo.Mode()&os.ModeSymlink != 0 || !dataInfo.IsDir() {
		return state, errors.New("quarantined data is not a real directory")
	}
	state.hasData = true
	if !known[roomDeletionIntentName] {
		return state, errors.New("quarantined Room data has no deletion intent")
	}
	if err := decodeStrictJSON(filepath.Join(state.path, roomDeletionIntentName), &state.intent); err != nil {
		return state, fmt.Errorf("decode deletion intent: %w", err)
	}
	if err := validateDeletionIntent(state.intent); err != nil {
		return state, fmt.Errorf("validate deletion intent: %w", err)
	}
	if known[roomDeletionCommittedName] {
		var commit roomDeletionCommit
		if err := decodeStrictJSON(filepath.Join(state.path, roomDeletionCommittedName), &commit); err != nil {
			return state, fmt.Errorf("decode committed marker: %w", err)
		}
		if err := validateDeletionCommit(commit, state.intent); err != nil {
			return state, fmt.Errorf("validate committed marker: %w", err)
		}
		state.committed = true
	}
	return state, nil
}

func (r *Registry) classifyQuarantinedRoomFacts(ctx context.Context, state inspectedDeletionEntry) (RoomDataDisposition, error) {
	stubErr := r.verifyMissingRoomArchiveStub(state)
	if stubErr == nil {
		// Older PairRoom versions opened lifecycle stores in create mode. If a
		// managed Room directory had already disappeared, archiving recreated a
		// tiny metadata + lifecycle-only Event Log. The original Room data was
		// still gone; stage and remove this narrowly recognized artifact while
		// reporting the durable data as already missing.
		return RoomDataAlreadyMissing, nil
	}

	room, project, found, err := r.readRoomFacts(ctx, state.data)
	if err != nil {
		return "", errors.Join(
			fmt.Errorf("read quarantined Room facts: %w", err),
			fmt.Errorf("validate missing-data archive stub: %w", stubErr),
		)
	}
	if !found {
		return "", errors.Join(
			errors.New("quarantined data does not contain a materializable Room Event Log"),
			fmt.Errorf("validate missing-data archive stub: %w", stubErr),
		)
	}
	if room.ID != state.intent.RoomID || room.ProjectID != state.intent.ProjectID || project.ID != state.intent.ProjectID {
		return "", fmt.Errorf("quarantined Room identity %s/%s does not match intent %s/%s", room.ProjectID, room.ID, state.intent.ProjectID, state.intent.RoomID)
	}
	if !room.Archived() {
		return "", fmt.Errorf("quarantined Room %s is %s instead of archived", room.ID, room.Lifecycle)
	}
	return RoomDataDeleted, nil
}

func (r *Registry) verifyQuarantinedRoomFacts(ctx context.Context, state inspectedDeletionEntry) error {
	_, err := r.classifyQuarantinedRoomFacts(ctx, state)
	return err
}

func (r *Registry) recoverArchivedRoomsWithoutFactsFromCheckpoint() error {
	checkpointRooms, trusted, _ := r.trustedCheckpointRooms()
	if !trusted {
		return nil
	}
	roomIDs := make([]string, 0, len(checkpointRooms))
	for roomID := range checkpointRooms {
		roomIDs = append(roomIDs, roomID)
	}
	sort.Strings(roomIDs)
	for _, roomID := range roomIDs {
		room := checkpointRooms[roomID]
		if !room.Archived() {
			continue
		}
		managed, err := r.managedRoomPath(room.DataDir)
		if err != nil {
			return fmt.Errorf("classify checkpoint Room %s data: %w", room.ID, err)
		}
		if !managed {
			continue
		}
		info, err := r.roomDeletionFS.lstat(room.DataDir)
		if errors.Is(err, os.ErrNotExist) {
			project, ok := r.projects[room.ProjectID]
			if !ok {
				return fmt.Errorf("checkpoint missing-data Room %s references unknown Project %s", room.ID, room.ProjectID)
			}
			if err := r.indexRoomLocked(project, room); err != nil {
				return fmt.Errorf("index checkpoint missing-data Room %s: %w", room.ID, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect checkpoint Room %s data: %w", room.ID, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			continue
		}
		state := inspectedDeletionEntry{
			data: room.DataDir,
			intent: roomDeletionIntent{
				RoomID: room.ID, ProjectID: room.ProjectID,
			},
		}
		if err := r.verifyMissingRoomArchiveStub(state); err != nil {
			continue
		}
		project, ok := r.projects[room.ProjectID]
		if !ok {
			return fmt.Errorf("checkpoint archive stub Room %s references unknown Project %s", room.ID, room.ProjectID)
		}
		if err := r.indexRoomLocked(project, room); err != nil {
			return fmt.Errorf("index checkpoint archive stub Room %s: %w", room.ID, err)
		}
	}
	return nil
}

func (r *Registry) verifyMissingRoomArchiveStub(state inspectedDeletionEntry) error {
	entries, err := r.roomDeletionFS.readDir(state.data)
	if err != nil {
		return fmt.Errorf("read archive stub directory: %w", err)
	}

	paths := make(map[string]string, 2)
	for _, entry := range entries {
		name := entry.Name()
		if name != "events.jsonl" && name != "metadata.json" {
			return fmt.Errorf("archive stub contains unexpected entry %q", name)
		}
		path := filepath.Join(state.data, name)
		info, err := r.roomDeletionFS.lstat(path)
		if err != nil {
			return fmt.Errorf("inspect archive stub entry %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("archive stub entry %q is not a regular file", name)
		}
		paths[name] = path
	}
	if len(paths) != 2 {
		return errors.New("archive stub does not contain both events.jsonl and metadata.json")
	}

	var metadata struct {
		Format        string `json:"format"`
		SchemaVersion int    `json:"schema_version"`
		AppVersion    string `json:"app_version"`
	}
	if err := decodeStrictJSON(paths["metadata.json"], &metadata); err != nil {
		return fmt.Errorf("decode archive stub metadata: %w", err)
	}
	if metadata.Format != "pairroom-jsonl" || metadata.SchemaVersion != version.StoreSchema || strings.TrimSpace(metadata.AppVersion) == "" {
		return fmt.Errorf("archive stub metadata is not a PairRoom schema %d store", version.StoreSchema)
	}

	logInfo, err := r.roomDeletionFS.lstat(paths["events.jsonl"])
	if err != nil {
		return fmt.Errorf("inspect archive stub Event Log: %w", err)
	}
	if logInfo.Size() <= 0 || logInfo.Size() > roomArchiveStubMaxLogSize {
		return fmt.Errorf("archive stub Event Log size %d is outside the trusted recovery bound", logInfo.Size())
	}
	data, err := os.ReadFile(paths["events.jsonl"])
	if err != nil {
		return fmt.Errorf("read archive stub Event Log: %w", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return errors.New("archive stub Event Log is empty or has an unterminated tail")
	}
	lines := bytes.Split(data[:len(data)-1], []byte{'\n'})
	if len(lines) == 0 || len(lines) > roomArchiveStubMaxEvents {
		return fmt.Errorf("archive stub contains %d events; expected 1-%d", len(lines), roomArchiveStubMaxEvents)
	}

	lifecycle := RoomActive
	archivedSeen := false
	eventIDs := make(map[string]struct{}, len(lines))
	for index, line := range lines {
		if len(line) == 0 {
			return fmt.Errorf("archive stub Event Log line %d is empty", index+1)
		}
		var event model.Event
		if err := decodeStrictJSONBytes(line, &event); err != nil {
			return fmt.Errorf("decode archive stub event %d: %w", index+1, err)
		}
		if event.Seq != uint64(index+1) {
			return fmt.Errorf("archive stub event %d has sequence %d", index+1, event.Seq)
		}
		if strings.TrimSpace(event.ID) == "" {
			return fmt.Errorf("archive stub event %d has an empty ID", index+1)
		}
		if _, duplicate := eventIDs[event.ID]; duplicate {
			return fmt.Errorf("archive stub event %d repeats ID %q", index+1, event.ID)
		}
		eventIDs[event.ID] = struct{}{}
		if event.RoomID != state.intent.RoomID {
			return fmt.Errorf("archive stub event %d belongs to Room %q instead of %q", index+1, event.RoomID, state.intent.RoomID)
		}
		if event.Actor != model.ActorSystem {
			return fmt.Errorf("archive stub event %d is not authored by system", index+1)
		}
		if event.CreatedAt.IsZero() {
			return fmt.Errorf("archive stub event %d has an empty creation time", index+1)
		}

		switch event.Kind {
		case EventRoomRenamed:
			var payload roomRenamedPayload
			if err := decodeStrictJSONBytes(event.Data, &payload); err != nil {
				return fmt.Errorf("decode archive stub rename event %d: %w", index+1, err)
			}
			if err := validateRoomName(payload.Name); err != nil {
				return fmt.Errorf("invalid archive stub rename event %d: %w", index+1, err)
			}
			if payload.UpdatedAt.IsZero() {
				return fmt.Errorf("archive stub rename event %d has an empty update time", index+1)
			}
		case EventRoomArchived, EventRoomRestored:
			var payload roomLifecyclePayload
			if err := decodeStrictJSONBytes(event.Data, &payload); err != nil {
				return fmt.Errorf("decode archive stub lifecycle event %d: %w", index+1, err)
			}
			expected := RoomArchived
			if event.Kind == EventRoomRestored {
				expected = RoomActive
			}
			if payload.Lifecycle != expected || payload.UpdatedAt.IsZero() {
				return fmt.Errorf("invalid archive stub lifecycle event %d", index+1)
			}
			if event.Kind == EventRoomArchived {
				if lifecycle == RoomArchived {
					return fmt.Errorf("archive stub archives an already archived Room at event %d", index+1)
				}
				archivedSeen = true
			} else if lifecycle != RoomArchived {
				return fmt.Errorf("archive stub restores an active Room at event %d", index+1)
			}
			lifecycle = payload.Lifecycle
		default:
			return fmt.Errorf("archive stub contains non-lifecycle event kind %q", event.Kind)
		}
	}
	if !archivedSeen || lifecycle != RoomArchived {
		return errors.New("archive stub does not end in the archived lifecycle")
	}
	return nil
}

func (r *Registry) restoreDeletionEntry(state inspectedDeletionEntry) error {
	source := filepath.Join(r.roomsRoot, state.intent.SourceBase)
	if filepath.Dir(source) != r.roomsRoot {
		return fmt.Errorf("refuse non-direct Room restore destination %s", source)
	}
	if _, err := r.roomDeletionFS.lstat(source); err == nil {
		return fmt.Errorf("refuse to overwrite existing Room path %s", source)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Room restore destination: %w", err)
	}
	if err := r.roomDeletionFS.rename(state.data, source); err != nil {
		return fmt.Errorf("restore Room %s from deletion quarantine: %w", state.intent.RoomID, err)
	}
	if err := errors.Join(syncDir(r.roomsRoot), syncDir(state.path)); err != nil {
		// The rename is already visible. Leave the metadata container in place,
		// but do not attempt a reverse rename after an uncertain durability barrier.
		return fmt.Errorf("sync restored Room %s: %w", state.intent.RoomID, err)
	}
	if err := r.roomDeletionFS.removeAll(state.path); err != nil {
		return fmt.Errorf("remove restored Room %s quarantine metadata: %w", state.intent.RoomID, err)
	}
	if err := syncDir(r.deletedRoomsRoot); err != nil {
		return fmt.Errorf("sync restored Room %s quarantine cleanup: %w", state.intent.RoomID, err)
	}
	return nil
}

func (r *Registry) trustedCheckpointRooms() (map[string]Room, bool, string) {
	data, err := os.ReadFile(r.checkpoint)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, "service registry checkpoint is missing"
	}
	if err != nil {
		return nil, false, fmt.Sprintf("read service registry checkpoint: %v", err)
	}
	var snapshot RegistrySnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, false, fmt.Sprintf("decode service registry checkpoint: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, false, "service registry checkpoint contains trailing JSON"
	}
	if snapshot.Schema != 1 && snapshot.Schema != 2 {
		return nil, false, fmt.Sprintf("unsupported service registry checkpoint schema %d", snapshot.Schema)
	}

	projects := make(map[string]Project, len(snapshot.Projects))
	projectRoots := make(map[string]string, len(snapshot.Projects))
	for _, project := range snapshot.Projects {
		root := strings.TrimSpace(project.Root)
		if strings.TrimSpace(project.ID) == "" || root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || project.ID != projectID(root) {
			return nil, false, fmt.Sprintf("checkpoint Project %q has an invalid identity", project.ID)
		}
		if _, duplicate := projects[project.ID]; duplicate {
			return nil, false, fmt.Sprintf("checkpoint contains duplicate Project %s", project.ID)
		}
		if owner, duplicate := projectRoots[root]; duplicate && owner != project.ID {
			return nil, false, fmt.Sprintf("checkpoint Project root %s has multiple identities", root)
		}
		projects[project.ID] = project
		projectRoots[root] = project.ID
	}

	rooms := make(map[string]Room, len(snapshot.Rooms))
	dataDirs := make(map[string]string, len(snapshot.Rooms))
	bindingOwners := make(map[string]string)
	for _, room := range snapshot.Rooms {
		if err := room.Validate(); err != nil {
			return nil, false, fmt.Sprintf("checkpoint Room %q is invalid: %v", room.ID, err)
		}
		if _, ok := projects[room.ProjectID]; !ok {
			return nil, false, fmt.Sprintf("checkpoint Room %s references unknown Project %s", room.ID, room.ProjectID)
		}
		if _, duplicate := rooms[room.ID]; duplicate {
			return nil, false, fmt.Sprintf("checkpoint contains duplicate Room %s", room.ID)
		}
		dir := strings.TrimSpace(room.DataDir)
		if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return nil, false, fmt.Sprintf("checkpoint Room %s has an invalid data directory", room.ID)
		}
		if owner, duplicate := dataDirs[dir]; duplicate && owner != room.ID {
			return nil, false, fmt.Sprintf("checkpoint data directory %s belongs to multiple Rooms", dir)
		}
		if pathWithin(r.roomsRoot, dir) {
			relative, relErr := filepath.Rel(r.roomsRoot, dir)
			if relErr != nil || filepath.Dir(relative) != "." || validateManagedRoomSourceBase(relative) != nil {
				return nil, false, fmt.Sprintf("checkpoint Room %s has an invalid managed data directory %s", room.ID, dir)
			}
		}
		for _, binding := range room.Bindings {
			if !binding.OwnsIdentity() {
				continue
			}
			key := binding.Key().String()
			if owner, duplicate := bindingOwners[key]; duplicate && owner != room.ID {
				return nil, false, fmt.Sprintf("checkpoint binding %s session %q belongs to multiple Rooms", binding.Agent, binding.SessionID)
			}
			bindingOwners[key] = room.ID
		}
		room.DataDir = dir
		rooms[room.ID] = cloneRoom(room)
		dataDirs[dir] = room.ID
	}
	return rooms, true, ""
}

// recoverRoomDeletionQuarantine resolves deletion intents before the normal
// Room scan. A prepared intent is restored whenever the checkpoint still owns
// the Room or the checkpoint cannot be trusted. A committed marker, or a valid
// checkpoint that omits the Room, makes physical cleanup safe.
func (r *Registry) recoverRoomDeletionQuarantine(ctx context.Context) error {
	exists, err := r.roomDeletionQuarantine(false)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	entries, err := r.roomDeletionFS.readDir(r.deletedRoomsRoot)
	if err != nil {
		return fmt.Errorf("scan Room deletion quarantine: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	checkpointRooms, checkpointTrusted, checkpointDiagnostic := r.trustedCheckpointRooms()
	var fatal []error
	var cleanupFailures []string
	for _, entry := range entries {
		state, inspectErr := r.inspectDeletionEntry(entry)
		if inspectErr != nil {
			fatal = append(fatal, fmt.Errorf("quarantine %s: %w", entry.Name(), inspectErr))
			continue
		}
		if !state.hasData {
			if removeErr := r.removeMetadataOnlyDeletionContainer(state.path); removeErr != nil {
				cleanupFailures = append(cleanupFailures, fmt.Sprintf("%s: %v", state.name, removeErr))
			}
			continue
		}
		if verifyErr := r.verifyQuarantinedRoomFacts(ctx, state); verifyErr != nil {
			fatal = append(fatal, fmt.Errorf("quarantine %s: %w", state.name, verifyErr))
			continue
		}

		deleteData := state.committed
		if !deleteData && checkpointTrusted {
			checkpointRoom, present := checkpointRooms[state.intent.RoomID]
			if present {
				expected := filepath.Join(r.roomsRoot, state.intent.SourceBase)
				if checkpointRoom.ProjectID != state.intent.ProjectID || checkpointRoom.DataDir != expected {
					fatal = append(fatal, fmt.Errorf("quarantine %s: checkpoint identity for Room %s conflicts with deletion intent", state.name, state.intent.RoomID))
					continue
				}
			} else {
				deleteData = true
			}
		}

		if deleteData {
			if removeErr := r.roomDeletionFS.removeAll(state.path); removeErr != nil {
				cleanupFailures = append(cleanupFailures, fmt.Sprintf("%s: %v", state.name, removeErr))
			}
			continue
		}
		if restoreErr := r.restoreDeletionEntry(state); restoreErr != nil {
			reason := checkpointDiagnostic
			if checkpointTrusted {
				reason = "checkpoint still references the Room"
			}
			fatal = append(fatal, fmt.Errorf("quarantine %s: restore required because %s: %w", state.name, reason, restoreErr))
		}
	}
	if len(cleanupFailures) == 0 {
		if err := syncDir(r.deletedRoomsRoot); err != nil {
			cleanupFailures = append(cleanupFailures, err.Error())
		}
	}
	sort.Strings(cleanupFailures)
	r.setRoomDeletionCleanupDiagnostic(strings.Join(cleanupFailures, "; "))
	if len(fatal) > 0 {
		return errors.Join(fatal...)
	}
	return nil
}

func (r *Registry) retryRoomDeletionCleanup(ctx context.Context) {
	exists, err := r.roomDeletionQuarantine(false)
	if err != nil {
		r.setRoomDeletionCleanupDiagnostic(err.Error())
		return
	}
	if !exists {
		r.setRoomDeletionCleanupDiagnostic("")
		return
	}
	entries, err := r.roomDeletionFS.readDir(r.deletedRoomsRoot)
	if err != nil {
		r.setRoomDeletionCleanupDiagnostic(fmt.Sprintf("scan Room deletion quarantine: %v", err))
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var failures []string
	for _, entry := range entries {
		state, inspectErr := r.inspectDeletionEntry(entry)
		if inspectErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", entry.Name(), inspectErr))
			continue
		}
		if !state.hasData {
			if err := r.removeMetadataOnlyDeletionContainer(state.path); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", state.name, err))
			}
			continue
		}
		if err := r.verifyQuarantinedRoomFacts(ctx, state); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", state.name, err))
			continue
		}
		r.mu.RLock()
		_, roomStillOwned := r.rooms[state.intent.RoomID]
		healthy := r.poisoned == nil
		r.mu.RUnlock()
		if !state.committed && (!healthy || roomStillOwned) {
			reason := fmt.Sprintf("prepared deletion is still referenced by Room %s", state.intent.RoomID)
			if !healthy {
				reason = "prepared deletion is retained while the Registry is fail-closed"
			}
			failures = append(failures, fmt.Sprintf("%s: %s", state.name, reason))
			continue
		}
		if err := r.roomDeletionFS.removeAll(state.path); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", state.name, err))
		}
	}
	if len(failures) == 0 {
		if err := syncDir(r.deletedRoomsRoot); err != nil {
			failures = append(failures, err.Error())
		}
	}
	sort.Strings(failures)
	r.setRoomDeletionCleanupDiagnostic(strings.Join(failures, "; "))
}

// RetryRoomDeletionCleanup retries physical erasure only for deletion intents
// whose logical Registry removal is already safe to honor. It is serialized
// with provisioning/removal and refuses to cross a fail-closed Registry state.
func (r *Registry) RetryRoomDeletionCleanup(ctx context.Context) (RoomDeletionMaintenance, error) {
	if err := ctx.Err(); err != nil {
		return r.RoomDeletionMaintenance(), err
	}
	r.provisionMu.Lock()
	defer r.provisionMu.Unlock()
	r.mu.RLock()
	healthErr := r.healthyLocked()
	r.mu.RUnlock()
	if healthErr != nil {
		return r.RoomDeletionMaintenance(), healthErr
	}
	r.retryRoomDeletionCleanup(ctx)
	return r.RoomDeletionMaintenance(), nil
}

func (r *Registry) RoomDeletionMaintenance() RoomDeletionMaintenance {
	status := RoomDeletionMaintenance{}
	exists, err := r.roomDeletionQuarantine(false)
	if err != nil {
		status.Diagnostic = err.Error()
		return status
	}
	if exists {
		entries, readErr := r.roomDeletionFS.readDir(r.deletedRoomsRoot)
		if readErr != nil {
			status.Diagnostic = fmt.Sprintf("scan Room deletion quarantine: %v", readErr)
			return status
		}
		status.PendingCleanup = len(entries)
	}
	r.mu.RLock()
	status.Diagnostic = r.roomDeletionCleanupDiagnostic
	r.mu.RUnlock()
	return status
}

func (r *Registry) setRoomDeletionCleanupDiagnostic(value string) {
	r.mu.Lock()
	r.roomDeletionCleanupDiagnostic = strings.TrimSpace(value)
	r.mu.Unlock()
}

func (r *Registry) refreshRoomDeletionCleanupDiagnostic() {
	status := r.RoomDeletionMaintenance()
	if status.PendingCleanup == 0 {
		r.setRoomDeletionCleanupDiagnostic("")
	}
}
