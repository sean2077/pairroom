package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

var (
	ErrProjectAlreadyRegistered = errors.New("project is already registered")
	ErrProjectNotFound          = errors.New("project not found")
	ErrRoomNotFound             = errors.New("room not found")
	ErrRoomBindingPending       = errors.New("room has pending agent bindings")
	ErrBindingOwned             = errors.New("binding identity is already owned")
	ErrRegistryFailClosed       = errors.New("service registry is fail-closed")
)

const roomDeletionQuarantineName = ".deleted-rooms"

type RegistryConfig struct {
	Root     string
	Resolver *ProjectResolver
	Now      func() time.Time
}

type Registry struct {
	mu          sync.RWMutex
	provisionMu sync.Mutex

	root             string
	roomsRoot        string
	deletedRoomsRoot string
	checkpoint       string
	resolver         *ProjectResolver
	now              func() time.Time

	projects      map[string]Project
	projectByRoot map[string]string
	rooms         map[string]Room
	bindingOwners map[string]string
	importedDirs  map[string]struct{}
	poisoned      error

	roomDeletionFS                roomDeletionFS
	roomDeletionCleanupDiagnostic string
}

func ensureRealDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, mode); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a real directory", path)
	}
	return nil
}

func DefaultRoot() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(base, "pairroom"), nil
}

func OpenRegistry(ctx context.Context, cfg RegistryConfig) (*Registry, error) {
	root, err := ResolveRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create service data root: %w", err)
	}
	roomsRoot := filepath.Join(root, "rooms")
	if err := os.MkdirAll(roomsRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create room data root: %w", err)
	}
	// Keep the deletion quarantine lazy. A fresh Registry should not gain an
	// otherwise unexplained data directory until the first permanent Room
	// removal. Startup recovery validates and scans it only when it already
	// exists.
	deletedRoomsRoot := filepath.Join(roomsRoot, roomDeletionQuarantineName)
	resolver := cfg.Resolver
	if resolver == nil {
		resolver = NewProjectResolver()
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	registry := &Registry{
		root:             root,
		roomsRoot:        roomsRoot,
		deletedRoomsRoot: deletedRoomsRoot,
		checkpoint:       filepath.Join(root, "service-registry.json"),
		resolver:         resolver,
		now:              now,
		projects:         make(map[string]Project),
		projectByRoot:    make(map[string]string),
		rooms:            make(map[string]Room),
		bindingOwners:    make(map[string]string),
		importedDirs:     make(map[string]struct{}),
		roomDeletionFS:   defaultRoomDeletionFS(),
	}
	// Resolve crash-interrupted Room deletions before the normal discovery scan.
	// Recovery restores prepared data when the durable checkpoint still owns the
	// Room (or cannot be trusted), and completes cleanup only after a committed
	// marker or a valid checkpoint proves that logical deletion won.
	if err := registry.recoverRoomDeletionQuarantine(ctx); err != nil {
		return nil, fmt.Errorf("recover Room deletion quarantine: %w", err)
	}
	// A valid checkpoint preserves explicitly registered projects that do not yet
	// have a Room. Room facts and binding ownership are always rebuilt from Room
	// Event Logs below; a missing or corrupt checkpoint is therefore recoverable.
	registry.loadCheckpointProjects()
	// PairRoom versions before the missing-data archive fix could replace an
	// externally deleted managed Room with a tiny lifecycle-only Event Log. The
	// fixed archive path can also hold an archived projection while the whole
	// directory remains absent. Neither state has independently materializable
	// facts, so recover only from a fully validated checkpoint and keep the Room
	// visible until the user explicitly completes permanent cleanup.
	if err := registry.recoverArchivedRoomsWithoutFactsFromCheckpoint(); err != nil {
		return nil, fmt.Errorf("recover archived Rooms without Event Log facts: %w", err)
	}
	if err := registry.scanRooms(ctx); err != nil {
		return nil, err
	}
	registry.refreshProjectAvailability(ctx)
	if _, err := registry.writeCheckpointLocked(); err != nil {
		return nil, fmt.Errorf("checkpoint rebuilt service registry: %w", err)
	}
	return registry, nil
}

