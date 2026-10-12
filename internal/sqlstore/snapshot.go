package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sean2077/pairroom/internal/model"
)

// RoomSnapshot is one authenticated, fence-held, lock-consistent read of a
// Room: the authoritative room row (host mode, sequence, revision) plus the
// complete contiguous event log.
type RoomSnapshot struct {
	RoomID   string
	HostMode string
	Events   []model.Event
	Seq      uint64
	Revision uint64
}

// ReadonlySnapshot serves the authenticated read side. It runs under the same
// ownership fence and live user/membership authorization as the commit path,
// takes only shared locks in the documented global order, writes nothing and
// never reaps. Refusals (authorization, room_not_found, state_invalid,
// lease_lost) disclose zero content; an incomplete read is an unknown-class
// error, never a partial snapshot.
func (s *Store) ReadonlySnapshot(ctx context.Context, auth AuthContext, roomID string) (RoomSnapshot, error) {
	if auth.Deployment == "" || !validToken(auth.UserID) || !validToken(roomID) {
		return RoomSnapshot{}, refusedError(ClassInvalidRequest, nil)
	}
	if auth.Deployment != s.cfg.DeploymentID {
		return RoomSnapshot{}, refusedError(ClassInvalidRequest, nil)
	}
	opCtx, release, err := s.beginOperation(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return RoomSnapshot{}, &KernelError{Outcome: OutcomeUnknown, Class: ClassCanceled, Err: err}
		}
		return RoomSnapshot{}, &KernelError{Outcome: OutcomeUnknown, Class: ClassOperationBudget, Err: err}
	}
	defer release()
	if s.closed {
		return RoomSnapshot{}, refusedError(ClassClosed, nil)
	}
	if s.poisoned != nil {
		return RoomSnapshot{}, refusedError(ClassLeaseLost, s.poisoned)
	}
	tx, err := s.work.BeginTx(opCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RoomSnapshot{}, incompleteError(opCtx, err)
	}
	defer func() {
		rctx, cancel := cleanupCtx()
		defer cancel()
		_ = tx.Rollback(rctx)
	}()

	// The same global lock order as the commit path, shared locks only.
	var ownerEpoch int64
	var ownerPID int32
	var ownerStart time.Time
	if err := tx.QueryRow(opCtx, `SELECT epoch, lease_pid, lease_backend_start FROM `+s.table("ownership")+` WHERE id FOR SHARE`).Scan(&ownerEpoch, &ownerPID, &ownerStart); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.poisoned = errLeaseLost
			return RoomSnapshot{}, refusedError(ClassLeaseLost, errLeaseLost)
		}
		return RoomSnapshot{}, incompleteError(opCtx, err)
	}
	var locked dependencyState
	if err := s.readDependencies(opCtx, tx, roomID, auth.UserID, &locked, lockShare); err != nil {
		return RoomSnapshot{}, incompleteError(opCtx, err)
	}
	var hostMode string
	err = tx.QueryRow(opCtx, `SELECT host_mode, current_seq, revision FROM `+s.table("rooms")+` WHERE room_id = $1 FOR SHARE`, roomID).Scan(&hostMode, &locked.currentSeq, &locked.revision)
	if errors.Is(err, pgx.ErrNoRows) {
		locked.roomMissing = true
	} else if err != nil {
		return RoomSnapshot{}, incompleteError(opCtx, err)
	}
	if err := s.fenceHoldsLocked(opCtx, tx, ownerEpoch, ownerPID, ownerStart); err != nil {
		if errors.Is(err, errLeaseLost) {
			s.poisoned = err
			return RoomSnapshot{}, refusedError(ClassLeaseLost, err)
		}
		return RoomSnapshot{}, incompleteError(opCtx, err)
	}

	switch {
	case locked.roomMissing:
		return RoomSnapshot{}, refusedError(ClassRoomNotFound, nil)
	case !locked.readAllowed():
		// Disabled or removed principals learn nothing about the Room.
		return RoomSnapshot{}, refusedError(ClassAuthorization, nil)
	}

	rows, err := tx.Query(opCtx, `SELECT seq, event_id, fact_version, kind, actor, event_encoding FROM `+s.table("room_events")+` WHERE room_id = $1 ORDER BY seq`, roomID)
	if err != nil {
		return RoomSnapshot{}, incompleteError(opCtx, err)
	}
	defer rows.Close()
	events := make([]model.Event, 0, locked.currentSeq)
	var expected uint64 = 1
	for rows.Next() {
		var seq int64
		var eventID, kind, actor string
		var factVersion int
		var encoding []byte
		if err := rows.Scan(&seq, &eventID, &factVersion, &kind, &actor, &encoding); err != nil {
			return RoomSnapshot{}, incompleteError(opCtx, err)
		}
		if uint64(seq) != expected {
			return RoomSnapshot{}, refusedError(ClassStateInvalid, fmt.Errorf("event sequence %d does not immediately follow %d", seq, expected-1))
		}
		event, err := decodeStoredFact(roomID, uint64(seq), eventID, factVersion, kind, actor, encoding)
		if err != nil {
			return RoomSnapshot{}, refusedError(ClassStateInvalid, err)
		}
		events = append(events, event)
		expected++
	}
	if err := rows.Err(); err != nil {
		return RoomSnapshot{}, incompleteError(opCtx, err)
	}
	if expected-1 != locked.currentSeq {
		return RoomSnapshot{}, refusedError(ClassStateInvalid, fmt.Errorf("event log ends at %d but the room row records sequence %d", expected-1, locked.currentSeq))
	}
	return RoomSnapshot{RoomID: roomID, HostMode: hostMode, Events: events, Seq: locked.currentSeq, Revision: locked.revision}, nil
}

// refusedError is the error form of a definite refusal for read-only APIs
// that return no Result.
func refusedError(class string, cause error) error {
	return &KernelError{Outcome: OutcomeRefused, Class: class, Err: cause}
}

// incompleteError classifies an interrupted read: nothing may be disclosed.
func incompleteError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return &KernelError{Outcome: OutcomeUnknown, Class: ClassCanceled, Err: err}
	}
	return &KernelError{Outcome: OutcomeUnknown, Class: ClassConnectionFailed, Err: err}
}
