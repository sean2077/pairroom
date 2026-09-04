package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

type BindingProvisioner interface {
	// Provision validates exactly one binding selection. A new native binding may
	// remain pending until its first accepted input materializes the vendor ID.
	// The returned cleanup function stops any temporary validation runtime; it
	// must not delete a durable vendor session that may already exist.
	Provision(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error)
}

type selectionBindingProvisioner interface {
	ProvisionSelection(context.Context, Project, model.ActorID, BindingSpec, model.AgentSelection, model.RuntimeKind, string) (Binding, func(context.Context) error, error)
}

type defaultAgentSelectionProvider interface {
	DefaultSelections() map[model.ActorID]model.AgentSelection
}

type ProvisionerFunc func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error)

func (f ProvisionerFunc) Provision(ctx context.Context, project Project, actor model.ActorID, spec BindingSpec, dataDir string) (Binding, func(context.Context) error, error) {
	return f(ctx, project, actor, spec, dataDir)
}

func (r *Registry) ProvisionRoom(ctx context.Context, request ProvisionRequest, provisioner BindingProvisioner) (Room, error) {
	if provisioner == nil {
		return Room{}, errors.New("binding provisioner is required")
	}
	// A nil map represents the omitted JSON field and snapshots the current
	// Service defaults. A non-nil empty/partial map is an explicit submission
	// and must fail closed rather than silently filling one missing slot.
	if request.Agents == nil {
		if defaults, ok := provisioner.(defaultAgentSelectionProvider); ok {
			request.Agents = defaults.DefaultSelections()
		} else {
			request.Agents = defaultAgentSelections()
		}
	}
	selections, err := validateAgentSelections(request.Agents)
	if err != nil {
		return Room{}, err
	}
	request.Agents = selections
	if err := request.Validate(); err != nil {
		return Room{}, err
	}

	// The critical section includes vendor validation. Provisioning is not a hot
	// path, while serializing it makes the global binding uniqueness check a
	// simple, auditable transaction even under concurrent API requests.
	r.provisionMu.Lock()
	defer r.provisionMu.Unlock()

	r.mu.RLock()
	if err := r.healthyLocked(); err != nil {
		r.mu.RUnlock()
		return Room{}, err
	}
	project, ok := r.projects[request.ProjectID]
	if !ok {
		r.mu.RUnlock()
		return Room{}, ErrProjectNotFound
	}
	if !project.Available {
		r.mu.RUnlock()
		return Room{}, fmt.Errorf("project is unavailable: %s", project.Diagnostic)
	}
	for actor, spec := range request.Bindings {
		if spec.Mode != BindingExisting {
			continue
		}
		key := BindingKey{Agent: actor, SessionID: strings.TrimSpace(spec.SessionID)}
		if owner, owned := r.bindingOwners[key.String()]; owned {
			r.mu.RUnlock()
			return Room{}, fmt.Errorf("%w: %s session %q belongs to room %s", ErrBindingOwned, actor, spec.SessionID, owner)
		}
	}
	r.mu.RUnlock()

	roomID := model.NewID("room")
	stageDir, err := os.MkdirTemp(r.roomsRoot, ".provision-"+roomID+"-")
	if err != nil {
		return Room{}, fmt.Errorf("create room provisioning directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stageDir)
		}
	}()

	bindings := make(map[model.ActorID]Binding, 2)
	cleanups := make([]func(context.Context) error, 0, 2)
	cleanupAll := func() error {
		var joined error
		for index := len(cleanups) - 1; index >= 0; index-- {
			if cleanups[index] == nil {
				continue
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := cleanups[index](cleanupCtx)
			cancel()
			joined = errors.Join(joined, err)
		}
		return joined
	}

	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		spec := request.Bindings[actor]
		selection := request.Agents[actor]
		peerRuntime := request.Agents[model.OtherParticipant(actor)].Runtime
		var binding Binding
		var cleanup func(context.Context) error
		var err error
		if selected, ok := provisioner.(selectionBindingProvisioner); ok {
			binding, cleanup, err = selected.ProvisionSelection(ctx, project, actor, spec, selection, peerRuntime, stageDir)
		} else {
			binding, cleanup, err = provisioner.Provision(ctx, project, actor, spec, stageDir)
		}
		if cleanup != nil {
			cleanups = append(cleanups, cleanup)
		}
		if err != nil {
			_ = cleanupAll()
			return Room{}, fmt.Errorf("validate %s binding: %w", actor, err)
		}
		binding.Agent = actor
		binding.Mode = spec.Mode
		binding.SessionID = strings.TrimSpace(binding.SessionID)
		if binding.BoundAt.IsZero() {
			binding.BoundAt = r.now()
		}
		if binding.Pending && (spec.Mode != BindingNew || binding.SessionID != "") {
			_ = cleanupAll()
			return Room{}, fmt.Errorf("validate %s binding result: only a new binding without a session ID may be deferred", actor)
		}
		if err := binding.Validate(); err != nil {
			_ = cleanupAll()
			return Room{}, fmt.Errorf("validate %s binding result: %w", actor, err)
		}
		if spec.Mode == BindingExisting && binding.SessionID != strings.TrimSpace(spec.SessionID) {
			_ = cleanupAll()
			return Room{}, fmt.Errorf("%s resumed session %q instead of requested session %q", actor, binding.SessionID, spec.SessionID)
		}
		bindings[actor] = binding
	}
	if err := cleanupAll(); err != nil {
		return Room{}, fmt.Errorf("stop temporary binding validators: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Room{}, fmt.Errorf("room provisioning canceled before commit: %w", err)
	}

	// No vendor call is allowed after this second ownership check. Combined with
	// provisionMu, the check is the transaction's binding-reservation point.
	r.mu.RLock()
	if err := r.healthyLocked(); err != nil {
		r.mu.RUnlock()
		return Room{}, err
	}
	for _, binding := range bindings {
		if !binding.OwnsIdentity() {
			continue
		}
		if owner, owned := r.bindingOwners[binding.Key().String()]; owned {
			r.mu.RUnlock()
			return Room{}, fmt.Errorf("%w: %s session %q belongs to room %s", ErrBindingOwned, binding.Agent, binding.SessionID, owner)
		}
	}
	r.mu.RUnlock()

	now := r.now()
	room := Room{
		ID: roomID, ProjectID: project.ID, Name: strings.TrimSpace(request.Name),
		Lifecycle: RoomActive, Bindings: bindings, Agents: cloneAgentSelections(request.Agents),
		TranscriptBoundaryNotice: TranscriptBoundaryNotice,
		CreatedAt:                now, UpdatedAt: now,
	}
	payload := roomProvisionedPayload{
		Schema: 2, Project: project, RoomID: room.ID, Name: room.Name,
		Lifecycle: room.Lifecycle, Bindings: cloneBindings(room.Bindings),
		Agents:                   cloneAgentSelections(room.Agents),
		TranscriptBoundaryNotice: room.TranscriptBoundaryNotice, CreatedAt: room.CreatedAt,
	}
	if err := writeInitialRoomLog(stageDir, project, room, payload); err != nil {
		return Room{}, err
	}
	if err := syncDir(stageDir); err != nil {
		return Room{}, err
	}

	finalDir := filepath.Join(r.roomsRoot, room.ID)
	if _, err := os.Stat(finalDir); err == nil {
		return Room{}, fmt.Errorf("room directory collision: %s", finalDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Room{}, fmt.Errorf("stat room destination: %w", err)
	}
	if err := os.Rename(stageDir, finalDir); err != nil {
		return Room{}, fmt.Errorf("publish room directory: %w", err)
	}
	published = true
	room.DataDir = finalDir
	if err := syncDir(r.roomsRoot); err != nil {
		// Rename may already be durable or visible. Preserve ownership in memory
		// and fail closed rather than risking a second Room claiming the binding.
		r.mu.Lock()
		_ = r.indexRoomLocked(project, room)
		err = r.poisonLocked(fmt.Errorf("room %s was published but room-root sync failed: %w", room.ID, err))
		r.mu.Unlock()
		return Room{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.indexRoomLocked(project, room); err != nil {
		return Room{}, r.poisonLocked(fmt.Errorf("room %s was published but could not be indexed: %w", room.ID, err))
	}
	if _, err := r.writeCheckpointLocked(); err != nil {
		return Room{}, r.poisonLocked(fmt.Errorf("room %s was published but registry checkpoint failed: %w", room.ID, err))
	}
	return cloneRoom(room), nil
}

func defaultAgentSelections() map[model.ActorID]model.AgentSelection {
	return map[model.ActorID]model.AgentSelection{
		model.ActorClaude: {Runtime: model.RuntimeClaude, Provider: model.NativeProviderRef(), OrdinaryReviewerPolicy: model.ReviewerEnforced},
		model.ActorCodex:  {Runtime: model.RuntimeCodex, Provider: model.NativeProviderRef(), OrdinaryReviewerPolicy: model.ReviewerEnforced},
	}
}

func writeInitialRoomLog(dir string, project Project, room Room, payload roomProvisionedPayload) error {
	eventStore, err := store.Open(dir)
	if err != nil {
		return fmt.Errorf("open staged room event log: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = eventStore.Close()
		}
	}()
	appendEvent := func(kind string, actor model.ActorID, value any) error {
		event, err := model.NewEvent(room.ID, kind, actor, value)
		if err != nil {
			return err
		}
		if err := eventStore.Append(&event); err != nil {
			return fmt.Errorf("append %s: %w", kind, err)
		}
		return nil
	}
	meta := model.RoomMeta{ID: room.ID, Name: room.Name, Repo: project.Root, CreatedAt: room.CreatedAt}
	if err := appendEvent("room.created", model.ActorSystem, meta); err != nil {
		return err
	}
	if err := appendEvent(EventRoomProvisioned, model.ActorSystem, payload); err != nil {
		return err
	}
	if err := appendEvent("room.settings.updated", model.ActorSystem, model.DefaultRoomSettings()); err != nil {
		return err
	}
	participants := []model.ParticipantSnapshot{
		{ID: model.ActorClaude, DisplayName: model.ParticipantDisplayName(model.ActorClaude, room.Agents[model.ActorClaude].Runtime), Role: model.RoleDriver, State: model.StateStopped, Model: room.Agents[model.ActorClaude].Model, RuntimeKind: room.Agents[model.ActorClaude].Runtime, SessionID: room.Bindings[model.ActorClaude].SessionID},
		{ID: model.ActorCodex, DisplayName: model.ParticipantDisplayName(model.ActorCodex, room.Agents[model.ActorCodex].Runtime), Role: model.RoleReviewer, State: model.StateStopped, Model: room.Agents[model.ActorCodex].Model, RuntimeKind: room.Agents[model.ActorCodex].Runtime, SessionID: room.Bindings[model.ActorCodex].SessionID},
	}
	for _, participant := range participants {
		if err := appendEvent("participant.updated", participant.ID, participant); err != nil {
			return err
		}
	}
	if err := eventStore.Close(); err != nil {
		return fmt.Errorf("close staged room event log: %w", err)
	}
	closed = true
	return nil
}
