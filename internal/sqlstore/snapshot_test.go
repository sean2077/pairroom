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

// The snapshot takes the shared Room lock in the documented order and, when a
// real writer commits an in-flight fact behind an external barrier, observes
// the complete new state — a stale or mixed view fails these assertions.
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

	// The writer queues behind the barrier first (it acquires the work
	// connection), so the snapshot queues behind the writer.
	cmd := humanSendCommand("u-owner", "conc-1", "slot2", "concurrent body")
	commitDone := make(chan Result, 1)
	var cerr error
	go func() {
		res, err := f.store.CommitHumanSend(ctx, cmd)
		cerr = err
		commitDone <- res
	}()
	waitForLockWait(t, f.admin, f.store.work.PgConn().PID())

	snapDone := make(chan RoomSnapshot, 1)
	var serr error
	go func() {
		snap, err := f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
		serr = err
		snapDone <- snap
	}()

	if err := tx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	res := <-commitDone
	if cerr != nil || res.Outcome != OutcomeCommitted || res.Seq != 1 {
		t.Fatalf("in-flight writer: %+v %v", res, cerr)
	}
	snap := <-snapDone
	if serr != nil {
		t.Fatal(serr)
	}
	if snap.Seq != 1 || snap.Revision != 1 || len(snap.Events) != 1 || snap.Events[0].ID != cmd.Fact.ID {
		t.Fatalf("snapshot must observe the complete new state: %+v", snap)
	}
	if events, operations, audits, seq, revision := f.roomCounts(t); events != 1 || operations != 1 || audits != 1 || seq != 1 || revision != 1 {
		t.Fatalf("unexpected state after the chain: %d/%d/%d/%d/%d", events, operations, audits, seq, revision)
	}
}

// A corrupt-but-bigint-valid sequence counter is refused as state_invalid —
// never used as an allocation hint, never a panic or OOM.
func TestReadonlySnapshotHugeCorruptSequenceDoesNotPanic(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.store.CommitHumanSend(ctx, humanSendCommand("u-owner", "huge-1", "slot2", "body")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(ctx, `UPDATE rooms SET current_seq = 9223372036854775807 WHERE room_id = 'room-1'`); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
	expectKernel(t, err, OutcomeRefused, ClassStateInvalid)
	if _, err := f.admin.Exec(ctx, `UPDATE rooms SET current_seq = 1 WHERE room_id = 'room-1'`); err != nil {
		t.Fatal(err)
	}
}
