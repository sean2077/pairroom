package sqlstore

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Resolve verifies the final state of an append_fact operation under the same
// fence and live authorization the commit path uses.
func (s *Store) Resolve(ctx context.Context, auth AuthContext, key OpKey, mutation Mutation) (Result, error) {
	cmd := Command{Auth: auth, Key: key, Mutation: mutation}
	if class := validateCommand(cmd); class != "" {
		return refused(class, nil)
	}
	if auth.Deployment != s.cfg.DeploymentID {
		return refused(ClassInvalidRequest, nil)
	}
	plan := opPlan{auth: auth, key: key, userID: auth.UserID, hash: canonicalHash(key, mutation)}
	return s.resolveWithFallback(ctx, plan)
}

// resolveWithFallback executes one read-only verification with the shared
// semantics: no effect, no retry, and — when the commit signal was lost and
// the work connection is unusable — one bounded replacement connection used
// for verification only. Any valid membership role, including a viewer, may
// recover its own accepted operation; a disabled or removed principal is
// refused before any stored result is disclosed.
func (s *Store) resolveWithFallback(ctx context.Context, plan opPlan) (Result, error) {
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
	result, err := s.resolvePlanned(opCtx, plan, s.work)
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
		rctx, cancel := cleanupCtx()
		defer cancel()
		_ = replacement.Close(rctx)
	}()
	return s.resolvePlanned(opCtx, plan, replacement)
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
	fail := func(err error) (*pgx.Conn, error) {
		cctx, ccancel := cleanupCtx()
		defer ccancel()
		_ = conn.Close(cctx)
		return nil, err
	}
	if err := configureSession(ctx, conn, s.cfg.Schema); err != nil {
		return fail(err)
	}
	if err := verifySchemaVersion(ctx, conn, s.cfg.Schema); err != nil {
		return fail(err)
	}
	return conn, nil
}