func (r *Registry) Root() string      { return r.root }
func (r *Registry) RoomsRoot() string { return r.roomsRoot }

func (r *Registry) Healthy() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.poisoned != nil {
		return fmt.Errorf("%w: %v", ErrRegistryFailClosed, r.poisoned)
	}
	return nil
}

func (r *Registry) RegisterProject(ctx context.Context, input string) (Project, error) {
	project, err := r.resolver.Resolve(ctx, input)
	if err != nil {
		return Project{}, err
	}
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}
	project.CreatedAt = r.now()

	// Serialize every Registry write with Room provisioning. This prevents a
	// checkpoint failure in Project registration from racing a Room commit past
	// the Service's fail-closed boundary.
	r.provisionMu.Lock()
	defer r.provisionMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.healthyLocked(); err != nil {
		return Project{}, err
	}
	if existingID, ok := r.projectByRoot[project.Root]; ok {
		return r.projects[existingID], fmt.Errorf("%w: %s", ErrProjectAlreadyRegistered, project.Root)
	}
	r.projects[project.ID] = project
	r.projectByRoot[project.Root] = project.ID
	published, err := r.writeCheckpointLocked()
	if err != nil {
		if published {
			return Project{}, r.poisonLocked(fmt.Errorf("project registration checkpoint was replaced but directory sync failed: %w", err))
		}
		delete(r.projects, project.ID)
		delete(r.projectByRoot, project.Root)
		return Project{}, fmt.Errorf("persist project registration: %w", err)
	}
	return project, nil
}

func (r *Registry) Project(id string) (Project, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	project, ok := r.projects[id]
	return project, ok
}

func (r *Registry) Room(id string) (Room, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	room, ok := r.rooms[id]
	return cloneRoom(room), ok
}

func (r *Registry) Snapshot(includeArchived bool) RegistrySnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	projects := make([]Project, 0, len(r.projects))
	for _, project := range r.projects {
		projects = append(projects, project)
	}
	rooms := make([]Room, 0, len(r.rooms))
	for _, room := range r.rooms {
		if !includeArchived && room.Archived() {
			continue
		}
		rooms = append(rooms, cloneRoom(room))
	}
	return RegistrySnapshot{Schema: 1, GeneratedAt: r.now(), Projects: projects, Rooms: rooms}.Sorted()
}

func (r *Registry) BindingOwner(key BindingKey) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	owner, ok := r.bindingOwners[key.String()]
	return owner, ok
}

func (r *Registry) healthyLocked() error {
	if r.poisoned != nil {
		return fmt.Errorf("%w: %v", ErrRegistryFailClosed, r.poisoned)
	}
	return nil
}

func (r *Registry) poisonLocked(err error) error {
	if err == nil {
		return nil
	}
	if r.poisoned == nil {
		r.poisoned = err
	}
	return fmt.Errorf("%w: %v", ErrRegistryFailClosed, r.poisoned)
}

func (r *Registry) loadCheckpointProjects() {
	data, err := os.ReadFile(r.checkpoint)
	if err != nil {
		return
	}
	var snapshot RegistrySnapshot
	if json.Unmarshal(data, &snapshot) != nil || snapshot.Schema != 1 {
		return
	}
	for _, project := range snapshot.Projects {
		root := filepath.Clean(strings.TrimSpace(project.Root))
		if strings.TrimSpace(project.ID) == "" || root == "." || !filepath.IsAbs(root) || project.ID != projectID(root) {
			continue
		}
		project.Root = root
		if existingID, ok := r.projectByRoot[root]; ok && existingID != project.ID {
			continue
		}
		if existing, ok := r.projects[project.ID]; ok && existing.Root != root {
			continue
		}
		r.projects[project.ID] = project
		r.projectByRoot[root] = project.ID
	}
	// Checkpoint Room entries are never trusted as facts. They only retain the
	// absolute locations of explicitly imported custom legacy Rooms so those
	// Event Logs can be scanned again on a normal restart. If the checkpoint is
	// deleted, only the default Room root is discovered, as required.
	for _, room := range snapshot.Rooms {
		dir := filepath.Clean(strings.TrimSpace(room.DataDir))
		if dir == "" || !filepath.IsAbs(dir) || pathWithin(r.roomsRoot, dir) {
			continue
		}
		r.importedDirs[dir] = struct{}{}
	}
}

