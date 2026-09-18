package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/version"
)

var (
	ErrProjectAlreadyRegistered = errors.New("project is already registered")
	ErrProjectNotFound          = errors.New("project not found")
	ErrRoomNotFound             = errors.New("room not found")
	ErrBindingOwned             = errors.New("binding identity is already owned")
	ErrRegistryFailClosed       = errors.New("service registry is fail-closed")
)

const roomDeletionQuarantineName = ".deleted-rooms"

const registryCheckpointSchema = 3

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
	poisoned      error

	roomDeletionFS                roomDeletionFS
	roomDeletionCleanupDiagnostic string
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
	if err := preflightRegistryRoot(root); err != nil {
		return nil, err
	}
	roomsRoot := filepath.Join(root, "rooms")
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
		roomDeletionFS:   defaultRoomDeletionFS(),
	}
	// Validate a current checkpoint before creating a root/rooms directory or
	// performing any recovery mutation. A retired or malformed root is never
	// repaired, replayed, rewritten, or even completed with missing directories.
	if err := registry.loadCheckpointProjects(); err != nil {
		return nil, fmt.Errorf("load service registry checkpoint: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create service data root: %w", err)
	}
	if err := os.MkdirAll(roomsRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create room data root: %w", err)
	}
	// Keep the deletion quarantine lazy. A fresh Registry should not gain an
	// otherwise unexplained data directory until the first permanent Room
	// removal. Startup recovery validates and scans it only when it already
	// exists.
	// Resolve crash-interrupted Room deletions only after the root and checkpoint
	// have passed the retirement boundary.
	if err := registry.recoverRoomDeletionQuarantine(ctx); err != nil {
		return nil, fmt.Errorf("recover Room deletion quarantine: %w", err)
	}
	// An archived Room whose directory was lost remains visible for explicit
	// cleanup. Only a validated checkpoint can recover its identity.
	if err := registry.recoverMissingArchivedRoomsFromCheckpoint(); err != nil {
		return nil, fmt.Errorf("recover archived Rooms with missing data: %w", err)
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
	return RegistrySnapshot{Schema: registryCheckpointSchema, GeneratedAt: r.now(), Projects: projects, Rooms: rooms}.Sorted()
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

// preflightRegistryRoot is intentionally read-only and runs before OpenRegistry
// creates directories, repairs deletion quarantine, replays events, or rewrites
// a checkpoint. A schema-2 root is retired as one unit, including empty Project
// registrations that cannot be reconstructed from Room logs.
func preflightRegistryRoot(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Service data root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Service data root must be a direct directory")
	}
	if schema, exists, err := readSchemaHeader(filepath.Join(root, "service-registry.json")); err != nil {
		return fmt.Errorf("inspect Service registry checkpoint before recovery: %w", err)
	} else if exists && schema < registryCheckpointSchema {
		return fmt.Errorf("retired Service data root (checkpoint schema %d); start with a new data root and recreate Rooms and profiles; legacy data was not modified", schema)
	}
	if schema, exists, err := readSchemaHeader(filepath.Join(root, agentPairProfilesFile)); err != nil {
		return fmt.Errorf("inspect Agent pair profiles before recovery: %w", err)
	} else if exists && schema < 2 {
		return fmt.Errorf("retired Service data root (Agent pair profile schema %d); start with a new data root and recreate Rooms and profiles; legacy data was not modified", schema)
	}
	return preflightRoomSchemas(filepath.Join(root, "rooms"))
}

func readSchemaHeader(path string) (int, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 16<<20 {
		return 0, false, errors.New("schema file must be a bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false, err
	}
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return 0, false, err
	}
	return header.Schema, true, nil
}

func preflightRoomSchemas(roomsRoot string) error {
	info, err := os.Lstat(roomsRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Room data root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Room data root must be a direct directory")
	}
	return filepath.WalkDir(roomsRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("Room data root contains a symlink")
		}
		if entry.IsDir() || entry.Name() != "metadata.json" {
			return nil
		}
		schema, _, err := readStoreSchemaHeader(path)
		if err != nil {
			return fmt.Errorf("inspect Room metadata %s: %w", path, err)
		}
		if schema < version.StoreSchema {
			return fmt.Errorf("retired Service data root (Room store schema %d); start with a new data root and recreate Rooms and profiles; legacy data was not modified", schema)
		}
		return nil
	})
}

