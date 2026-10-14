package sqlstore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"testing"
)

func cancelBlockedWorkRead(t *testing.T, f *fixture) uint32 {
	t.Helper()
	blocker := schemaConn(t, f.dsn, f.schema)
	tx, err := blocker.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT room_id FROM rooms WHERE room_id='room-1' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	pid := f.store.work.PgConn().PID()
	go func() {
		_, err := f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
		done <- err
	}()
	waitForLockWait(t, f.admin, pid)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled blocked read succeeded")
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !f.store.work.IsClosed() {
		t.Fatal("probe did not close the work connection")
	}
	return pid
}

func TestClosedWorkConnectionNewExplicitCommandKeepsOriginalOwnership(t *testing.T) {
	f := newFixture(t)
	epoch, leasePID, leaseStart := f.store.lease.epoch, f.store.lease.pid, f.store.lease.start
	oldPID := cancelBlockedWorkRead(t, f)
	events, ops, _, _, _ := f.roomCounts(t)
	if events != 0 || ops != 0 {
		t.Fatal("read cancellation wrote effects")
	}
	res, err := f.store.CommitHumanSend(t.Context(), humanSendCommand("u-owner", "after-read-cancel", "slot2", "new explicit request"))
	if err != nil || res.Outcome != OutcomeCommitted || res.Seq != 1 {
		t.Fatalf("new explicit command must recover transport: outcome=%s error=%v", res.Outcome, err)
	}
	if f.store.work.PgConn().PID() == oldPID || f.store.lease.epoch != epoch || f.store.lease.pid != leasePID || !f.store.lease.start.Equal(leaseStart) {
		t.Fatal("transport replacement changed ownership or reused dead work")
	}
	events, ops, _, _, _ = f.roomCounts(t)
	if events != 1 || ops != 1 {
		t.Fatal("new explicit request was repeated")
	}
}

func TestClosedWorkConnectionCannotReviveLostLease(t *testing.T) {
	f := newFixture(t)
	cancelBlockedWorkRead(t, f)
	epoch := f.store.lease.epoch
	f.terminate(t, f.store.lease.pid, "lease")
	res, err := f.store.CommitHumanSend(t.Context(), humanSendCommand("u-owner", "after-cancel-lost-lease", "slot2", "must refuse"))
	if k := kernelErr(t, res, err); k.Outcome != OutcomeRefused || k.Class != ClassLeaseLost {
		t.Fatalf("replacement revived lost lease: %+v", k)
	}
	if f.store.lease.epoch != epoch || f.store.poisoned == nil {
		t.Fatal("lost lease incarnation changed or was not retired")
	}
	events, ops, _, _, _ := f.roomCounts(t)
	if events != 0 || ops != 0 {
		t.Fatal("deposed transport wrote effects")
	}
}

func TestClosedWorkConnectionOriginalUnknownNeedsExplicitCommand(t *testing.T) {
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			proxy := startProxy(t, mode)
			f := newFixtureProxy(t, func(dsn string) string { return proxy.dsn(dsn) })
			cmd := humanSendCommand("u-owner", "original-unknown-work", "slot2", "original effect")
			proxy.arm()
			res, err := f.store.CommitHumanSend(t.Context(), cmd)
			if k := kernelErr(t, res, err); k.Outcome != OutcomeUnknown {
				t.Fatal(k)
			}
			if !f.store.work.IsClosed() {
				t.Fatal("lost-response probe did not close work")
			}
			beforeEvents, beforeOps, _, _, _ := f.roomCounts(t)
			expected := int64(0)
			if mode == "drop-response" {
				expected = 1
			}
			if beforeEvents != expected || beforeOps != expected {
				t.Fatal("unknown unexpectedly retried")
			}
			resolved, err := f.store.ResolveHumanSend(t.Context(), cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "drop-response" && resolved.Outcome != OutcomeReplayed || mode == "break-on-commit" && resolved.Outcome != OutcomeNotCommitted {
				t.Fatal(resolved)
			}
			if events, ops, _, _, _ := f.roomCounts(t); events != beforeEvents || ops != beforeOps {
				t.Fatal("Resolve automatically replayed effect")
			}
			// The fault proxy remains armed for later connections; the explicit
			// recovery request uses the same database without the injected cut.
			f.store.cfg.DSN = f.dsn
			res, err = f.store.CommitHumanSend(t.Context(), cmd)
			want := OutcomeCommitted
			if mode == "drop-response" {
				want = OutcomeReplayed
			}
			if err != nil || res.Outcome != want {
				t.Fatalf("explicit original request: %s %v", res.Outcome, err)
			}
			if events, ops, _, _, _ := f.roomCounts(t); events != 1 || ops != 1 {
				t.Fatal("explicit recovery duplicated original effect")
			}
		})
	}
}

func TestClosedWorkConnectionReplacementFailureNeverRetriesOrWrites(t *testing.T) {
	f := newFixture(t)
	oldPID := cancelBlockedWorkRead(t, f)
	originalDSN := f.store.cfg.DSN
	f.store.cfg.DSN = "postgres://postgres@127.0.0.1:1/pairroom_test?sslmode=disable&connect_timeout=1"
	cmd := humanSendCommand("u-owner", "unavailable-replacement", "slot2", "same explicit intent")
	res, err := f.store.CommitHumanSend(t.Context(), cmd)
	if k := kernelErr(t, res, err); k.Outcome != OutcomeNotCommitted || k.Class != ClassConnectionFailed {
		t.Fatal(k)
	}
	if f.store.work.PgConn().PID() != oldPID || !f.store.work.IsClosed() || f.store.poisoned != nil {
		t.Fatal("failed replacement installed transport or altered authority")
	}
	if events, ops, _, _, _ := f.roomCounts(t); events != 0 || ops != 0 {
		t.Fatal("failed replacement wrote effects")
	}
	f.store.cfg.DSN = originalDSN
	res, err = f.store.CommitHumanSend(t.Context(), cmd)
	if err != nil || res.Outcome != OutcomeCommitted {
		t.Fatalf("explicit later request: %s %v", res.Outcome, err)
	}
	if events, ops, _, _, _ := f.roomCounts(t); events != 1 || ops != 1 {
		t.Fatal("explicit later request duplicated")
	}
}

func TestClosedWorkConnectionCanceledCallerDoesNotReconnect(t *testing.T) {
	f := newFixture(t)
	oldPID := cancelBlockedWorkRead(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res, err := f.store.CommitHumanSend(ctx, humanSendCommand("u-owner", "cancel-before-replace", "slot2", "refuse before transport"))
	if k := kernelErr(t, res, err); k.Outcome != OutcomeNotCommitted || k.Class != ClassCanceled {
		t.Fatal(k)
	}
	if f.store.work.PgConn().PID() != oldPID || !f.store.work.IsClosed() {
		t.Fatal("canceled caller reconnected work")
	}
	if events, ops, _, _, _ := f.roomCounts(t); events != 0 || ops != 0 {
		t.Fatal("canceled caller wrote effect")
	}
}