func (r *Registry) scanRooms(ctx context.Context) error {
	entries, err := os.ReadDir(r.roomsRoot)
	if err != nil {
		return fmt.Errorf("scan room data root: %w", err)
	}
	dirs := make([]string, 0, len(entries)+len(r.importedDirs))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if entry.Name() == roomDeletionQuarantineName {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".provision-") {
			// A staging directory is never a published Room. It can remain only
			// after process death, so removing it cannot delete visible history.
			if err := os.RemoveAll(filepath.Join(r.roomsRoot, entry.Name())); err != nil {
				return fmt.Errorf("remove stale room provisioning directory %s: %w", entry.Name(), err)
			}
			continue
		}
		dirs = append(dirs, filepath.Join(r.roomsRoot, entry.Name()))
	}
	for dir := range r.importedDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	seen := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		dir = filepath.Clean(dir)
		if _, duplicate := seen[dir]; duplicate {
			continue
		}
		seen[dir] = struct{}{}
		var recoveredWithoutFacts *Room
		for _, existing := range r.rooms {
			if filepath.Clean(existing.DataDir) != dir {
				continue
			}
			room := cloneRoom(existing)
			recoveredWithoutFacts = &room
			break
		}
		if existing := recoveredWithoutFacts; existing != nil {
			// Match checkpoint-only archive stubs by their persisted DataDir because
			// older managed Rooms can predate the durable-ID directory convention.
			// The stub has no room.created/provisioned fact to replay, so revalidate
			// it after recovery and fail closed on a path replacement race.
			state := inspectedDeletionEntry{
				data: dir,
				intent: roomDeletionIntent{
					RoomID: existing.ID, ProjectID: existing.ProjectID,
				},
			}
			if err := r.verifyMissingRoomArchiveStub(state); err != nil {
				return fmt.Errorf("revalidate recovered archive stub %s: %w", dir, err)
			}
			continue
		}
		room, project, found, err := r.readRoomFacts(ctx, dir)
		if err != nil {
			return fmt.Errorf("rebuild room %s: %w", dir, err)
		}
		if !found {
			continue
		}
		if err := r.indexRoomLocked(project, room); err != nil {
			return err
		}
		if !pathWithin(r.roomsRoot, dir) {
			r.importedDirs[dir] = struct{}{}
		}
	}
	return nil
}

