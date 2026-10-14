package sqlstore

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// One committed command appends the Room fact, its operation identity and its
// audit record atomically, and advances the Room sequence and revision.
func TestCommitAppendsFactOperationAndAuditAtomically(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cmd := f.command("a1")

	res, err := f.store.Commit(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeCommitted || res.Seq != 1 || res.Revision != 1 {
		t.Fatalf("result: %+v", res)
	}
	if res.Fact.ID != cmd.Mutation.Fact.ID || res.Fact.Seq != 1 {
		t.Fatalf("stored fact identity changed: %+v", res.Fact)
	}
	events, operations, audits, currentSeq, revision := f.roomCounts(t)
	if events != 1 || operations != 1 || audits != 1 || currentSeq != 1 || revision != 1 {
		t.Fatalf("counts: events=%d operations=%d audits=%d seq=%d revision=%d", events, operations, audits, currentSeq, revision)
	}

	var encoding []byte
	if err := f.admin.QueryRow(ctx, `SELECT event_encoding FROM room_events WHERE room_id = 'room-1' AND seq = 1`).Scan(&encoding); err != nil {
		t.Fatal(err)
	}
	var record storedEventRecord
	if err := json.Unmarshal(encoding, &record); err != nil {
		t.Fatal(err)
	}
	stored := record.Event
	if stored.ID != cmd.Mutation.Fact.ID || stored.Seq != 1 || stored.Kind != "room.note" ||
		!bytes.Equal(stored.Data, cmd.Mutation.Fact.Data) || !stored.CreatedAt.Equal(cmd.Mutation.Fact.CreatedAt) {
		t.Fatalf("stored encoding differs from the committed fact: %+v", stored)
	}
}

// The expected revision is a strict compare-and-swap against the locked Room
// revision; zero is a legal initial value and there is no bypass.
func TestExpectedRevisionIsAStrictCAS(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if res, err := f.store.Commit(ctx, f.command("cas-1")); err != nil || res.Revision != 1 {
		t.Fatalf("initial revision 0: %+v %v", res, err)
	}
	res, err := f.store.Commit(ctx, f.command("cas-2"))
	if kerr := kernelErr(t, res, err); kerr.Class != ClassRevisionConflict {
		t.Fatalf("stale plan was not refused: %+v", res)
	}
	if res.Revision != 1 {
		t.Fatalf("conflict must report the current revision: %+v", res)
	}
	next := f.command("cas-3")
	next.ExpectedRevision = 1
	if res, err := f.store.Commit(ctx, next); err != nil || res.Revision != 2 {
		t.Fatalf("fresh plan: %+v %v", res, err)
	}
	if events, _, _, _, revision := f.roomCounts(t); events != 2 || revision != 2 {
		t.Fatalf("stale attempts must not append: events=%d revision=%d", events, revision)
	}
}

// Same key and identical stable mutation replays the original stored fact —
// never the freshly constructed candidate — and any changed stable field under
// the same key is a definite conflict.
func TestIdempotentReplayReturnsTheOriginalFact(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.command("idem-1")
	res1, err := f.store.Commit(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	// A later operation advances the Room revision; the replay must still
	// return the original result revision, never the current one.
	later := f.command("idem-2")
	later.ExpectedRevision = 1
	if res, err := f.store.Commit(ctx, later); err != nil || res.Revision != 2 {
		t.Fatalf("later commit: %+v %v", res, err)
	}

	recandidate := f.command("idem-1")
	recandidate.Mutation.Fact.ID = "evt-regenerated"
	recandidate.Mutation.Fact.CreatedAt = first.Mutation.Fact.CreatedAt.Add(time.Hour)
	res2, err := f.store.Commit(ctx, recandidate)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Outcome != OutcomeReplayed || res2.Seq != res1.Seq {
		t.Fatalf("replay outcome: %+v", res2)
	}
	if res2.Revision != 1 {
		t.Fatalf("replay must return the original result revision, got %d", res2.Revision)
	}
	if res2.Fact.ID != first.Mutation.Fact.ID || !res2.Fact.CreatedAt.Equal(first.Mutation.Fact.CreatedAt) {
		t.Fatalf("replay must return the original fact, got %+v", res2.Fact)
	}
	resolved, err := f.store.Resolve(ctx, recandidate.Auth, recandidate.Key, recandidate.Mutation)
	if err != nil || resolved.Outcome != OutcomeReplayed || resolved.Seq != res1.Seq || resolved.Revision != 1 || resolved.Fact.ID != first.Mutation.Fact.ID {
		t.Fatalf("resolve must return the original result: %+v %v", resolved, err)
	}

	changedBody := f.command("idem-1")
	changedBody.Mutation.Fact.Data = []byte(`{"text":"changed"}`)
	res3, err := f.store.Commit(ctx, changedBody)
	if kerr := kernelErr(t, res3, err); kerr.Outcome != OutcomeConflict || kerr.Class != ClassPayloadConflict {
		t.Fatalf("changed body must conflict: %+v", res3)
	}
	changedKind := f.command("idem-1")
	changedKind.Mutation.Fact.Kind = "room.other"
	res4, err := f.store.Commit(ctx, changedKind)
	if kerr := kernelErr(t, res4, err); kerr.Outcome != OutcomeConflict || kerr.Class != ClassPayloadConflict {
		t.Fatalf("changed fact kind must conflict: %+v", res4)
	}
	if events, operations, _, _, revision := f.roomCounts(t); events != 2 || operations != 2 || revision != 2 {
		t.Fatalf("replays and conflicts must not append: events=%d operations=%d revision=%d", events, operations, revision)
	}
}

// The authoritative encoding preserves the event id and nanosecond creation
// time; the timestamptz index column is a coarser derived projection and is
// never the replay source.
func TestStoredEncodingPreservesNanosecondsAndEventID(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cmd := f.command("ns-1")
	cmd.Mutation.Fact.CreatedAt = time.Date(2026, 10, 12, 15, 0, 0, 123456789, time.UTC)
	if _, err := f.store.Commit(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	var encoding []byte
	if err := f.admin.QueryRow(ctx, `SELECT event_encoding FROM room_events WHERE room_id = 'room-1' AND seq = 1`).Scan(&encoding); err != nil {
		t.Fatal(err)
	}
	var record storedEventRecord
	if err := json.Unmarshal(encoding, &record); err != nil {
		t.Fatal(err)
	}
	stored := record.Event
	if stored.ID != cmd.Mutation.Fact.ID || stored.CreatedAt.Nanosecond() != 123456789 {
		t.Fatalf("nanoseconds or event id lost: %+v", stored)
	}
}

// A constraint failure in any prepared statement rolls back the whole
// transaction: no fact, no operation, no audit, no sequence movement.
func TestConstraintFailureRollsBackTheWholeTransaction(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.command("rb-1")
	if _, err := f.store.Commit(ctx, first); err != nil {
		t.Fatal(err)
	}
	duplicate := f.command("rb-2")
	duplicate.Mutation.Fact.ID = first.Mutation.Fact.ID
	duplicate.ExpectedRevision = 1
	res, err := f.store.Commit(ctx, duplicate)
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeNotCommitted || kerr.Class != ClassConstraintViolation {
		t.Fatalf("duplicate event id: %+v", res)
	}
	events, operations, audits, currentSeq, revision := f.roomCounts(t)
	if events != 1 || operations != 1 || audits != 1 || currentSeq != 1 || revision != 1 {
		t.Fatalf("rollback left partial state: events=%d operations=%d audits=%d seq=%d revision=%d", events, operations, audits, currentSeq, revision)
	}
}

// A COMMIT whose signal is lost is reported as unknown — never as rolled
// back and never as success. The two real boundaries — a forwarded COMMIT
// whose response is dropped, and a connection break before the server sees
// COMMIT — are injected by the protocol proxy in boundary_test.go.

// Resolve is the only sanctioned adjudication: a stored operation replays its
// original fact, a complete check proves absence, a same-key different
// mutation conflicts, and an interrupted check is unknown.
func TestResolveOutcomes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cmd := f.command("res-1")
	if _, err := f.store.Commit(ctx, cmd); err != nil {
		t.Fatal(err)
	}

	recandidate := f.command("res-1")
	recandidate.Mutation.Fact.ID = "evt-regenerated"
	res, err := f.store.Resolve(ctx, recandidate.Auth, recandidate.Key, recandidate.Mutation)
	if err != nil || res.Outcome != OutcomeReplayed || res.Revision != 1 || res.Fact.ID != cmd.Mutation.Fact.ID {
		t.Fatalf("resolve replay: %+v %v", res, err)
	}

	absent := f.command("res-absent")
	res, err = f.store.Resolve(ctx, absent.Auth, absent.Key, absent.Mutation)
	if err != nil || res.Outcome != OutcomeNotCommitted {
		t.Fatalf("resolve absence: %+v %v", res, err)
	}

	conflict := f.command("res-1")
	conflict.Mutation.Fact.Data = []byte(`{"text":"changed"}`)
	res, err = f.store.Resolve(ctx, conflict.Auth, conflict.Key, conflict.Mutation)
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeConflict || kerr.Class != ClassPayloadConflict {
		t.Fatalf("resolve conflict: %+v", res)
	}
}

// A dependency change between discovery and locking restarts the pure
// discovery phase and then refuses the stale plan. The concurrent-change
// boundary and the restart-limit path live in boundary_test.go, driven by a
// real lock barrier and a driver tracer.

// Unsupported surfaces and non-writer roles are refused with stable classes
// and no effect.
func TestUnsupportedAndAuthorizationRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	viewer := f.command("vz-1")
	viewer.Auth.UserID = "u-viewer"
	viewer.Key.Principal = "u-viewer"
	res, err := f.store.Commit(ctx, viewer)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassAuthorization {
		t.Fatalf("viewer write: %+v", res)
	}

	if _, err := f.admin.Exec(ctx, `UPDATE users SET disabled = true WHERE user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	res, err = f.store.Commit(ctx, f.command("vz-2"))
	if kerr := kernelErr(t, res, err); kerr.Class != ClassAuthorization {
		t.Fatalf("disabled user write: %+v", res)
	}

	binding := f.command("vz-3")
	binding.Key.Namespace = "binding"
	binding.Key.BindingID = "b-1"
	binding.Key.Generation = 1
	res, err = f.store.Commit(ctx, binding)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassUnsupportedNamespace {
		t.Fatalf("binding namespace: %+v", res)
	}

	unknownOp := f.command("vz-4")
	unknownOp.Key.OpKind = "send"
	res, err = f.store.Commit(ctx, unknownOp)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassUnsupportedOp {
		t.Fatalf("unknown op: %+v", res)
	}

	future := f.command("vz-5")
	future.Mutation.Version = 2
	res, err = f.store.Commit(ctx, future)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassUnsupportedMutation {
		t.Fatalf("future mutation: %+v", res)
	}

	spoofedActor := f.command("vz-6")
	spoofedActor.Mutation.Fact.Actor = model.ActorSlot1
	res, err = f.store.Commit(ctx, spoofedActor)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassInvalidFact {
		t.Fatalf("actor spoof: %+v", res)
	}

	spoofedDeployment := f.command("vz-7")
	spoofedDeployment.Auth.Deployment = "dep-other"
	spoofedDeployment.Key.Deployment = "dep-other"
	res, err = f.store.Commit(ctx, spoofedDeployment)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassInvalidRequest {
		t.Fatalf("deployment spoof: %+v", res)
	}
	if events, operations, _, _, _ := f.roomCounts(t); events != 0 || operations != 0 {
		t.Fatalf("refusals wrote state: events=%d operations=%d", events, operations)
	}
}

// Sequence allocation stays contiguous and every operation points at its own
// Room fact.
func TestSequenceStaysContiguous(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i, id := range []string{"seq-1", "seq-2", "seq-3"} {
		cmd := f.command(id)
		cmd.ExpectedRevision = uint64(i)
		res, err := f.store.Commit(ctx, cmd)
		if err != nil {
			t.Fatal(err)
		}
		if res.Seq != uint64(i+1) {
			t.Fatalf("commit %d: seq = %d", i, res.Seq)
		}
	}
	rows, err := f.admin.Query(ctx, `SELECT result_seq FROM operations ORDER BY result_seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 3 || seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 3 {
		t.Fatalf("operation result sequences: %v", seqs)
	}
	if events, operations, audits, currentSeq, revision := f.roomCounts(t); events != 3 || operations != 3 || audits != 3 || currentSeq != 3 || revision != 3 {
		t.Fatalf("counts: events=%d operations=%d audits=%d seq=%d revision=%d", events, operations, audits, currentSeq, revision)
	}
}
