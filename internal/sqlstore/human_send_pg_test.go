package sqlstore

import (
	"context"
	"testing"
)

// One human_send commits its fact, operation identity and audit atomically;
// a regenerated candidate with the same intent replays the original fact.
func TestCommitHumanSendAndReplay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cmd := humanSendCommand("u-owner", "hs-1", "slot2", "hello")

	res, err := f.store.CommitHumanSend(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeCommitted || res.Seq != 1 || res.Revision != 1 || res.Fact.ID != "evt-hs-1" {
		t.Fatalf("commit: %+v", res)
	}
	if events, operations, audits, seq, revision := f.roomCounts(t); events != 1 || operations != 1 || audits != 1 || seq != 1 || revision != 1 {
		t.Fatalf("counts: events=%d operations=%d audits=%d seq=%d revision=%d", events, operations, audits, seq, revision)
	}

	regenerated := humanSendCommand("u-owner", "hs-1", "slot2", "hello")
	regenerated.Fact = humanSendCandidate("u-owner", "hs-1", "slot2", "hello", map[string]any{
		"id":         "relay-regenerated",
		"created_at": "2026-10-13T01:02:03.123456789Z",
		"updated_at": "2026-10-13T01:02:03.123456789Z",
	})
	regenerated.Fact.ID = "evt-regenerated"
	res2, err := f.store.CommitHumanSend(ctx, regenerated)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Outcome != OutcomeReplayed || res2.Seq != 1 || res2.Revision != 1 || res2.Fact.ID != "evt-hs-1" {
		t.Fatalf("replay must return the original fact and result revision: %+v", res2)
	}
	if events, operations, _, _, _ := f.roomCounts(t); events != 1 || operations != 1 {
		t.Fatalf("replay appended: events=%d operations=%d", events, operations)
	}

	changed := humanSendCommand("u-owner", "hs-1", "slot2", "different text")
	res3, err := f.store.CommitHumanSend(ctx, changed)
	if kerr := kernelErr(t, res3, err); kerr.Outcome != OutcomeConflict || kerr.Class != ClassPayloadConflict {
		t.Fatalf("changed intent must conflict: %+v", res3)
	}

	// Resolve mirrors the same three outcomes.
	resolved, err := f.store.ResolveHumanSend(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
	if err != nil || resolved.Outcome != OutcomeReplayed || resolved.Seq != 1 || resolved.Fact.ID != "evt-hs-1" {
		t.Fatalf("resolve replay: %+v %v", resolved, err)
	}
	resolved, err = f.store.ResolveHumanSend(ctx, changed.Auth, changed.RoomID, changed.ClientID, changed.Intent)
	if kerr := kernelErr(t, resolved, err); kerr.Outcome != OutcomeConflict {
		t.Fatalf("resolve conflict: %+v", resolved)
	}
	resolved, err = f.store.ResolveHumanSend(ctx, cmd.Auth, cmd.RoomID, "hs-absent", HumanSendIntent{To: "slot2", Text: "x"})
	if err != nil || resolved.Outcome != OutcomeNotCommitted {
		t.Fatalf("resolve absence: %+v %v", resolved, err)
	}
}

// The operation key is principal-scoped: two principals reusing one client ID
// produce two independent facts; the same principal replays.
func TestHumanSendPrincipalIsolation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.admin.Exec(ctx, `INSERT INTO users (user_id) VALUES ('u-owner2')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(ctx, `INSERT INTO room_members (room_id, user_id, role) VALUES ('room-1', 'u-owner2', 'member')`); err != nil {
		t.Fatal(err)
	}
	resA, err := f.store.CommitHumanSend(ctx, humanSendCommand("u-owner", "shared-id", "slot2", "from owner"))
	if err != nil || resA.Outcome != OutcomeCommitted || resA.Seq != 1 {
		t.Fatalf("owner send: %+v %v", resA, err)
	}
	second := humanSendCommand("u-owner2", "shared-id", "slot2", "from owner2")
	second.ExpectedRevision = 1
	second.Fact.ID = "evt-shared-owner2"
	resB, err := f.store.CommitHumanSend(ctx, second)
	if err != nil || resB.Outcome != OutcomeCommitted || resB.Seq != 2 {
		t.Fatalf("second principal must be independent: %+v %v", resB, err)
	}
	resA2, err := f.store.CommitHumanSend(ctx, humanSendCommand("u-owner", "shared-id", "slot2", "from owner"))
	if err != nil || resA2.Outcome != OutcomeReplayed || resA2.Fact.ID != resA.Fact.ID {
		t.Fatalf("same principal must replay: %+v %v", resA2, err)
	}
	if events, _, _, _, _ := f.roomCounts(t); events != 2 {
		t.Fatalf("events = %d, want 2", events)
	}
}

// Live authorization ordering: readAllowed gates every disclosure including
// the caller's own replay; a new send additionally requires writer roles; a
// disabled or removed principal is refused before any disclosure.
func TestHumanSendAuthorization(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cmd := humanSendCommand("u-owner", "hs-auth", "slot2", "owner body")
	if _, err := f.store.CommitHumanSend(ctx, cmd); err != nil {
		t.Fatal(err)
	}

	// A role downgrade to viewer still allows recovering the accepted result
	// but refuses a new send.
	if _, err := f.admin.Exec(ctx, `UPDATE room_members SET role = 'viewer' WHERE room_id = 'room-1' AND user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	res, err := f.store.CommitHumanSend(ctx, humanSendCommand("u-owner", "hs-auth", "slot2", "owner body"))
	if err != nil || res.Outcome != OutcomeReplayed {
		t.Fatalf("viewer replay: %+v %v", res, err)
	}
	res, err = f.store.CommitHumanSend(ctx, humanSendCommand("u-owner", "hs-new", "slot2", "new body"))
	if kerr := kernelErr(t, res, err); kerr.Class != ClassAuthorization {
		t.Fatalf("viewer new send: %+v", res)
	}
	res, err = f.store.ResolveHumanSend(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
	if err != nil || res.Outcome != OutcomeReplayed {
		t.Fatalf("viewer resolve: %+v %v", res, err)
	}

	// Disabled user: zero disclosure.
	if _, err := f.admin.Exec(ctx, `UPDATE users SET disabled = true WHERE user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	res, err = f.store.CommitHumanSend(ctx, humanSendCommand("u-owner", "hs-auth", "slot2", "owner body"))
	if kerr := kernelErr(t, res, err); kerr.Class != ClassAuthorization {
		t.Fatalf("disabled replay: %+v", res)
	}
	res, err = f.store.ResolveHumanSend(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassAuthorization {
		t.Fatalf("disabled resolve: %+v", res)
	}

	// Removed membership: zero disclosure.
	if _, err := f.admin.Exec(ctx, `UPDATE users SET disabled = false WHERE user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(ctx, `DELETE FROM room_members WHERE room_id = 'room-1' AND user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	res, err = f.store.ResolveHumanSend(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassAuthorization {
		t.Fatalf("removed member resolve: %+v", res)
	}
}

// A forged candidate (typed mismatch) is refused before SQL: nothing is
// written and nothing is disclosed.
func TestHumanSendTypedMismatchZeroWrites(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	forged := humanSendCommand("u-owner", "hs-forged", "slot2", "intended text")
	forged.Fact = humanSendCandidate("u-owner", "hs-forged", "slot2", "other text", nil)
	res, err := f.store.CommitHumanSend(ctx, forged)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassInvalidFact {
		t.Fatalf("forged text: %+v", res)
	}
	unknown := humanSendCommand("u-owner", "hs-forged", "slot2", "intended text")
	unknown.Fact = humanSendCandidate("u-owner", "hs-forged", "slot2", "intended text", map[string]any{"mystery": 1})
	res, err = f.store.CommitHumanSend(ctx, unknown)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassInvalidFact {
		t.Fatalf("unknown field: %+v", res)
	}
	if events, operations, audits, _, _ := f.roomCounts(t); events != 0 || operations != 0 || audits != 0 {
		t.Fatalf("refusals wrote state: events=%d operations=%d audits=%d", events, operations, audits)
	}
}