// resolvePlanned performs one read-only verification attempt on the given
// connection. An incomplete check is unknown; a complete check that proves
// absence is NotCommitted.
func (s *Store) resolvePlanned(ctx context.Context, plan opPlan, conn *pgx.Conn) (Result, error) {
	cutoffContext, cutoffErr := captureApprovalCutoff(ctx, conn, plan)
	if cutoffErr != nil {
		return resolveIncomplete(ctx, cutoffErr)
	}
	ctx = cutoffContext
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return resolveIncomplete(ctx, err)
	}
	defer func() {
		rctx, cancel := cleanupCtx()
		defer cancel()
		_ = tx.Rollback(rctx)
	}()

	// The same global lock order and preselected ownership mode. Room-create
	// resolution takes FOR UPDATE on ownership before any authorization row,
	// so it waits for an uncertain creation even if no Room row exists yet.
	// Membership controls also preselect exclusive ownership and lock their
	// sorted authorization set. Ordinary appends retain their shared guards.
	var ownerEpoch int64
	var ownerPID int32
	var ownerStart time.Time
	if err := tx.QueryRow(ctx, `SELECT epoch, lease_pid, lease_backend_start FROM `+s.table("ownership")+` WHERE id`+plan.ownershipLock()).Scan(&ownerEpoch, &ownerPID, &ownerStart); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.poisoned = errLeaseLost
			return refused(ClassLeaseLost, errLeaseLost)
		}
		return resolveIncomplete(ctx, err)
	}
	var locked dependencyState
	if err := s.readPlanDependencies(ctx, tx, plan, &locked, lockShare); err != nil {
		return resolveIncomplete(ctx, err)
	}
	if err := s.readPlannedRoom(ctx, tx, plan, &locked, lockShare); err != nil {
		return resolveIncomplete(ctx, err)
	}
	if err := s.fenceHoldsLocked(ctx, tx, ownerEpoch, ownerPID, ownerStart); err != nil {
		if errors.Is(err, errLeaseLost) {
			s.poisoned = err
			return refused(ClassLeaseLost, err)
		}
		return resolveIncomplete(ctx, err)
	}
	if plan.restore != nil {
		return s.resolveRestorePlan(ctx, tx, plan, locked.restore)
	}
	if plan.attachment != nil {
		return s.resolveAttachmentPlan(ctx, tx, plan, locked.attachment)
	}
	if plan.catalog != nil {
		return s.resolveCatalogPlan(ctx, tx, plan, locked.catalog)
	}
	if plan.recovery != nil {
		return s.resolveRecoveryPlan(ctx, tx, plan, locked.recovery)
	}
	if plan.control != nil {
		return s.resolveControlPlan(ctx, tx, plan, locked.control)
	}
	if plan.governance != nil {
		return s.resolveGovernancePlan(ctx, tx, plan, locked.governance)
	}
	if plan.binding != nil {
		return s.resolveBindingPlan(ctx, tx, plan, locked.binding)
	}
	if plan.relay != nil {
		return s.resolveRelayPlan(ctx, tx, plan, locked)
	}
	if plan.roomReadAuthorization {
		if locked.roomMissing {
			return refused(ClassRoomNotFound, nil)
		}
		if !locked.readAllowed() || plan.auth.BindingGrantID != "" && (locked.credential.kind != CredentialBinding || locked.credential.roomID != plan.key.RoomID) {
			return refused(ClassAuthorization, nil)
		}
		return Result{Outcome: OutcomeReplayed}, nil
	}

	if plan.roomCreate != nil {
		if locked.userMissing || locked.userDisabled || !locked.credential.allowed() {
			return refused(ClassAuthorization, nil)
		}
	} else {
		switch {
		case locked.roomMissing:
			return refused(ClassRoomNotFound, nil)
		case !locked.readAllowed():
			return refused(ClassAuthorization, nil)
		}
	}

	var storedHash []byte
	var resultSeq int64
	var resultRevision int64
	err = tx.QueryRow(ctx, `
		SELECT intent_hash, result_seq, result_revision FROM `+s.table("operations")+`
		 WHERE op_namespace = $1 AND deployment_id = $2 AND room_id = $3
		   AND binding_id = $4 AND generation = $5 AND op_kind = $6 AND identifier = $7 AND principal = $8`,
		plan.key.Namespace, plan.key.Deployment, plan.key.RoomID, plan.key.BindingID, int64(plan.key.Generation), plan.key.OpKind, plan.key.Identifier, plan.key.Principal).Scan(&storedHash, &resultSeq, &resultRevision)
	switch {
	case err == nil:
		if plan.roomCreate != nil && !locked.readAllowed() {
			return refused(ClassAuthorization, nil)
		}
		if !bytes.Equal(storedHash, plan.hash[:]) {
			// The stored payload hash is authoritative; a differing mutation
			// under the same key is a definite conflict.
			kerr := &KernelError{Outcome: OutcomeConflict, Class: ClassPayloadConflict}
			return Result{Outcome: OutcomeConflict, Class: ClassPayloadConflict, Err: kerr, Revision: uint64(resultRevision)}, kerr
		}
		original, err := s.loadFact(ctx, tx, plan.key.RoomID, uint64(resultSeq))
		if err != nil {
			if (plan.roomCreate != nil || plan.roomMembership != nil) && isStoredFactCorruption(err) {
				return refused(ClassStoredFactUnreadable, err)
			}
			return resolveIncomplete(ctx, err)
		}
		if plan.roomCreate != nil {
			if err := s.validateRoomCreateReplay(ctx, tx, plan, original); err != nil {
				if isStoredFactCorruption(err) {
					return refused(ClassStoredFactUnreadable, err)
				}
				return resolveIncomplete(ctx, err)
			}
		}
		if plan.roomMembership != nil {
			if err := validateRoomMembershipReplay(plan, original); err != nil {
				return refused(ClassStoredFactUnreadable, err)
			}
		}
		if plan.human != nil {
			if err := s.validateHumanMessageReplay(ctx, tx, plan, original); err != nil {
				if isStoredFactCorruption(err) {
					return refused(ClassStoredFactUnreadable, err)
				}
				return resolveIncomplete(ctx, err)
			}
		}
		// The original stored result, including the Room revision that the
		// original commit defined — never the current revision.
		return Result{Outcome: OutcomeReplayed, Seq: uint64(resultSeq), Revision: uint64(resultRevision), Fact: original}, nil
	case errors.Is(err, pgx.ErrNoRows):
		if plan.roomCreate != nil {
			// This exact creation operation is absent. Resolving it never
			// adopts or discloses an unrelated existing Room, including its
			// revision. Only Commit classifies that ID as room_taken.
			return Result{Outcome: OutcomeNotCommitted}, nil
		}
		if plan.roomMembership != nil {
			return Result{Outcome: OutcomeNotCommitted}, nil
		}
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
