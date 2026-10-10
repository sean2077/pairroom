package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/relay"
)

func nativeRegistryBinding(b relay.Binding) Binding {
	result := Binding{Agent: b.Slot, Mode: BindingNew, Pending: true, BoundAt: b.LastActivity}
	if b.RemoteKey != "" {
		result.RemoteKey = b.RemoteKey
		result.Pending = !b.Active
		return result
	}
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
	if candidate.HostMode == model.HostNative && r.identities != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		probe := nativeidentity.Claim{Runtime: runtime, SessionID: session, Association: nativeidentity.Hosted(r.root, candidate.ID, slot), BindID: "preflight"}
		if err := r.identities.Available(ctx, probe); err != nil {
			return fmt.Errorf("%w: %v", ErrBindingOwned, err)
		}
	}
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
	key := nativeClaimKey(roomID, b.Slot)
	var previous *nativeidentity.Claim
	if c, ok := r.nativeClaims[key]; ok {
		previous = &c
	}
	next := r.nativeIdentityClaim(room, b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := r.identities.Commit(ctx, previous, next, func() error {
		if err := appendFact(); err != nil {
			return err
		}
		room = cloneRoom(room)
		room.Bindings[b.Slot] = nativeRegistryBinding(b)
		if b.RemoteKey != "" {
			room.Agents[b.Slot] = model.AgentSelection{Runtime: b.Runtime, Provider: model.NativeProviderRef()}
		}
		room.UpdatedAt = r.now()
		r.rooms[roomID] = room
		if _, err := r.writeCheckpointLocked(); err != nil {
			return r.poisonLocked(fmt.Errorf("native binding committed but checkpoint failed: %w", err))
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, nativeidentity.ErrOwned) || errors.Is(err, nativeidentity.ErrUnowned) {
			return fmt.Errorf("%w: %v", ErrBindingOwned, err)
		}
		return err
	}
	if next == nil {
		delete(r.nativeClaims, key)
	} else {
		r.nativeClaims[key] = *next
	}
	return nil
}

func nativeClaimKey(room string, slot model.ActorID) string { return room + "/" + string(slot) }

func (r *Registry) nativeIdentityClaim(room Room, b relay.Binding) *nativeidentity.Claim {
	if room.HostMode != model.HostNative || b.RemoteKey != "" || !b.Active || b.SessionID == "" {
		return nil
	}
	return &nativeidentity.Claim{Runtime: room.Agents[b.Slot].Runtime.CanonicalForSlot(b.Slot), SessionID: b.SessionID, Association: nativeidentity.Hosted(r.root, room.ID, b.Slot), BindID: b.BindID, Generation: b.Generation}
}

// Register live and archived hosted Native bindings before this Registry can
// admit new sessions. A stopped Service retains these private reservations.
func (r *Registry) reserveNativeIdentities(ctx context.Context) error {
	for _, room := range r.rooms {
		if room.HostMode != model.HostNative {
			continue
		}
		for slot, b := range room.Bindings {
			if !b.OwnsIdentity() {
				continue
			}
			key := nativeClaimKey(room.ID, slot)
			claim, ok := r.nativeClaims[key]
			if !ok {
				// Checkpoint-only archived Rooms still own their explicit session
				// identity. This reserves it without inventing relay credentials.
				claim = nativeidentity.Claim{Runtime: room.Agents[slot].Runtime.CanonicalForSlot(slot), SessionID: b.SessionID, Association: nativeidentity.Hosted(r.root, room.ID, slot), BindID: "archived-checkpoint"}
				retained, exists, err := r.identities.Retained(ctx, claim)
				if err != nil {
					return fmt.Errorf("%w: archived native identity conflicts with another local association: %v", ErrBindingOwned, err)
				}
				if exists {
					claim = retained
				}
			}
			if err := r.identities.Reserve(ctx, claim); err != nil {
				return fmt.Errorf("%w: hosted native identity conflicts with another local association: %v", ErrBindingOwned, err)
			}
			r.nativeClaims[key] = claim
		}
	}
	// A crash can land after a binding/unbind fact commits but before the old
	// cross-process claim is released. Only a later validated Room fact is
	// sufficient proof to reconcile that exact superseded operation.
	for claim := range r.nativeRetired {
		if _, err := r.identities.ReleaseIfHeld(ctx, claim); err != nil {
			return err
		}
		delete(r.nativeRetired, claim)
	}
	return nil
}

func (r *Registry) releaseNativeIdentitiesLocked(ctx context.Context, room Room) error {
	for _, slot := range []model.ActorID{model.ActorSlot1, model.ActorSlot2} {
		key := nativeClaimKey(room.ID, slot)
		if claim, ok := r.nativeClaims[key]; ok {
			if _, err := r.identities.ReleaseIfHeld(ctx, claim); err != nil {
				return err
			}
			delete(r.nativeClaims, key)
		}
		association := nativeidentity.Hosted(r.root, room.ID, slot)
		for claim := range r.nativeRetired {
			if claim.Association == association {
				if _, err := r.identities.ReleaseIfHeld(ctx, claim); err != nil {
					return err
				}
				delete(r.nativeRetired, claim)
			}
		}
	}
	return nil
}
