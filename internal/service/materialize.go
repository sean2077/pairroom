package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

type materializationAppender func(kind string, payload any) error

// MaterializeBinding converts one deferred new binding into immutable vendor
// ownership after the live Room adapter has accepted its first native input.
// The caller supplies the active Engine's append path so the Room Event Log
// retains exactly one writer while the runtime is open.
func (r *Registry) MaterializeBinding(ctx context.Context, roomID string, actor model.ActorID, sessionID string, appendFact materializationAppender) (Room, error) {
	if !actor.ValidParticipant() {
		return Room{}, fmt.Errorf("invalid materialization actor %q", actor)
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Room{}, errors.New("materialized session ID is required")
	}
	if len(sessionID) > 512 || strings.ContainsAny(sessionID, "\r\n\x00") {
		return Room{}, errors.New("materialized session ID is invalid")
	}
	if appendFact == nil {
		return Room{}, errors.New("materialization event appender is required")
	}
	if err := ctx.Err(); err != nil {
		return Room{}, err
	}

	r.provisionMu.Lock()
	defer r.provisionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return Room{}, err
	}

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
	if room.Archived() {
		r.mu.RUnlock()
		return Room{}, errors.New("archived Room cannot materialize a binding")
	}
	binding, ok := room.Bindings[actor]
	if !ok {
		r.mu.RUnlock()
		return Room{}, fmt.Errorf("Room is missing %s binding", actor)
	}
	if !binding.Pending {
		r.mu.RUnlock()
		if binding.SessionID == sessionID {
			return cloneRoom(room), nil
		}
		return Room{}, fmt.Errorf("%s binding %q is durable and cannot be replaced with %q", actor, binding.SessionID, sessionID)
	}
	if binding.Mode != BindingNew {
		r.mu.RUnlock()
		return Room{}, fmt.Errorf("%w: %s requires explicit binding completion", ErrRoomBindingPending, actor)
	}
	key := BindingKey{Agent: actor, SessionID: sessionID}
	if owner, owned := r.bindingOwners[key.String()]; owned && owner != room.ID {
		r.mu.RUnlock()
		return Room{}, fmt.Errorf("%w: %s session %q belongs to room %s", ErrBindingOwned, actor, sessionID, owner)
	}
	room = cloneRoom(room)
	r.mu.RUnlock()

	now := r.now()
	binding.SessionID = sessionID
	binding.Pending = false
	binding.BoundAt = now
	if err := binding.Validate(); err != nil {
		return Room{}, fmt.Errorf("validate materialized %s binding: %w", actor, err)
	}
	payload := roomBindingMaterializedPayload{Binding: binding, UpdatedAt: now}
	if err := appendFact(EventRoomBindingMaterialized, payload); err != nil {
		return Room{}, fmt.Errorf("append %s binding materialization: %w", actor, err)
	}

	room.Bindings[actor] = binding
	room.UpdatedAt = now
	r.mu.Lock()
	defer r.mu.Unlock()
	// The Room Event Log is already committed. Retain the ownership projection
	// and fail closed if the disposable checkpoint cannot be synchronized.
	r.bindingOwners[key.String()] = room.ID
	r.rooms[room.ID] = cloneRoom(room)
	if _, err := r.writeCheckpointLocked(); err != nil {
		return Room{}, r.poisonLocked(fmt.Errorf("%s binding materialized but registry checkpoint failed: %w", actor, err))
	}
	return cloneRoom(room), nil
}