func readStoreSchemaHeader(path string) (int, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false, err
	}
	var metadata struct {
		Format        string `json:"format"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return 0, false, err
	}
	if metadata.Format != "pairroom-jsonl" {
		return 0, false, fmt.Errorf("unsupported event metadata format %q", metadata.Format)
	}
	return metadata.SchemaVersion, true, nil
}

func (r *Registry) readCheckpoint() (RegistrySnapshot, bool, error) {
	data, err := os.ReadFile(r.checkpoint)
	if errors.Is(err, os.ErrNotExist) {
		return RegistrySnapshot{}, false, nil
	}
	if err != nil {
		return RegistrySnapshot{}, false, err
	}
	var snapshot RegistrySnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return RegistrySnapshot{}, false, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return RegistrySnapshot{}, false, errors.New("service registry checkpoint contains trailing JSON")
	}
	if snapshot.Schema < registryCheckpointSchema {
		return RegistrySnapshot{}, false, fmt.Errorf("retired service registry checkpoint schema %d", snapshot.Schema)
	}
	if snapshot.Schema != registryCheckpointSchema {
		return RegistrySnapshot{}, false, fmt.Errorf("unsupported service registry checkpoint schema %d", snapshot.Schema)
	}
	return snapshot, true, nil
}

func (r *Registry) validatedCheckpoint() (RegistrySnapshot, map[string]Room, bool, error) {
	snapshot, exists, err := r.readCheckpoint()
	if err != nil || !exists {
		return RegistrySnapshot{}, nil, exists, err
	}
	projects := make(map[string]Project, len(snapshot.Projects))
	projectRoots := make(map[string]string, len(snapshot.Projects))
	for _, project := range snapshot.Projects {
		root := strings.TrimSpace(project.Root)
		if strings.TrimSpace(project.ID) == "" || root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || project.ID != projectID(root) {
			return RegistrySnapshot{}, nil, false, fmt.Errorf("checkpoint Project %q has an invalid identity", project.ID)
		}
		if _, duplicate := projects[project.ID]; duplicate {
			return RegistrySnapshot{}, nil, false, fmt.Errorf("checkpoint contains duplicate Project %s", project.ID)
		}
		if owner, duplicate := projectRoots[root]; duplicate && owner != project.ID {
			return RegistrySnapshot{}, nil, false, fmt.Errorf("checkpoint Project root %s has multiple identities", root)
		}
		projects[project.ID] = project
		projectRoots[root] = project.ID
	}
	rooms := make(map[string]Room, len(snapshot.Rooms))
	dataDirs := make(map[string]string, len(snapshot.Rooms))
	bindingOwners := make(map[string]string)
	for _, room := range snapshot.Rooms {
		if err := validateCheckpointRoom(room); err != nil {
			return RegistrySnapshot{}, nil, false, fmt.Errorf("checkpoint Room %q is invalid: %w", room.ID, err)
		}
		if _, ok := projects[room.ProjectID]; !ok {
			return RegistrySnapshot{}, nil, false, fmt.Errorf("checkpoint Room %s references unknown Project %s", room.ID, room.ProjectID)
		}
		if _, duplicate := rooms[room.ID]; duplicate {
			return RegistrySnapshot{}, nil, false, fmt.Errorf("checkpoint contains duplicate Room %s", room.ID)
		}
		dir := strings.TrimSpace(room.DataDir)
		if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return RegistrySnapshot{}, nil, false, fmt.Errorf("checkpoint Room %s has an invalid data directory", room.ID)
		}
		if owner, duplicate := dataDirs[dir]; duplicate && owner != room.ID {
			return RegistrySnapshot{}, nil, false, fmt.Errorf("checkpoint data directory %s belongs to multiple Rooms", dir)
		}
		relative, relErr := filepath.Rel(r.roomsRoot, dir)
		if relErr != nil || filepath.Dir(relative) != "." || validateManagedRoomSourceBase(relative) != nil {
			return RegistrySnapshot{}, nil, false, fmt.Errorf("checkpoint Room %s has an invalid managed data directory %s", room.ID, dir)
		}
		for _, binding := range room.Bindings {
			if !binding.OwnsIdentity() {
				continue
			}
			key := binding.Key().String()
			if owner, duplicate := bindingOwners[key]; duplicate && owner != room.ID {
				return RegistrySnapshot{}, nil, false, fmt.Errorf("checkpoint binding %s session %q belongs to multiple Rooms", binding.Agent, binding.SessionID)
			}
			bindingOwners[key] = room.ID
		}
		room.DataDir = dir
		rooms[room.ID] = cloneRoom(room)
		dataDirs[dir] = room.ID
	}
	return snapshot, rooms, true, nil
}

func (r *Registry) loadCheckpointProjects() error {
	snapshot, _, exists, err := r.validatedCheckpoint()
	if err != nil || !exists {
		return err
	}
	for _, project := range snapshot.Projects {
		r.projects[project.ID] = project
		r.projectByRoot[project.Root] = project.ID
	}
	return nil
}

func (r *Registry) scanRooms(ctx context.Context) error {
	entries, err := os.ReadDir(r.roomsRoot)
	if err != nil {
		return fmt.Errorf("scan room data root: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == roomDeletionQuarantineName {
			continue
		}
		dir := filepath.Join(r.roomsRoot, entry.Name())
		if strings.HasPrefix(entry.Name(), ".provision-") {
			// A staging directory is never a published Room. It can remain only
			// after process death, so removing it cannot delete visible history.
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("remove stale room provisioning directory %s: %w", entry.Name(), err)
			}
			continue
		}
		for _, existing := range r.rooms {
			if filepath.Clean(existing.DataDir) == dir {
				// A previously absent checkpoint-only path has reappeared. Do not
				// reinterpret replacement bytes as the missing Room's history.
				return fmt.Errorf("recovered missing Room data path reappeared: %s", dir)
			}
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
	}
	return nil
}

func (r *Registry) readRoomFacts(ctx context.Context, dir string) (Room, Project, bool, error) {
	if err := ctx.Err(); err != nil {
		return Room{}, Project{}, false, err
	}
	eventPath := filepath.Join(dir, "events.jsonl")
	if _, err := os.Stat(eventPath); errors.Is(err, os.ErrNotExist) {
		return Room{}, Project{}, false, nil
	} else if err != nil {
		return Room{}, Project{}, false, err
	}
	// Validate the current schema before reading Event Log bytes. Discovery and
	// activation share the same boundary; neither imports nor migrates old data.
	storeSchema, err := readRoomStoreSchema(dir)
	if err != nil {
		return Room{}, Project{}, false, err
	}
	events, err := readEventsReadOnly(eventPath)
	if err != nil {
		return Room{}, Project{}, false, err
	}
	if len(events) == 0 {
		return Room{}, Project{}, false, nil
	}

	var provisioned *roomProvisionedPayload
	materializedBindings := make(map[model.ActorID]Binding, 2)
	nativeBindings := make(map[model.ActorID]relay.Binding, 2)
	var meta model.RoomMeta
	var lifecycle RoomLifecycle = RoomActive
	var renamed string
	var updatedAt time.Time
	createdSeen := false
	serviceMutationSeen := false
	for _, event := range events {
		if !validDurableEventActor(event.Actor) {
			return Room{}, Project{}, false, fmt.Errorf("event %d has a retired or invalid actor %q", event.Seq, event.Actor)
		}
		if strings.HasPrefix(event.Kind, "native.") {
			if provisioned == nil || provisioned.HostMode != model.HostNative || lifecycle == RoomArchived {
				return Room{}, Project{}, false, errors.New("native event requires an active native Room")
			}
			switch event.Kind {
			case relay.EventBinding, relay.EventMessage, relay.EventPublication, relay.EventPublicationGap, relay.EventFailure, relay.EventWakeConfig, relay.EventWakeReserved, relay.EventWakeAttempted:
			default:
				return Room{}, Project{}, false, fmt.Errorf("unsupported native event %q", event.Kind)
			}
		}
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
			if serviceMutationSeen {
				return Room{}, Project{}, false, errors.New("room service mutation precedes provisioning")
			}
			var payload roomProvisionedPayload
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("decode %s event %d: %w", event.Kind, event.Seq, err)
			}
			if payload.Schema < 5 {
				return Room{}, Project{}, false, fmt.Errorf("retired room provisioning schema %d; recreate the Room", payload.Schema)
			}
			if payload.Schema != 5 {
				return Room{}, Project{}, false, fmt.Errorf("unsupported room provisioning schema %d", payload.Schema)
			}
			if storeSchema != version.StoreSchema {
				return Room{}, Project{}, false, errors.New("store/provisioning schema mismatch: require 12/5")
			}
			if !payload.HostMode.Valid() {
				return Room{}, Project{}, false, errors.New("provisioning 5 requires explicit host_mode")
			}
			if payload.Collaboration == nil {
				return Room{}, Project{}, false, errors.New("provisioning requires collaboration instructions")
			}
			if err := payload.Collaboration.Validate(); err != nil {
				return Room{}, Project{}, false, err
			}
			if _, err := validateAgentSelections(payload.Agents); err != nil {
				return Room{}, Project{}, false, fmt.Errorf("invalid provisioned Agent selections: %w", err)
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
		case "service.room.bindings.completed", "service.legacy.imported":
			return Room{}, Project{}, false, fmt.Errorf("unsupported retired Room event %q", event.Kind)
		case relay.EventBinding:
			if provisioned == nil || provisioned.HostMode != model.HostNative || lifecycle == RoomArchived {
				return Room{}, Project{}, false, errors.New("native binding requires an active native Room")
			}
			b, err := relay.BindingFromEvent(event)
			if err != nil {
				return Room{}, Project{}, false, err
			}
			if event.Actor != b.Slot {
				return Room{}, Project{}, false, errors.New("native binding actor mismatch")
			}
			prior := nativeBindings[b.Slot]
			if b.Generation < prior.Generation || (b.Generation == prior.Generation && prior.BindID != "" && b.BindID != prior.BindID) {
				return Room{}, Project{}, false, errors.New("native binding generation regressed")
			}
			nativeBindings[b.Slot] = b
		case EventRoomBindingMaterialized:
			if provisioned != nil && provisioned.HostMode == model.HostNative {
				return Room{}, Project{}, false, errors.New("native Rooms require hook association, not embedded materialization")
			}
			if event.Actor != model.ActorSystem {
				return Room{}, Project{}, false, fmt.Errorf("%s event %d must be authored by system", event.Kind, event.Seq)
			}
			if !createdSeen || provisioned == nil {
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
			initial, ok := provisioned.Bindings[binding.Agent]
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
		}
	}

	if provisioned == nil {
		return Room{}, Project{}, false, errors.New("Room has no service.room.provisioned event; standalone Room stores are unsupported")
	}
	payload := *provisioned
	if meta.ID != payload.RoomID {
		return Room{}, Project{}, false, fmt.Errorf("room.created ID %q conflicts with provisioned ID %q", meta.ID, payload.RoomID)
	}
	if filepath.Clean(meta.Repo) != filepath.Clean(payload.Project.Root) {
		return Room{}, Project{}, false, fmt.Errorf("room.created repository %q conflicts with provisioned Project root %q", meta.Repo, payload.Project.Root)
	}
	if strings.TrimSpace(meta.Name) != strings.TrimSpace(payload.Name) {
		return Room{}, Project{}, false, fmt.Errorf("room.created name %q conflicts with provisioned name %q", meta.Name, payload.Name)
	}
	if !meta.CreatedAt.Equal(payload.CreatedAt) {
		return Room{}, Project{}, false, fmt.Errorf("room.created time %s conflicts with provisioned time %s", meta.CreatedAt, payload.CreatedAt)
	}
	if meta.Collaboration == nil || *meta.Collaboration != *payload.Collaboration {
		return Room{}, Project{}, false, errors.New("room.created collaboration conflicts with provisioning")
	}
	room := Room{
		HostMode:                 payload.HostMode,
		Collaboration:            model.CloneCollaboration(payload.Collaboration),
		ID:                       payload.RoomID,
		ProjectID:                payload.Project.ID,
		Name:                     payload.Name,
		DataDir:                  dir,
		Lifecycle:                lifecycle,
		Bindings:                 cloneBindings(payload.Bindings),
		Agents:                   cloneAgentSelections(payload.Agents),
		TranscriptBoundaryNotice: payload.TranscriptBoundaryNotice,
		CreatedAt:                payload.CreatedAt,
		UpdatedAt:                updatedAt,
	}
	if renamed != "" {
		room.Name = renamed
	}
	for actor, binding := range materializedBindings {
		room.Bindings[actor] = binding
	}
	for actor, binding := range nativeBindings {
		room.Bindings[actor] = nativeRegistryBinding(binding)
	}
	if room.UpdatedAt.IsZero() {
		room.UpdatedAt = room.CreatedAt
	}
	if err := room.Validate(); err != nil {
		return Room{}, Project{}, false, err
	}
	return room, payload.Project, true, nil
}

func validateRoomStoreMetadata(dir string) error { _, err := readRoomStoreSchema(dir); return err }

func readRoomStoreSchema(dir string) (int, error) {
	path := filepath.Join(dir, "metadata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read event metadata: %w", err)
	}
	var metadata struct {
		Format        string `json:"format"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return 0, fmt.Errorf("decode event metadata: %w", err)
	}
	if metadata.Format != "pairroom-jsonl" {
		return 0, fmt.Errorf("unsupported event metadata format %q", metadata.Format)
	}
	if metadata.SchemaVersion < version.StoreSchema {
		return 0, fmt.Errorf("retired event store schema %d; recreate the Room", metadata.SchemaVersion)
	}
	if !version.SupportsStoreSchema(metadata.SchemaVersion) {
		return 0, fmt.Errorf("event store schema %d is unsupported; this build requires schema %d", metadata.SchemaVersion, version.StoreSchema)
	}
	return metadata.SchemaVersion, nil
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
	for _, actor := range []model.ActorID{model.ActorSlot1, model.ActorSlot2} {
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
		if actor != model.ActorSlot1 && actor != model.ActorSlot2 {
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

func (r *Registry) indexRoomLocked(project Project, room Room) error {
	// Preflight every conflict before changing any map, so a failed Room
	// registration cannot leave a partial Project or Binding reservation behind.
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
	for actor, binding := range room.Bindings {
		if err := r.checkNativeIdentityLocked(room, actor, binding.SessionID); err != nil {
			return err
		}
		if !indexesAdapterBindingOwnership(room, binding) {
			continue
		}
		if owner, ok := r.bindingOwners[binding.Key().String()]; ok && owner != room.ID {
			return fmt.Errorf("%w: %s session %q is claimed by rooms %s and %s", ErrBindingOwned, binding.Agent, binding.SessionID, owner, room.ID)
		}
	}

	if existing, ok := r.projects[project.ID]; ok {
		if !existing.Available && project.Available {
			r.projects[project.ID] = project
		}
	} else {
		r.projects[project.ID] = project
	}
	r.projectByRoot[project.Root] = project.ID
	for _, binding := range room.Bindings {
		if indexesAdapterBindingOwnership(room, binding) {
			r.bindingOwners[binding.Key().String()] = room.ID
		}
	}
	r.rooms[room.ID] = cloneRoom(room)
	return nil
}

func (r *Registry) writeCheckpointLocked() (bool, error) {
	snapshot := RegistrySnapshot{Schema: registryCheckpointSchema, GeneratedAt: r.now()}
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

func validDurableEventActor(actor model.ActorID) bool {
	return actor == model.ActorUser || actor == model.ActorSystem || actor.ValidParticipant()
}

func validateCheckpointRoom(room Room) error {
	if err := room.Validate(); err != nil {
		return err
	}
	if len(room.RuntimeNames) != 2 {
		return errors.New("checkpoint Room must contain exactly two canonical runtime_names")
	}
	for _, actor := range model.SlotActors() {
		name := strings.TrimSpace(room.RuntimeNames[actor])
		if name == "" {
			return fmt.Errorf("checkpoint Room runtime_names is missing %s", actor)
		}
	}
	for actor := range room.RuntimeNames {
		if !actor.ValidParticipant() {
			return fmt.Errorf("checkpoint Room runtime_names contains retired or invalid slot %q", actor)
		}
	}
	return nil
}

func cloneRoom(room Room) Room {
	room.Collaboration = model.CloneCollaboration(room.Collaboration)
	room.Bindings = cloneBindings(room.Bindings)
	room.Agents = cloneAgentSelections(room.Agents)
	room.RuntimeNames = make(map[model.ActorID]string, 2)
	for _, actor := range []model.ActorID{model.ActorSlot1, model.ActorSlot2} {
		room.RuntimeNames[actor] = model.NativeSessionName(room.ID, room.Name, actor, room.Agents[actor].Runtime, room.Agents[model.OtherParticipant(actor)].Runtime)
	}
	return room
}

func cloneBindings(values map[model.ActorID]Binding) map[model.ActorID]Binding {
	out := make(map[model.ActorID]Binding, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
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
