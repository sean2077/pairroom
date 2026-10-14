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
	RoomID         string
	DeploymentID   string
	OrgID          string
	CreationUserID string
	HostMode       string
	Events         []model.Event
	Seq            uint64
	Revision       uint64
}

// ReadonlySnapshot serves the authenticated read side. It runs under the same
// ownership fence and live user/membership authorization as the commit path,
// takes only shared locks in the documented global order, writes nothing and
// never reaps.
//
// When the work connection is unusable — for example after a lost commit
// response — one bounded read-only replacement connection performs the same
// verification under the same fence and authorization; it never changes the
// epoch, opens ownership recovery, or retries any effect, and if it cannot be
// established the read stays unknown with zero disclosure. Refusals
// (authorization, room_not_found, state_invalid, lease_lost) disclose zero
// content; an incomplete read is an unknown-class error, never a partial
// snapshot.
func (s *Store) ReadonlySnapshot(ctx context.Context, auth AuthContext, roomID string) (RoomSnapshot, error) {
	if auth.DeviceGrantID != "" || auth.BindingGrantID != "" && auth.SessionID != "" {
		return RoomSnapshot{}, refusedError(ClassAuthorization, nil)
	}
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
	snapshot, err := s.snapshotOnce(opCtx, auth, roomID, s.work)
	var kerr *KernelError
	if !errors.As(err, &kerr) || kerr.Outcome != OutcomeUnknown {
		return snapshot, err
	}
	// The read could not complete on the work connection. One bounded
	// replacement connection is used for this read-only verification.
	replacement, cerr := s.openVerificationConn(opCtx)
	if cerr != nil {
		return snapshot, err
	}
	defer func() {
		rctx, cancel := cleanupCtx()
		defer cancel()
		_ = replacement.Close(rctx)
	}()
	return s.snapshotOnce(opCtx, auth, roomID, replacement)
}

// snapshotOnce performs one read-only snapshot attempt on the given
// connection.
func (s *Store) snapshotOnce(ctx context.Context, auth AuthContext, roomID string, conn *pgx.Conn) (RoomSnapshot, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RoomSnapshot{}, incompleteError(ctx, err)
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
	if err := tx.QueryRow(ctx, `SELECT epoch, lease_pid, lease_backend_start FROM `+s.table("ownership")+` WHERE id FOR SHARE`).Scan(&ownerEpoch, &ownerPID, &ownerStart); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.poisoned = errLeaseLost
			return RoomSnapshot{}, refusedError(ClassLeaseLost, errLeaseLost)
		}
		return RoomSnapshot{}, incompleteError(ctx, err)
	}
	var locked dependencyState
	if err := s.readDependencies(ctx, tx, roomID, auth, &locked, lockShare); err != nil {
		return RoomSnapshot{}, incompleteError(ctx, err)
	}
	var hostMode, orgID string
	err = tx.QueryRow(ctx, `SELECT host_mode, current_seq, revision, org_id FROM `+s.table("rooms")+` WHERE room_id = $1 FOR SHARE`, roomID).Scan(&hostMode, &locked.currentSeq, &locked.revision, &orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		locked.roomMissing = true
	} else if err != nil {
		return RoomSnapshot{}, incompleteError(ctx, err)
	}
	if err := s.fenceHoldsLocked(ctx, tx, ownerEpoch, ownerPID, ownerStart); err != nil {
		if errors.Is(err, errLeaseLost) {
			s.poisoned = err
			return RoomSnapshot{}, refusedError(ClassLeaseLost, err)
		}
		return RoomSnapshot{}, incompleteError(ctx, err)
	}

	switch {
	case locked.roomMissing:
		return RoomSnapshot{}, refusedError(ClassRoomNotFound, nil)
	case !locked.readAllowed() || auth.BindingGrantID != "" && (locked.credential.kind != CredentialBinding || locked.credential.roomID != roomID):
		// Disabled or removed principals learn nothing about the Room.
		return RoomSnapshot{}, refusedError(ClassAuthorization, nil)
	}

	return s.readRoomEvents(ctx, tx, RoomSnapshot{RoomID: roomID, DeploymentID: s.cfg.DeploymentID, OrgID: orgID, HostMode: hostMode, Seq: locked.currentSeq, Revision: locked.revision})
}

// readRoomEvents is shared by an already authorized/fenced read and a new
// Relay command after its accepted-receipt lookup and revision check. It
// never begins a transaction, authorizes a caller or applies a transition.
func (s *Store) readRoomEvents(ctx context.Context, tx pgx.Tx, snapshot RoomSnapshot) (RoomSnapshot, error) {
	roomID := snapshot.RoomID
	// Refuse before PostgreSQL streams any encoding or Go allocates the replay
	// slice. Room sequence alone is untrusted; check actual count and bytes too.
	if err := s.checkRoomReplayBudget(ctx, tx, roomID, snapshot.Seq); err != nil {
		return RoomSnapshot{}, err
	}
	rows, err := tx.Query(ctx, `SELECT seq, event_id, fact_version, kind, actor, event_encoding, origin_kind, origin_user FROM `+s.table("room_events")+` WHERE room_id = $1 ORDER BY seq`, roomID)
	if err != nil {
		return RoomSnapshot{}, incompleteError(ctx, err)
	}
	defer rows.Close()
	// The event slice grows with the rows actually read: the room row's
	// sequence counter is validated, never trusted as an allocation hint, so a
	// corrupt-but-bigint-valid counter cannot panic or exhaust memory.
	var events []model.Event
	var creationUserID string
	var expected uint64 = 1
	for rows.Next() {
		var seq int64
		var eventID, kind, actor, originKind string
		var originUser *string
		var factVersion int
		var encoding []byte
		if err := rows.Scan(&seq, &eventID, &factVersion, &kind, &actor, &encoding, &originKind, &originUser); err != nil {
			return RoomSnapshot{}, incompleteError(ctx, err)
		}
		if uint64(seq) != expected {
			return RoomSnapshot{}, refusedError(ClassStateInvalid, fmt.Errorf("event sequence %d does not immediately follow %d", seq, expected-1))
		}
		event, err := decodeStoredFact(roomID, uint64(seq), eventID, factVersion, kind, actor, encoding, originKind, originUser)
		if err != nil {
			return RoomSnapshot{}, refusedError(ClassStateInvalid, err)
		}
		events = append(events, event)
		if seq == 1 {
			if originUser != nil {
				creationUserID = *originUser
			}
		}
		expected++
	}
	if err := rows.Err(); err != nil {
		return RoomSnapshot{}, incompleteError(ctx, err)
	}
	if expected-1 != snapshot.Seq {
		return RoomSnapshot{}, refusedError(ClassStateInvalid, fmt.Errorf("event log ends at %d but the room row records sequence %d", expected-1, snapshot.Seq))
	}
	snapshot.Events, snapshot.CreationUserID = events, creationUserID
	return snapshot, nil
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
