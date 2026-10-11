package service

import (
	"context"
	"errors"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/store"
)

// revokeLANMemberOffline runs only inside the archive lifecycle barrier, after
// the RuntimeManager has released the Room writer. Missing Project files need
// not prevent archive, but the existing Event Log must remain readable and
// writable: the same durable revocation and Registry projection used by a live
// Engine are required before archive can commit. No Runtime, listener, native
// capability or workspace access is started by this path.
func (s *ManagementServer) revokeLANMemberOffline(ctx context.Context, roomID string) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.registry.mu.RLock()
	err := s.registry.healthyLocked()
	room, ok := s.registry.rooms[roomID]
	room = cloneRoom(room)
	s.registry.mu.RUnlock()
	if err != nil {
		return err
	}
	if !ok || room.HostMode != model.HostNative || room.Sharing != "lan" || room.Archived() {
		return ErrRoomNotFound
	}
	if s.runtimes.ownsEventLog(roomID) {
		return ErrRoomLogOwnedByRuntime
	}
	log, err := store.OpenExistingForRoom(room.DataDir, room.ID)
	if err != nil {
		return err
	}
	engine, err := relay.Open(relay.Config{
		RoomID: room.ID, Store: log, SharedSlot: model.OtherParticipant(room.OwnerSlot),
		Runtimes: selectionsRuntimeKinds(room.Agents), OnAppend: s.registry.observeLANAuthorization,
		CommitBinding: func(binding relay.Binding, appendFact func() error) error {
			return s.registry.commitNativeBinding(room.ID, binding, appendFact)
		},
	})
	if err != nil {
		return errors.Join(err, log.Close())
	}
	defer func() { resultErr = errors.Join(resultErr, engine.Close()) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return engine.RevokeLANMember()
}