func (r *Registry) readRoomFacts(ctx context.Context, dir string) (Room, Project, bool, error) {
	events, err := readEventsReadOnly(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Room{}, Project{}, false, nil
		}
		return Room{}, Project{}, false, err
	}
	if len(events) == 0 {
		return Room{}, Project{}, false, nil
	}

	var provisioned *roomProvisionedPayload
	var bindingsCompleted *roomBindingsCompletedPayload
	materializedBindings := make(map[model.ActorID]Binding, 2)
	var meta model.RoomMeta
	participants := make(map[model.ActorID]model.ParticipantSnapshot)
	var lifecycle RoomLifecycle = RoomActive
	var renamed string
	var updatedAt time.Time
	createdSeen := false
	serviceMutationSeen := false
	for _, event := range events {
		switch event.Kind {
		case EventRoomProvisioned:
			if event.Actor != model.ActorSystem {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d must be authored by system", event.Kind, event.Seq)
			}
			if !createdSeen {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d precedes room.created", event.Kind, event.Seq)
			}
			if provisioned != nil {
				return Room{}, Project{}, false, errors.New("room has multiple provisioning events")
			}
			if bindingsCompleted != nil {
				return Room{}, Project{}, false, errors.New("room binding completion precedes provisioning")
			}
			if serviceMutationSeen {
				return Room{}, Project{}, false, errors.New("room service mutation precedes provisioning")
			}
			var payload roomProvisionedPayload
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("decode %s event %d: %w", event.Kind, event.Seq, err)
			}
			if payload.Schema != 1 {
				return Room{}, Project{}, false, fmt.Errorf("unsupported room service schema %d", payload.Schema)
			}
			if payload.RoomID != event.RoomID {
				return Room{}, Project{}, false, fmt.Errorf("provisioned room ID %q conflicts with event room ID %q", payload.RoomID, event.RoomID)
			}
			if err := validateRoomName(payload.Name); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("invalid provisioned Room name: %w", err)
			}
			if payload.Lifecycle != RoomActive {
				return Room{}, Project{}, false, fmt.Errorf("provisioned Room lifecycle is %q; expected %q", payload.Lifecycle, RoomActive)
			}
			if err := validateProvisionedProject(payload.Project); err != nil {
				return Room{}, Project{}, false, err
			}
			if err := validateProvisionedBindings(payload.Bindings); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("invalid provisioned bindings: %w", err)
			}
			if payload.CreatedAt.IsZero() {
				return Room{}, Project{}, false, errors.New("provisioned Room has an empty creation time")
			}
			provisioned = &payload
			updatedAt = event.CreatedAt
		case EventRoomBindingsCompleted:
			if event.Actor != model.ActorSystem {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d must be authored by system", event.Kind, event.Seq)
			}
			if !createdSeen {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d precedes room.created", event.Kind, event.Seq)
			}
			if provisioned != nil {
				return Room{}, Project{}, false, errors.New("a provisioned Room cannot replace its durable bindings")
			}
			if bindingsCompleted != nil {
				return Room{}, Project{}, false, errors.New("room has multiple binding-completion events")
			}
			var payload roomBindingsCompletedPayload
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("decode %s event %d: %w", event.Kind, event.Seq, err)
			}
			if err := validateProvisionedBindings(payload.Bindings); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("invalid %s event %d: %w", event.Kind, event.Seq, err)
			}
			if payload.UpdatedAt.IsZero() {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d has an empty update time", event.Kind, event.Seq)
			}
			bindingsCompleted = &payload
			updatedAt = payload.UpdatedAt
		case EventRoomBindingMaterialized:
			if event.Actor != model.ActorSystem {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d must be authored by system", event.Kind, event.Seq)
			}
			if !createdSeen || (provisioned == nil && bindingsCompleted == nil) {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d requires selected Room bindings", event.Kind, event.Seq)
			}
			if lifecycle == RoomArchived {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d cannot mutate an archived Room", event.Kind, event.Seq)
			}
			var payload roomBindingMaterializedPayload
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("decode %s event %d: %w", event.Kind, event.Seq, err)
			}
			binding := payload.Binding
			if !binding.Agent.ValidParticipant() || binding.Pending || binding.Mode != BindingNew {
				return Room{}, Project{}, false, fmt.Errorf("invalid %s event %d binding", event.Kind, event.Seq)
			}
			if err := binding.Validate(); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("invalid %s event %d binding: %w", event.Kind, event.Seq, err)
			}
			var selectedBindings map[model.ActorID]Binding
			if provisioned != nil {
				selectedBindings = provisioned.Bindings
			} else {
				selectedBindings = bindingsCompleted.Bindings
			}
			initial, ok := selectedBindings[binding.Agent]
			if !ok || !initial.Pending || initial.Mode != BindingNew || initial.SessionID != "" {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d replaces a non-pending %s binding", event.Kind, event.Seq, binding.Agent)
			}
			if _, exists := materializedBindings[binding.Agent]; exists {
				return Room{}, Project{}, false, fmt.Errorf("%s binding is materialized more than once", binding.Agent)
			}
			if payload.UpdatedAt.IsZero() {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d has an empty update time", event.Kind, event.Seq)
			}
			if !binding.BoundAt.Equal(payload.UpdatedAt) {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d binding time conflicts with update time", event.Kind, event.Seq)
			}
			materializedBindings[binding.Agent] = binding
			serviceMutationSeen = true
			updatedAt = payload.UpdatedAt
		case EventRoomRenamed:
			if event.Actor != model.ActorSystem {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d must be authored by system", event.Kind, event.Seq)
			}
			if !createdSeen {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d precedes room.created", event.Kind, event.Seq)
			}
			var payload roomRenamedPayload
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("decode %s event %d: %w", event.Kind, event.Seq, err)
			}
			if err := validateRoomName(payload.Name); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("invalid %s event %d: %w", event.Kind, event.Seq, err)
			}
			if payload.UpdatedAt.IsZero() {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d has an empty update time", event.Kind, event.Seq)
			}
			serviceMutationSeen = true
			renamed = strings.TrimSpace(payload.Name)
			updatedAt = payload.UpdatedAt
		case EventRoomArchived, EventRoomRestored:
			if event.Actor != model.ActorSystem {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d must be authored by system", event.Kind, event.Seq)
			}
			if !createdSeen {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d precedes room.created", event.Kind, event.Seq)
			}
			var payload roomLifecyclePayload
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("decode %s event %d: %w", event.Kind, event.Seq, err)
			}
			expected := RoomArchived
			if event.Kind == EventRoomRestored {
				expected = RoomActive
			}
			if payload.Lifecycle != expected {
				return Room{}, Project{}, false, fmt.Errorf("invalid %s event %d lifecycle %q; expected %q", event.Kind, event.Seq, payload.Lifecycle, expected)
			}
			if event.Kind == EventRoomArchived && lifecycle == RoomArchived {
				return Room{}, Project{}, false, fmt.Errorf("Room is already archived at event %d", event.Seq)
			}
			if event.Kind == EventRoomRestored && lifecycle != RoomArchived {
				return Room{}, Project{}, false, fmt.Errorf("Room is not archived at restore event %d", event.Seq)
			}
			if payload.UpdatedAt.IsZero() {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d has an empty update time", event.Kind, event.Seq)
			}
			serviceMutationSeen = true
			lifecycle = payload.Lifecycle
			updatedAt = payload.UpdatedAt
		case "room.created":
			if createdSeen {
				return Room{}, Project{}, false, errors.New("room has multiple room.created events")
			}
			if err := json.Unmarshal(event.Data, &meta); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("decode room.created event %d: %w", event.Seq, err)
			}
			if strings.TrimSpace(meta.ID) == "" || meta.ID != event.RoomID {
				return Room{}, Project{}, false, fmt.Errorf("room.created ID %q conflicts with event room ID %q", meta.ID, event.RoomID)
			}
			if err := validateRoomName(meta.Name); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("invalid room.created name: %w", err)
			}
			if meta.CreatedAt.IsZero() {
				return Room{}, Project{}, false, errors.New("room.created has an empty creation time")
			}
			createdSeen = true
		case "participant.updated":
			var participant model.ParticipantSnapshot
			if err := json.Unmarshal(event.Data, &participant); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("decode participant.updated event %d: %w", event.Seq, err)
			}
			if participant.ID.ValidParticipant() {
				participants[participant.ID] = participant
			}
		}
	}

	if provisioned != nil {
		payload := *provisioned
		if err := validateProvisionedProject(payload.Project); err != nil {
			return Room{}, Project{}, false, err
		}
		if meta.ID != "" && meta.ID != payload.RoomID {
			return Room{}, Project{}, false, fmt.Errorf("room.created ID %q conflicts with provisioned ID %q", meta.ID, payload.RoomID)
		}
		if meta.Repo != "" && filepath.Clean(meta.Repo) != filepath.Clean(payload.Project.Root) {
			return Room{}, Project{}, false, fmt.Errorf("room.created repository %q conflicts with provisioned Project root %q", meta.Repo, payload.Project.Root)
		}
		if strings.TrimSpace(meta.Name) != strings.TrimSpace(payload.Name) {
			return Room{}, Project{}, false, fmt.Errorf("room.created name %q conflicts with provisioned name %q", meta.Name, payload.Name)
		}
		if !meta.CreatedAt.Equal(payload.CreatedAt) {
			return Room{}, Project{}, false, fmt.Errorf("room.created time %s conflicts with provisioned time %s", meta.CreatedAt, payload.CreatedAt)
		}
		room := Room{
			ID:                       payload.RoomID,
			ProjectID:                payload.Project.ID,
			Name:                     payload.Name,
			DataDir:                  dir,
			Lifecycle:                payload.Lifecycle,
			Bindings:                 cloneBindings(payload.Bindings),
			TranscriptBoundaryNotice: payload.TranscriptBoundaryNotice,
			CreatedAt:                payload.CreatedAt,
			UpdatedAt:                updatedAt,
		}
		if room.Lifecycle == "" {
			room.Lifecycle = lifecycle
		}
		if lifecycle.Valid() {
			room.Lifecycle = lifecycle
		}
		if renamed != "" {
			room.Name = renamed
		}
		if bindingsCompleted != nil {
			room.Bindings = cloneBindings(bindingsCompleted.Bindings)
		}
		for actor, binding := range materializedBindings {
			room.Bindings[actor] = binding
		}
		if room.UpdatedAt.IsZero() {
			room.UpdatedAt = room.CreatedAt
		}
		if err := room.Validate(); err != nil {
			return Room{}, Project{}, false, err
		}
		return room, payload.Project, true, nil
	}

	if strings.TrimSpace(meta.ID) == "" || strings.TrimSpace(meta.Repo) == "" {
		return Room{}, Project{}, false, errors.New("legacy room has no reconstructable room.created event")
	}
	if bindingsCompleted != nil {
		for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
			existingID := strings.TrimSpace(participants[actor].SessionID)
			if existingID == "" {
				continue
			}
			if completedID := strings.TrimSpace(bindingsCompleted.Bindings[actor].SessionID); completedID != existingID {
				return Room{}, Project{}, false, fmt.Errorf("binding completion replaces existing %s session %q with %q", actor, existingID, completedID)
			}
		}
	}
	project := r.legacyProject(ctx, meta.Repo, meta.CreatedAt)
	bindings := make(map[model.ActorID]Binding, 2)
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		participant := participants[actor]
		sessionID := strings.TrimSpace(participant.SessionID)
		bindings[actor] = Binding{
			Agent: actor, Mode: BindingExisting, SessionID: sessionID,
			Pending: sessionID == "", BoundAt: meta.CreatedAt,
		}
	}
	name := meta.Name
	if renamed != "" {
		name = renamed
	}
	room := Room{
		ID: meta.ID, ProjectID: project.ID, Name: name, DataDir: dir,
		Lifecycle: lifecycle, Bindings: bindings,
		TranscriptBoundaryNotice: TranscriptBoundaryNotice,
		Legacy:                   true, CreatedAt: meta.CreatedAt, UpdatedAt: updatedAt,
	}
	if bindingsCompleted != nil {
		room.Bindings = cloneBindings(bindingsCompleted.Bindings)
	}
	for actor, binding := range materializedBindings {
		room.Bindings[actor] = binding
	}
	if room.UpdatedAt.IsZero() {
		room.UpdatedAt = room.CreatedAt
	}
	if err := room.Validate(); err != nil {
		return Room{}, Project{}, false, err
	}
	return room, project, true, nil
}

