package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func (r *Registry) RenameRoom(ctx context.Context, roomID, name string) (Room, error) {
	if err := validateSubmittedRoomName(name, false); err != nil {
		return Room{}, err
	}
	updatedAt := r.now()
	name = strings.TrimSpace(name)
	return r.mutateRoom(ctx, roomID, EventRoomRenamed, roomRenamedPayload{Name: name, UpdatedAt: updatedAt}, roomMutationOptions{}, func(room *Room) {
		room.Name = name
		room.UpdatedAt = updatedAt
	})
}

func (r *Registry) ArchiveRoom(ctx context.Context, roomID string) (Room, error) {
	updatedAt := r.now()
	return r.mutateRoom(ctx, roomID, EventRoomArchived, roomLifecyclePayload{Lifecycle: RoomArchived, UpdatedAt: updatedAt}, roomMutationOptions{allowMissingDataDir: true}, func(room *Room) {
		room.Lifecycle = RoomArchived
		room.UpdatedAt = updatedAt
	})
}

func (r *Registry) RestoreRoom(ctx context.Context, roomID string) (Room, error) {
	updatedAt := r.now()
	return r.mutateRoom(ctx, roomID, EventRoomRestored, roomLifecyclePayload{Lifecycle: RoomActive, UpdatedAt: updatedAt}, roomMutationOptions{}, func(room *Room) {
		room.Lifecycle = RoomActive
		room.UpdatedAt = updatedAt
	})
}

type roomMutationOptions struct {
	allowMissingDataDir bool
}

func (r *Registry) mutateRoom(ctx context.Context, roomID, eventKind string, payload any, options roomMutationOptions, apply func(*Room)) (Room, error) {
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
	r.mu.RUnlock()
	if !ok {
		return Room{}, ErrRoomNotFound
	}
	if eventKind == EventRoomArchived && room.Archived() {
		return cloneRoom(room), nil
	}
	if eventKind == EventRoomRestored && !room.Archived() {
		return cloneRoom(room), nil
	}

	if eventKind == EventRoomRenamed && room.Name == payload.(roomRenamedPayload).Name {
		return cloneRoom(room), nil
	}
	eventCommitted := true
	if err := appendServiceEvent(room, eventKind, payload); err != nil {
		// Archive is the mandatory safety gate before permanent removal. If the
		// entire Room data directory was already lost outside PairRoom, writing an
		// archive event must not recreate a partial, unverifiable Event Log. Keep
		// this exception narrow: rename, restore, missing files inside an existing
		// directory, and every other filesystem error still fail closed.
		if eventKind != EventRoomArchived || !options.allowMissingDataDir || room.HostMode == model.HostNative {
			return Room{}, err
		}
		missing, inspectErr := roomDataDirMissing(room.DataDir)
		if inspectErr != nil {
			return Room{}, errors.Join(err, inspectErr)
		}
		if !missing {
			return Room{}, err
		}
		eventCommitted = false
	}

	original := cloneRoom(room)
	apply(&room)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rooms[room.ID] = cloneRoom(room)
	published, err := r.writeCheckpointLocked()
	if err != nil {
		if !eventCommitted && !published {
			// No Event Log fact was committed, and checkpoint publication never
			// became visible, so the in-memory transition is fully reversible.
			r.rooms[room.ID] = original
			return Room{}, fmt.Errorf("persist Room archive for missing data directory: %w", err)
		}
		if !eventCommitted {
			return Room{}, r.poisonLocked(fmt.Errorf("Room archive checkpoint was replaced without a source Event Log but directory sync failed: %w", err))
		}
		// The Room Event Log is already committed. Retain the new projection and
		// fail closed if the disposable checkpoint cannot be made consistent.
		return Room{}, r.poisonLocked(fmt.Errorf("room event %s committed but registry checkpoint failed: %w", eventKind, err))
	}
	return cloneRoom(room), nil
}

func roomDataDirMissing(path string) (bool, error) {
	_, err := os.Lstat(filepath.Clean(path))
	if err == nil {
		return false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	return false, fmt.Errorf("inspect Room data directory: %w", err)
}

func appendServiceEvent(room Room, kind string, payload any) error {
	eventStore, err := store.OpenExistingForRoom(room.DataDir, room.ID)
	if err != nil {
		return fmt.Errorf("open room event log: %w", err)
	}
	defer eventStore.Close()
	event, err := model.NewEvent(room.ID, kind, model.ActorSystem, payload)
	if err != nil {
		return err
	}
	if err := eventStore.Append(&event); err != nil {
		return fmt.Errorf("append room lifecycle event: %w", err)
	}
	return nil
}
