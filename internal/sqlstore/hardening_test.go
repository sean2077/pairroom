package sqlstore

import (
	"context"
	"testing"
)

// The audit write participates in the same transaction: an injected audit
// failure rolls the whole commit back.
func TestAuditFailureRollsBackEverything(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.admin.Exec(ctx, `CREATE FUNCTION `+qualified(f.schema, "audit_fail")+`() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $body$`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(ctx, `CREATE TRIGGER audit_fail BEFORE INSERT ON `+qualified(f.schema, "audit")+` FOR EACH ROW EXECUTE FUNCTION `+qualified(f.schema, "audit_fail")+`()`); err != nil {
		t.Fatal(err)
	}
	res, err := f.store.Commit(ctx, f.command("audit-rb"))
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeNotCommitted || kerr.Class != ClassStatementFailed {
		t.Fatalf("audit failure: %+v", res)
	}
	events, operations, audits, currentSeq, revision := f.roomCounts(t)
	if events != 0 || operations != 0 || audits != 0 || currentSeq != 0 || revision != 0 {
		t.Fatalf("audit failure left partial state: events=%d operations=%d audits=%d seq=%d revision=%d", events, operations, audits, currentSeq, revision)
	}
}

// A successful Commit must not disturb the healthy work connection: further
// commits and resolves keep working, including with an unbounded caller
// context — the internal budget bounds the operation.
func TestSuccessfulOperationsLeaveTheConnectionUsable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if res, err := f.store.Commit(ctx, f.command("reuse-1")); err != nil || res.Seq != 1 {
		t.Fatalf("first commit: %+v %v", res, err)
	}
	second := f.command("reuse-2")
	second.ExpectedRevision = 1
	if res, err := f.store.Commit(ctx, second); err != nil || res.Seq != 2 {
		t.Fatalf("second commit: %+v %v", res, err)
	}
	cmd := f.command("reuse-1")
	res, err := f.store.Resolve(ctx, cmd.Auth, cmd.Key, cmd.Mutation)
	if err != nil || res.Outcome != OutcomeReplayed || res.Seq != 1 {
		t.Fatalf("resolve after commits: %+v %v", res, err)
	}
}

// A canceled caller context is a definite non-commit before any effect.
func TestCanceledContextIsDefiniteNonCommit(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := f.store.Commit(ctx, f.command("canceled-1"))
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeNotCommitted || kerr.Class != ClassCanceled {
		t.Fatalf("canceled commit: %+v", res)
	}
	if events, _, _, _, _ := f.roomCounts(t); events != 0 {
		t.Fatalf("canceled commit wrote %d events", events)
	}
}

// A member downgraded to viewer after an accepted append can still recover
// that operation but cannot append again.
func TestDowngradedMemberCanRecoverButNotAppend(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cmd := f.command("downgrade-1")
	if _, err := f.store.Commit(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(ctx, `UPDATE room_members SET role = 'viewer' WHERE room_id = 'room-1' AND user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	res, err := f.store.Resolve(ctx, cmd.Auth, cmd.Key, cmd.Mutation)
	if err != nil || res.Outcome != OutcomeReplayed {
		t.Fatalf("viewer recovery of its own accepted operation: %+v %v", res, err)
	}
	res, err = f.store.Commit(ctx, f.command("downgrade-2"))
	if kerr := kernelErr(t, res, err); kerr.Class != ClassAuthorization {
		t.Fatalf("viewer append: %+v", res)
	}
}
