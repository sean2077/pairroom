package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// CompleteBindings performs the explicit migration step for a discovered
// legacy Room whose old event log did not persist one or both native session
// IDs. It never replaces an already-owned binding. Both pending sides are
// validated before a single durable completion event is appended.
func (r *Registry) CompleteBindings(ctx context.Context, roomID string, specs map[model.ActorID]BindingSpec, provisioner BindingProvisioner) (Room, error) {
	if provisioner == nil {
		return Room{}, errors.New("binding provisioner is required")
	}
	r.provisionMu.Lock()
	defer r.provisionMu.Unlock()

	r.mu.RLock()
	if err := r.healthyLocked(); err != nil {
		r.mu.RUnlock()
		return Room{}, err
	}
	room, ok := r.rooms[roomID]
	if !ok {
		r.mu.RUnlock()
		return Room{}, ErrRoomNotFound
	}
	project, ok := r.projects[room.ProjectID]
	if !ok {
		r.mu.RUnlock()
		return Room{}, ErrProjectNotFound
	}
	if !project.Available {
		r.mu.RUnlock()
		return Room{}, fmt.Errorf("project is unavailable: %s", project.Diagnostic)
	}
	pending := make([]model.ActorID, 0, 2)
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		binding, exists := room.Bindings[actor]
		if !exists || binding.Pending {
			pending = append(pending, actor)
		}
	}
	if len(pending) == 0 {
		r.mu.RUnlock()
		if len(specs) != 0 {
			return Room{}, errors.New("Room has no pending bindings; existing bindings cannot be replaced")
		}
		return cloneRoom(room), nil
	}
	pendingSet := make(map[model.ActorID]struct{}, len(pending))
	for _, actor := range pending {
		pendingSet[actor] = struct{}{}
		spec, exists := specs[actor]
		if !exists {
			r.mu.RUnlock()
			return Room{}, fmt.Errorf("%s pending binding is required", actor)
		}
		if err := spec.Validate(); err != nil {
			r.mu.RUnlock()
			return Room{}, fmt.Errorf("%s binding: %w", actor, err)
		}
		if spec.Mode == BindingExisting {
			key := BindingKey{Agent: actor, SessionID: strings.TrimSpace(spec.SessionID)}
			if owner, owned := r.bindingOwners[key.String()]; owned && owner != room.ID {
				r.mu.RUnlock()
				return Room{}, fmt.Errorf("%w: %s session %q belongs to room %s", ErrBindingOwned, actor, spec.SessionID, owner)
			}
		}
	}
	for actor := range specs {
		if _, allowed := pendingSet[actor]; !allowed {
			r.mu.RUnlock()
			return Room{}, fmt.Errorf("%s binding is already durable and cannot be replaced", actor)
		}
	}
	r.mu.RUnlock()

	validationDir, err := os.MkdirTemp(r.root, ".binding-validation-")
	if err != nil {
		return Room{}, fmt.Errorf("create binding validation directory: %w", err)
	}
	defer os.RemoveAll(validationDir)

	validated := make(map[model.ActorID]Binding, len(pending))
	cleanups := make([]func(context.Context) error, 0, len(pending))
	cleanupAll := func() error {
		var result error
		for index := len(cleanups) - 1; index >= 0; index-- {
			if cleanups[index] == nil {
				continue
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := cleanups[index](cleanupCtx)
			cancel()
			result = errors.Join(result, err)
		}
		return result
	}
	for _, actor := range pending {
		spec := specs[actor]
		binding, cleanup, err := provisioner.Provision(ctx, project, actor, spec, validationDir)
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
		binding.Pending = false
		if binding.BoundAt.IsZero() {
			binding.BoundAt = r.now()
		}
		if err := binding.Validate(); err != nil {
			_ = cleanupAll()
			return Room{}, fmt.Errorf("validate %s binding result: %w", actor, err)
		}
		if spec.Mode == BindingExisting && binding.SessionID != strings.TrimSpace(spec.SessionID) {
			_ = cleanupAll()
			return Room{}, fmt.Errorf("%s resumed session %q instead of requested session %q", actor, binding.SessionID, spec.SessionID)
		}
		validated[actor] = binding
	}
	if err := cleanupAll(); err != nil {
		return Room{}, fmt.Errorf("stop temporary binding validators: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Room{}, fmt.Errorf("binding completion canceled before commit: %w", err)
	}

	// Recheck ownership after vendor validation. provisionMu serializes all Room
	// provisioning and binding-completion transactions in this process.
	r.mu.RLock()
	if err := r.healthyLocked(); err != nil {
		r.mu.RUnlock()
		return Room{}, err
	}
	for _, binding := range validated {
		if owner, owned := r.bindingOwners[binding.Key().String()]; owned && owner != room.ID {
			r.mu.RUnlock()
			return Room{}, fmt.Errorf("%w: %s session %q belongs to room %s", ErrBindingOwned, binding.Agent, binding.SessionID, owner)
		}
	}
	r.mu.RUnlock()

	updated := cloneRoom(room)
	for actor, binding := range validated {
		updated.Bindings[actor] = binding
	}
	updated.UpdatedAt = r.now()
	payload := roomBindingsCompletedPayload{Bindings: cloneBindings(updated.Bindings), UpdatedAt: updated.UpdatedAt}
	if err := appendServiceEvent(room, EventRoomBindingsCompleted, payload); err != nil {
		return Room{}, fmt.Errorf("append binding completion event: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, binding := range validated {
		r.bindingOwners[binding.Key().String()] = room.ID
	}
	r.rooms[room.ID] = cloneRoom(updated)
	if _, err := r.writeCheckpointLocked(); err != nil {
		return Room{}, r.poisonLocked(fmt.Errorf("binding completion committed but registry checkpoint failed: %w", err))
	}
	return cloneRoom(updated), nil
}
