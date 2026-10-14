package sqlstore

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A diagnostic application_name and the correct advisory tag do not identify
// the registered backend incarnation. This uses a real replacement backend
// before the old Store has observed its lease loss.
func TestLeaseSameApplicationNameAndLockCannotImpersonateIncarnationPG(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var name string
	if err := f.store.lease.conn.QueryRow(ctx, `SHOW application_name`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	originalPID, originalStart, originalEpoch := f.store.lease.pid, f.store.lease.start, f.store.lease.epoch
	f.terminate(t, originalPID, "original lease")
	fake, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fake.Close(context.Background()) })
	if _, err = fake.Exec(ctx, `SELECT set_config('application_name',$1,false)`, name); err != nil {
		t.Fatal(err)
	}
	var held bool
	if err = fake.QueryRow(ctx, `SELECT pg_catalog.pg_try_advisory_lock($1)`, int64(leaseKey(f.cfg.DeploymentID))).Scan(&held); err != nil || !held {
		t.Fatalf("replacement backend did not hold exact advisory tag: held=%t err=%v", held, err)
	}
	var fakeName string
	if err = fake.QueryRow(ctx, `SHOW application_name`).Scan(&fakeName); err != nil || fakeName != name || fake.PgConn().PID() == originalPID {
		t.Fatalf("replacement diagnostic identity: same_name=%t different_pid=%t err=%v", fakeName == name, fake.PgConn().PID() != originalPID, err)
	}
	var ownerPID int32
	var ownerEpoch int64
	if err = f.admin.QueryRow(ctx, `SELECT lease_pid,epoch FROM ownership WHERE id`).Scan(&ownerPID, &ownerEpoch); err != nil || uint32(ownerPID) != originalPID || ownerEpoch != originalEpoch {
		t.Fatal("replacement changed the registered ownership record")
	}
	cmd := f.command("same-label-fake-holder")
	res, err := f.store.Commit(ctx, cmd)
	if k := kernelErr(t, res, err); k.Class != ClassLeaseLost || k.Outcome != OutcomeRefused {
		t.Fatalf("fake holder revived original writer: %+v", res)
	}
	if _, err = f.store.ReadonlySnapshot(ctx, cmd.Auth, "room-1"); err == nil {
		t.Fatal("deposed Store released private snapshot")
	}
	if events, ops, audits, seq, revision := f.roomCounts(t); events != 0 || ops != 0 || audits != 0 || seq != 0 || revision != 0 {
		t.Fatalf("fake-holder rejection changed Room: %d %d %d %d %d", events, ops, audits, seq, revision)
	}
	if err = fake.Close(ctx); err != nil {
		t.Fatal(err)
	}
	successor, err := Open(ctx, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = successor.Close() })
	if successor.lease.epoch != originalEpoch+1 || successor.lease.start.Equal(originalStart) {
		t.Fatal("real successor did not establish a fresh incarnation and epoch")
	}
	res, err = successor.Commit(ctx, f.command("real-successor"))
	if err != nil || res.Outcome != OutcomeCommitted {
		t.Fatalf("real successor cannot write: %+v %v", res, err)
	}
	res, err = f.store.Commit(ctx, f.command("old-after-successor"))
	if k := kernelErr(t, res, err); k.Class != ClassLeaseLost {
		t.Fatalf("old Store revived after real successor: %+v", res)
	}
	if events, ops, audits, seq, revision := f.roomCounts(t); events != 1 || ops != 1 || audits != 1 || seq != 1 || revision != 1 {
		t.Fatalf("successor/old effects are not isolated: %d %d %d %d %d", events, ops, audits, seq, revision)
	}
}
