package service

import (
	"errors"
	"fmt"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func nativeRegistryBinding(b relay.Binding) Binding {
	result := Binding{Agent: b.Slot, Mode: BindingNew, Pending: true, BoundAt: b.LastActivity}
	if b.Active && b.SessionID != "" {
		result.Pending = false
		result.SessionID = b.SessionID
	}
	return result
}

// checkNativeIdentityLocked checks runtime identity, not historical slot labels.
// Existing embedded indexing stays compatible, while either side of a native /
// embedded collision must fail closed, including duplicate-runtime slots.
func (r *Registry) checkNativeIdentityLocked(candidate Room, slot model.ActorID, session string) error {
	if session == "" {
		return nil
	}
	runtime := candidate.Agents[slot].Runtime.CanonicalForSlot(slot)
	check := func(other Room) error {
		if candidate.HostMode != model.HostNative && other.HostMode != model.HostNative {
			return nil
		}
		for actor, b := range other.Bindings {
			if candidate.ID != "" && other.ID == candidate.ID && actor == slot {
				continue
			}
			if b.OwnsIdentity() && b.SessionID == session && other.Agents[actor].Runtime.CanonicalForSlot(actor) == runtime {
				return fmt.Errorf("%w: native session is already associated with another Room slot", ErrBindingOwned)
			}
		}
		return nil
	}
	if err := check(candidate); err != nil {
		return err
	}
	for _, other := range r.rooms {
		if err := check(other); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) commitNativeBinding(roomID string, b relay.Binding, appendFact func() error) error {
	r.provisionMu.Lock()
	defer r.provisionMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.healthyLocked(); err != nil {
		return err
	}
	room, ok := r.rooms[roomID]
	if !ok {
		return ErrRoomNotFound
	}
	if room.HostMode != model.HostNative || room.Archived() {
		return errors.New("native binding requires an active native Room")
	}
	if err := r.checkNativeIdentityLocked(room, b.Slot, b.SessionID); err != nil {
		return err
	}
	if err := appendFact(); err != nil {
		return err
	}
	room = cloneRoom(room)
	room.Bindings[b.Slot] = nativeRegistryBinding(b)
	room.UpdatedAt = r.now()
	r.rooms[roomID] = room
	if _, err := r.writeCheckpointLocked(); err != nil {
		return r.poisonLocked(fmt.Errorf("native binding committed but checkpoint failed: %w", err))
	}
	return nil
}