func validateProvisionedProject(project Project) error {
	root := strings.TrimSpace(project.Root)
	if strings.TrimSpace(project.ID) == "" || root == "" || !filepath.IsAbs(root) {
		return errors.New("provisioned room has an invalid absolute Project Identity")
	}
	if filepath.Clean(root) != root {
		return fmt.Errorf("provisioned Project root %q is not canonical", root)
	}
	if project.ID != projectID(root) {
		return fmt.Errorf("project ID %q does not match canonical-root identity", project.ID)
	}
	return nil
}

func validateProvisionedBindings(bindings map[model.ActorID]Binding) error {
	if len(bindings) != 2 {
		return fmt.Errorf("exactly two bindings are required; got %d", len(bindings))
	}
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		binding, ok := bindings[actor]
		if !ok {
			return fmt.Errorf("missing %s binding", actor)
		}
		if binding.Agent != actor {
			return fmt.Errorf("%s binding identifies agent %s", actor, binding.Agent)
		}
		if binding.Pending && (binding.Mode != BindingNew || binding.SessionID != "") {
			return fmt.Errorf("%s pending binding is not a deferred new binding", actor)
		}
		if err := binding.Validate(); err != nil {
			return fmt.Errorf("%s binding: %w", actor, err)
		}
	}
	for actor := range bindings {
		if actor != model.ActorClaude && actor != model.ActorCodex {
			return fmt.Errorf("unexpected binding agent %q", actor)
		}
	}
	return nil
}

