package sqlstore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

// replaceClosedWork is called once on entry to a new explicit command while
// sem is held. It repairs transport only: it never retries a prior command,
// acquires ownership, advances the epoch or clears a retired incarnation.
// Do not remove its admission fence as a connection-pool optimization.
func (s *Store) replaceClosedWork(ctx context.Context, plan opPlan) error {
	if !s.work.IsClosed() {
		return nil
	}
	conn, err := pgx.Connect(ctx, s.cfg.DSN)
	if err != nil {
		return err
	}
	accepted := false
	defer func() {
		if !accepted {
			closeCtx, cancel := cleanupCtx()
			defer cancel()
			_ = conn.Close(closeCtx)
		}
	}()
	if err := configureSession(ctx, conn, s.cfg.Schema); err != nil {
		return err
	}
	if err := verifySchemaVersion(ctx, conn, s.cfg.Schema); err != nil {
		return err
	}
	if err := verifyDeployment(ctx, conn, s.cfg.Schema, s.cfg.DeploymentID); err != nil {
		return err
	}
	// This transaction contains verification SELECTs only. PostgreSQL needs a
	// normal transaction for FOR SHARE; the lock prevents takeover during check.
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { rollbackCtx, cancel := cleanupCtx(); defer cancel(); _ = tx.Rollback(rollbackCtx) }()
	var epoch int64
	var pid int32
	var start time.Time
	if err := tx.QueryRow(ctx, `SELECT epoch,lease_pid,lease_backend_start FROM `+s.table("ownership")+` WHERE id FOR SHARE`).Scan(&epoch, &pid, &start); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.poisoned = errLeaseLost
			return refusedError(ClassLeaseLost, errLeaseLost)
		}
		return err
	}
	if err := s.fenceHoldsLocked(ctx, tx, epoch, pid, start); err != nil {
		if errors.Is(err, errLeaseLost) {
			s.poisoned = err
			return refusedError(ClassLeaseLost, err)
		}
		return err
	}
	// Typed offline restore/recovery commands retain their original barrier
	// semantics. An ordinary command may not install transport while pending.
	if plan.restore == nil && plan.recovery == nil {
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("restore_runs")+` WHERE completed_at IS NULL)`).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return refusedError(ClassRestorePending, nil)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.work = conn
	accepted = true
	return nil
}
