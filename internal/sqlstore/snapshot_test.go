package sqlstore

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func expectKernel(t *testing.T, err error, outcome Outcome, class string) {
	t.Helper()
	var kerr *KernelError
	if !errors.As(err, &kerr) {
		t.Fatalf("error is not a KernelError: %v", err)
	}
	if kerr.Outcome != outcome || kerr.Class != class {
		t.Fatalf("got %s/%s, want %s/%s (%v)", kerr.Outcome, kerr.Class, outcome, class, kerr)
	}
}

// The authenticated snapshot serves the authoritative room row and the
// complete contiguous event log, and writes nothing.
func TestReadonlySnapshotServesAuthoritativeView(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.store.CommitHumanSend(ctx, humanSendCommand("u-owner", "snap-1", "slot2", "snapshot body")); err != nil {
		t.Fatal(err)
	}
	beforeEvents, beforeOps, beforeAudits, beforeSeq, beforeRevision := f.roomCounts(t)

	snap, err := f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.RoomID != "room-1" || snap.HostMode != "native" || snap.Seq != 1 || snap.Revision != 1 || len(snap.Events) != 1 {
		t.Fatalf("snapshot: %+v", snap)
	}
	if snap.Events[0].ID != "evt-snap-1" || snap.Events[0].Seq != 1 || snap.Events[0].Kind != humanSendKind {
		t.Fatalf("snapshot event: %+v", snap.Events[0])
	}

	if _, err := f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-viewer"}, "room-1"); err != nil {
		t.Fatalf("viewer read: %v", err)
	}
	events, operations, audits, seq, revision := f.roomCounts(t)
	if events != beforeEvents || operations != beforeOps || audits != beforeAudits || seq != beforeSeq || revision != beforeRevision {
		t.Fatalf("snapshot wrote state: %d/%d/%d/%d/%d -> %d/%d/%d/%d/%d", beforeEvents, beforeOps, beforeAudits, beforeSeq, beforeRevision, events, operations, audits, seq, revision)
	}
}

// Refusals disclose nothing; a lost lease poisons the store.
func TestReadonlySnapshotRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, err := f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-x")
	expectKernel(t, err, OutcomeRefused, ClassRoomNotFound)

	if _, err := f.admin.Exec(ctx, `UPDATE users SET disabled = true WHERE user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
	expectKernel(t, err, OutcomeRefused, ClassAuthorization)
	if _, err := f.admin.Exec(ctx, `UPDATE users SET disabled = false WHERE user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(ctx, `DELETE FROM room_members WHERE room_id = 'room-1' AND user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
	expectKernel(t, err, OutcomeRefused, ClassAuthorization)

	f.terminate(t, f.store.lease.pid, "lease")
	_, err = f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-viewer"}, "room-1")
	expectKernel(t, err, OutcomeRefused, ClassLeaseLost)
}

// Consistency validation: index disagreement, sequence gaps and corrupt
// encodings are refused as state_invalid with zero content disclosed.
func TestReadonlySnapshotStateValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	owner := AuthContext{Deployment: "dep-test", UserID: "u-owner"}
	if _, err := f.store.CommitHumanSend(ctx, humanSendCommand("u-owner", "snap-2", "slot2", "body")); err != nil {
		t.Fatal(err)
	}

	if _, err := f.admin.Exec(ctx, `UPDATE room_events SET kind = 'tampered' WHERE room_id = 'room-1' AND seq = 1`); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.ReadonlySnapshot(ctx, owner, "room-1")
	expectKernel(t, err, OutcomeRefused, ClassStateInvalid)

	if _, err := f.admin.Exec(ctx, `UPDATE room_events SET kind = '`+humanSendKind+`' WHERE room_id = 'room-1' AND seq = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(ctx, `UPDATE rooms SET current_seq = 5 WHERE room_id = 'room-1'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.ReadonlySnapshot(ctx, owner, "room-1")
	expectKernel(t, err, OutcomeRefused, ClassStateInvalid)

	if _, err := f.admin.Exec(ctx, `UPDATE rooms SET current_seq = 1 WHERE room_id = 'room-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(ctx, `UPDATE room_events SET event_encoding = 'garbage' WHERE room_id = 'room-1' AND seq = 1`); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.ReadonlySnapshot(ctx, owner, "room-1")
	expectKernel(t, err, OutcomeRefused, ClassStateInvalid)
}

// The snapshot takes the shared Room lock in the documented order: it waits on
// an in-flight writer and returns an exactly consistent view afterwards.
func TestReadonlySnapshotWaitsOnConcurrentWriter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	conn2 := schemaConn(t, f.dsn, f.schema)
	tx2, err := conn2.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(ctx, `SELECT host_mode, current_seq, revision FROM `+f.store.table("rooms")+` WHERE room_id = 'room-1' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan RoomSnapshot, 1)
	var rerr error
	go func() {
		snap, err := f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
		rerr = err
		done <- snap
	}()
	waitForLockWait(t, f.admin, f.store.work.PgConn().PID())
	if err := tx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	snap := <-done
	if rerr != nil {
		t.Fatal(rerr)
	}
	if uint64(len(snap.Events)) != snap.Seq || snap.HostMode != "native" {
		t.Fatalf("inconsistent snapshot: seq=%d events=%d", snap.Seq, len(snap.Events))
	}
	if snap.Seq > 0 && snap.Events[snap.Seq-1].Seq != snap.Seq {
		t.Fatalf("snapshot tail disagrees: %+v", snap.Events[snap.Seq-1])
	}
}
