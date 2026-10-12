package sqlstore

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Resolve verifies the final state of an operation under the same fence and
// live authorization the commit path uses. It executes no effect and no
// retry: any valid membership role — including a viewer — may recover its own
// accepted operation, while a disabled or removed principal is refused before
// any stored result is disclosed. When the commit signal was lost and the
// work connection is unusable, Resolve may open one bounded replacement
// connection for this read-only verification; it never commits on it.
func (s *Store) Resolve(ctx context.Context, auth AuthContext, key OpKey, mutation Mutation) (Result, error) {
	cmd := Command{Auth: auth, Key: key, Mutation: mutation}
	if class := validateCommand(cmd); class != "" {
		return refused(class, nil)
	}
	if auth.Deployment != s.cfg.DeploymentID {
		return refused(ClassInvalidRequest, nil)
	}
	opCtx, release, err := s.beginOperation(ctx)
	if err != nil {
		return s.resolveEntryFailure(ctx, err)
	}
	defer release()
	if s.closed {
		return refused(ClassClosed, nil)
	}
	if s.poisoned != nil {
		return refused(ClassLeaseLost, s.poisoned)
	}
	hash := canonicalHash(key, mutation)
	result, err := s.resolveOnce(opCtx, cmd, hash, s.work)
	if result.Outcome != OutcomeUnknown {
		return result, err
	}
	// The check could not complete on the work connection. One bounded
	// replacement connection is used for verification only.
	replacement, cerr := s.openVerificationConn(opCtx)
	if cerr != nil {
		return result, err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = replacement.Close(closeCtx)
	}()
	return s.resolveOnce(opCtx, cmd, hash, replacement)
}

// resolveEntryFailure classifies a Resolve that never started a check: nothing
// was verified, so the outcome is unknown — never NotCommitted.
func (s *Store) resolveEntryFailure(parent context.Context, err error) (Result, error) {
	if parent.Err() != nil {
		return unknown(ClassCanceled, err)
	}
	return unknown(ClassOperationBudget, err)
}

// openVerificationConn opens a bounded read-only verification connection
// pinned to the configured schema.
func (s *Store) openVerificationConn(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, s.cfg.DSN)
	if err != nil {
		return nil, err
	}
	if err := pinSearchPath(ctx, conn, s.cfg.Schema); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	if err := verifySchemaVersion(ctx, conn, s.cfg.Schema); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return conn, nil
}

// resolveOnce performs one read-only verification attempt on the given
// connection. An incomplete check is unknown; a complete check that proves
// absence is NotCommitted.
func (s *Store) resolveOnce(ctx context.Context, cmd Command, hash [32]byte, conn *pgx.Conn) (Result, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return resolveIncomplete(ctx, err)
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()

	// The same global lock order; reads only, so FOR SHARE throughout. The
	// absence proof is sound because the only writer is the lease holder and
	// this Store serializes Commit and Resolve.
	var ownerEpoch int64
	var ownerPID int32
	var ownerStart time.Time
	if err := tx.QueryRow(ctx, `SELECT epoch, lease_pid, lease_backend_start FROM `+s.table("ownership")+` WHERE id FOR SHARE`).Scan(&ownerEpoch, &ownerPID, &ownerStart); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.poisoned = errLeaseLost
			return refused(ClassLeaseLost, errLeaseLost)
		}
		return resolveIncomplete(ctx, err)
	}
	var locked dependencyState
	if err := s.readDependencies(ctx, tx, cmd, &locked, lockShare); err != nil {
		return resolveIncomplete(ctx, err)
	}
	if err := s.readRoom(ctx, tx, cmd, &locked, lockShare); err != nil {
		return resolveIncomplete(ctx, err)
	}
	if err := s.fenceHoldsLocked(ctx, tx, ownerEpoch, ownerPID, ownerStart); err != nil {
		if errors.Is(err, errLeaseLost) {
			s.poisoned = err
			return refused(ClassLeaseLost, err)
		}
		return resolveIncomplete(ctx, err)
	}
	if s.beforeResolveLookup != nil {
		s.beforeResolveLookup()
	}

	switch {
	case locked.roomMissing:
		return refused(ClassRoomNotFound, nil)
	case !locked.readAllowed():
		return refused(ClassAuthorization, nil)
	}

	var storedHash []byte
	var resultSeq int64
	err = tx.QueryRow(ctx, `
		SELECT intent_hash, result_seq FROM `+s.table("operations")+`
		 WHERE op_namespace = $1 AND deployment_id = $2 AND room_id = $3
		   AND binding_id = '' AND generation = 0 AND op_kind = $4 AND identifier = $5 AND principal = $6`,
		cmd.Key.Namespace, cmd.Key.Deployment, cmd.Key.RoomID, cmd.Key.OpKind, cmd.Key.Identifier, cmd.Key.Principal).Scan(&storedHash, &resultSeq)
	switch {
	case err == nil:
		if !bytes.Equal(storedHash, hash[:]) {
			// The stored payload hash is authoritative; a differing mutation
			// under the same key is a definite conflict.
			kerr := &KernelError{Outcome: OutcomeConflict, Class: ClassPayloadConflict}
			return Result{Outcome: OutcomeConflict, Class: ClassPayloadConflict, Err: kerr, Revision: locked.revision}, kerr
		}
		original, err := s.loadFact(ctx, tx, cmd.Key.RoomID, uint64(resultSeq))
		if err != nil {
			return resolveIncomplete(ctx, err)
		}
		return Result{Outcome: OutcomeReplayed, Seq: uint64(resultSeq), Revision: locked.revision, Fact: original}, nil
	case errors.Is(err, pgx.ErrNoRows):
		return Result{Outcome: OutcomeNotCommitted, Revision: locked.revision}, nil
	default:
		return resolveIncomplete(ctx, err)
	}
}

// resolveIncomplete classifies an interrupted verification: because the check
// did not complete, the outcome is unknown — never a guessed absence.
func resolveIncomplete(ctx context.Context, err error) (Result, error) {
	if ctx.Err() != nil {
		return unknown(ClassCanceled, err)
	}
	return unknown(ClassConnectionFailed, err)
}