func (r *Registry) refreshProjectAvailability(ctx context.Context) {
	for id, project := range r.projects {
		resolved, err := r.resolver.Resolve(ctx, project.Root)
		if err != nil {
			project.Available = false
			project.Diagnostic = err.Error()
			r.projects[id] = project
			continue
		}
		if resolved.ID != id || resolved.Root != project.Root {
			project.Available = false
			project.Diagnostic = fmt.Sprintf("Project Identity now resolves to %s at %s", resolved.ID, resolved.Root)
			r.projects[id] = project
			continue
		}
		project.Available = true
		project.Diagnostic = ""
		r.projects[id] = project
	}
}

func (r *Registry) legacyProject(ctx context.Context, repo string, createdAt time.Time) Project {
	project, err := r.resolver.Resolve(ctx, repo)
	if err == nil {
		project.CreatedAt = createdAt
		return project
	}
	root := filepath.Clean(strings.TrimSpace(repo))
	// A relative repository path in a legacy log is not a stable Project
	// Identity. Preserve it as unavailable instead of resolving it against the
	// Service process's current working directory.
	if filepath.IsAbs(root) {
		if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
			root = resolved
		}
	}
	return Project{
		ID: projectID(root), Root: root, Available: false,
		Diagnostic: err.Error(), CreatedAt: createdAt,
	}
}

