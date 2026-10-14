package sqlstore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/enterprisebackup"
	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"path/filepath"
	"testing"
)

func closeRestoreStatusWorkByActualCancellation(t *testing.T, f *fixture) uint32 {
	t.Helper()
	blocker := schemaConn(t, f.dsn, f.schema)
	tx, err := blocker.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ctx, cancel := cleanupCtx(); defer cancel(); _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(t.Context(), `SELECT epoch FROM ownership WHERE id FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	pid := f.store.work.PgConn().PID()
	go func() { done <- f.store.EnsureRestoreReady(ctx) }()
	waitForLockWait(t, f.admin, pid)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled barrier read unexpectedly succeeded")
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !f.store.work.IsClosed() {
		t.Fatal("real cancelled status read did not close work")
	}
	return pid
}
func TestClosedWorkPendingRestoreAllowsTypedRecoveryAndCompletion(t *testing.T) {
	b, objects := backupPGSource(t)
	path := filepath.Join(t.TempDir(), "bundle")
	if _, err := b.f.store.BackupOffline(t.Context(), objects, path, "maintenance-source"); err != nil {
		t.Fatal(err)
	}
	bundle, err := enterprisebackup.Open(t.Context(), path, b.f.cfg.DeploymentID, "org-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.f.store.Close(); err != nil {
		t.Fatal(err)
	}
	target := backupPGPristine(t)
	targetObjects, err := enterpriseobject.Open(filepath.Join(t.TempDir(), "target-objects"), target.cfg.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := target.store.RestoreOffline(t.Context(), targetObjects, bundle, "maintenance-target")
	if err != nil || imported.Restore == nil || !imported.Restore.Imported || imported.Restore.Complete {
		t.Fatal("imported pending restore missing", err)
	}
	epoch, leasePID, leaseStart := target.store.lease.epoch, target.store.lease.pid, target.store.lease.start
	firstPID := closeRestoreStatusWorkByActualCancellation(t, target)
	if _, err := target.store.RecoverDeliveries(t.Context()); err != nil {
		t.Fatal("typed recovery blocked by candidate barrier", err)
	}
	if target.store.work.PgConn().PID() == firstPID || target.store.work.IsClosed() {
		t.Fatal("typed recovery did not install eligible transport")
	}
	if err := target.store.EnsureRestoreReady(t.Context()); err == nil {
		t.Fatal("recovery cleared restore barrier prematurely")
	}
	secondPID := closeRestoreStatusWorkByActualCancellation(t, target)
	completed, err := target.store.CompleteRestore(t.Context(), targetObjects, "maintenance-target", bundle.Digest())
	if err != nil || completed.Restore == nil || !completed.Restore.Complete {
		t.Fatal("typed completion blocked by candidate barrier", err)
	}
	if target.store.work.PgConn().PID() == secondPID || target.store.work.IsClosed() {
		t.Fatal("typed completion did not install eligible transport")
	}
	if target.store.lease.epoch != epoch || target.store.lease.pid != leasePID || !target.store.lease.start.Equal(leaseStart) {
		t.Fatal("maintenance transport acquired new ownership")
	}
	if err := target.store.EnsureRestoreReady(t.Context()); err != nil {
		t.Fatal("completed restore did not become ready", err)
	}
}