func (r *Registry) indexRoomLocked(project Project, room Room) error {
	// Preflight every conflict before changing any map. This function is used by
	// explicit legacy import while the Service remains live; a failed import must
	// not leave a Project or one side of a Binding reservation behind.
	if strings.TrimSpace(project.ID) == "" || strings.TrimSpace(project.Root) == "" {
		return errors.New("room has an invalid Project Identity")
	}
	if room.ProjectID != project.ID {
		return fmt.Errorf("room Project ID %q conflicts with Project %q", room.ProjectID, project.ID)
	}
	if err := room.Validate(); err != nil {
		return fmt.Errorf("validate room before indexing: %w", err)
	}
	if existing, ok := r.rooms[room.ID]; ok && existing.DataDir != room.DataDir {
		return fmt.Errorf("duplicate room ID %q in %s and %s", room.ID, existing.DataDir, room.DataDir)
	}
	if existingID, ok := r.projectByRoot[project.Root]; ok && existingID != project.ID {
		return fmt.Errorf("project root %s maps to conflicting IDs %s and %s", project.Root, existingID, project.ID)
	}
	if existing, ok := r.projects[project.ID]; ok && existing.Root != project.Root {
		return fmt.Errorf("project ID %s maps to conflicting roots %s and %s", project.ID, existing.Root, project.Root)
	}
	for _, binding := range room.Bindings {
		if !binding.OwnsIdentity() {
			continue
		}
		if owner, ok := r.bindingOwners[binding.Key().String()]; ok && owner != room.ID {
			return fmt.Errorf("%w: %s session %q is claimed by rooms %s and %s", ErrBindingOwned, binding.Agent, binding.SessionID, owner, room.ID)
		}
	}

	if existing, ok := r.projects[project.ID]; ok {
		// Prefer a currently accessible projection over an unavailable legacy one.
		if !existing.Available && project.Available {
			r.projects[project.ID] = project
		}
	} else {
		r.projects[project.ID] = project
	}
	r.projectByRoot[project.Root] = project.ID
	for _, binding := range room.Bindings {
		if binding.OwnsIdentity() {
			r.bindingOwners[binding.Key().String()] = room.ID
		}
	}
	r.rooms[room.ID] = cloneRoom(room)
	return nil
}

func (r *Registry) writeCheckpointLocked() (bool, error) {
	snapshot := RegistrySnapshot{Schema: 1, GeneratedAt: r.now()}
	for _, project := range r.projects {
		snapshot.Projects = append(snapshot.Projects, project)
	}
	for _, room := range r.rooms {
		snapshot.Rooms = append(snapshot.Rooms, cloneRoom(room))
	}
	snapshot = snapshot.Sorted()
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode service registry checkpoint: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(r.root, ".service-registry-*.tmp")
	if err != nil {
		return false, fmt.Errorf("create service registry checkpoint: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("chmod service registry checkpoint: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write service registry checkpoint: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("sync service registry checkpoint: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close service registry checkpoint: %w", err)
	}
	if err := os.Rename(tmpName, r.checkpoint); err != nil {
		return false, fmt.Errorf("replace service registry checkpoint: %w", err)
	}
	if err := syncDir(r.root); err != nil {
		return true, err
	}
	return true, nil
}

func readEventsReadOnly(path string) ([]model.Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 128*1024)
	var events []model.Event
	var previous uint64
	var roomID string
	for lineNo := 1; ; lineNo++ {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var event model.Event
			if err := json.Unmarshal(line, &event); err != nil {
				if errors.Is(readErr, io.EOF) {
					break // crash-partial final line; scanning is deliberately read-only
				}
				return nil, fmt.Errorf("decode event log line %d: %w", lineNo, err)
			}
			if event.Seq == 0 || event.Seq != previous+1 {
				return nil, fmt.Errorf("event sequence at line %d is %d; expected %d", lineNo, event.Seq, previous+1)
			}
			if strings.TrimSpace(event.RoomID) == "" {
				return nil, fmt.Errorf("event at line %d has an empty room ID", lineNo)
			}
			if roomID == "" {
				roomID = event.RoomID
			} else if event.RoomID != roomID {
				return nil, fmt.Errorf("event at line %d belongs to room %q instead of %q", lineNo, event.RoomID, roomID)
			}
			previous = event.Seq
			events = append(events, event)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read event log: %w", readErr)
		}
	}
	return events, nil
}

func cloneRoom(room Room) Room {
	room.Bindings = cloneBindings(room.Bindings)
	return room
}

func cloneBindings(values map[model.ActorID]Binding) map[model.ActorID]Binding {
	out := make(map[model.ActorID]Binding, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func pathWithin(root, candidate string) bool {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func syncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer directory.Close()
	// Windows does not support flushing a directory handle opened by os.Open.
	// Files are synced before rename; keep the directory barrier best-effort on
	// that platform instead of turning every durable registry update into EACCES.
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

func sortedRoomIDs(values map[string]Room) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
