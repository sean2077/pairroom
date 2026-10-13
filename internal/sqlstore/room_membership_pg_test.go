package sqlstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sean2077/pairroom/internal/model"
)

func prepareMembershipFixture(t *testing.T, f *fixture) RoomMembershipCommand {
	t.Helper()
	ctx := context.Background()
	if _, err := f.admin.Exec(ctx, `INSERT INTO users (user_id, org_id) VALUES ('u-target', 'org-test'), ('a-target', 'org-test'), ('z-target', 'org-test')`); err != nil {
		t.Fatal(err)
	}
	cmd := membershipCommand()
	creation := roomCreateCommand(t)
	creation.RoomID = cmd.RoomID
	if result, err := f.store.CommitRoomCreate(ctx, creation); err != nil || result.Outcome != OutcomeCommitted {
		t.Fatalf("create fixture Room: %+v %v", result, err)
	}
	return cmd
}

func membershipFixture(t *testing.T) (*fixture, RoomMembershipCommand) {
	t.Helper()
	f := newFixture(t)
	return f, prepareMembershipFixture(t, f)
}

func membershipExec(t *testing.T, conn *pgx.Conn, query string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func requireMembership(t *testing.T, f *fixture, roomID, userID, role string, disabled bool) {
	t.Helper()
	var gotRole string
	var gotDisabled bool
	if err := f.admin.QueryRow(context.Background(), `SELECT role, disabled FROM room_members WHERE room_id=$1 AND user_id=$2`, roomID, userID).Scan(&gotRole, &gotDisabled); err != nil || gotRole != role || gotDisabled != disabled {
		t.Fatalf("membership %s: role=%s disabled=%v, want %s/%v: %v", userID, gotRole, gotDisabled, role, disabled, err)
	}
}

func commitMembership(t *testing.T, f *fixture, cmd RoomMembershipCommand) Result {
	t.Helper()
	result, err := f.store.CommitRoomMembership(context.Background(), cmd)
	if err != nil || result.Outcome != OutcomeCommitted {
		t.Fatalf("membership commit: %+v %v", result, err)
	}
	return result
}

func TestRoomMembershipTransitionsAreAtomicAndRetainHistory(t *testing.T) {
	f, cmd := membershipFixture(t)
	for index, transition := range []struct {
		action   MembershipAction
		role     RoomRole
		wantRole string
		disabled bool
	}{
		{MembershipAdmit, RoomMember, "member", false},
		{MembershipChangeRole, RoomViewer, "viewer", false},
		{MembershipRemove, "", "viewer", true},
		{MembershipAdmit, RoomMember, "member", false},
	} {
		cmd.ClientID, cmd.ExpectedRevision = fmt.Sprintf("transition-%d", index), uint64(index+1)
		cmd.Intent.Action, cmd.Intent.Role = transition.action, transition.role
		result := commitMembership(t, f, cmd)
		wantSeq := int64(index + 2)
		if result.Seq != uint64(wantSeq) || result.Revision != uint64(wantSeq) {
			t.Fatalf("sequence/revision: %+v", result)
		}
		requireMembership(t, f, cmd.RoomID, cmd.Intent.TargetUserID, transition.wantRole, transition.disabled)
		if counts := creationCounts(t, f, cmd.RoomID); counts != [7]int64{1, 2, 2, 2, wantSeq, wantSeq, wantSeq} {
			t.Fatalf("partial or deleted control rows: %v", counts)
		}
		decoded, err := model.DecodeEnterpriseMembershipChange(result.Fact)
		if err != nil || decoded.EnterpriseMembershipIntent != cmd.Intent || result.Fact.Actor != model.ActorSystem {
			t.Fatalf("accepted fact: %+v %v", decoded, err)
		}
		var origin, auditKind, detail string
		if err := f.admin.QueryRow(context.Background(), `SELECT origin_user FROM room_events WHERE room_id=$1 AND seq=$2`, cmd.RoomID, wantSeq).Scan(&origin); err != nil || origin != "u-owner" {
			t.Fatalf("human provenance: %s %v", origin, err)
		}
		if err := f.admin.QueryRow(context.Background(), `SELECT kind, detail FROM audit WHERE room_id=$1 ORDER BY audit_seq DESC LIMIT 1`, cmd.RoomID).Scan(&auditKind, &detail); err != nil || auditKind != "room.membership."+string(transition.action) || detail != "" {
			t.Fatalf("audit: %s %q %v", auditKind, detail, err)
		}
	}
}

func TestRoomMembershipReplayChecksLiveACLAndFrozenIntent(t *testing.T) {
	f, cmd := membershipFixture(t)
	original := commitMembership(t, f, cmd)
	later := cmd
	later.ClientID, later.ExpectedRevision, later.Intent.Action, later.Intent.Role = "later-role", 2, MembershipChangeRole, RoomViewer
	commitMembership(t, f, later)
	before := creationCounts(t, f, cmd.RoomID)
	cmd.ExpectedRevision = 0 // Receipt recovery precedes CAS.
	for _, resolve := range []bool{false, true} {
		var got Result
		var err error
		if resolve {
			got, err = f.store.ResolveRoomMembership(context.Background(), cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
		} else {
			got, err = f.store.CommitRoomMembership(context.Background(), cmd)
		}
		if err != nil || got.Outcome != OutcomeReplayed || got.Seq != 2 || got.Revision != 2 || got.Fact.ID != original.Fact.ID || !got.Fact.CreatedAt.Equal(original.Fact.CreatedAt) || !bytes.Equal(got.Fact.Data, original.Fact.Data) {
			t.Fatalf("original receipt: %+v %v", got, err)
		}
	}
	changed := cmd
	changed.Intent.Role = RoomViewer
	if got, err := f.store.CommitRoomMembership(context.Background(), changed); kernelErr(t, got, err).Class != ClassPayloadConflict {
		t.Fatalf("changed frozen intent: %+v", got)
	}
	missing, err := f.store.ResolveRoomMembership(context.Background(), cmd.Auth, cmd.RoomID, "never-submitted", cmd.Intent)
	if err != nil || missing.Outcome != OutcomeNotCommitted || missing.Revision != 0 || missing.Seq != 0 || missing.Fact.ID != "" {
		t.Fatalf("absent receipt disclosed a cursor: %+v %v", missing, err)
	}
	// Revoking the caller hides both its original result and payload conflict.
	if _, err := f.admin.Exec(context.Background(), `UPDATE room_members SET disabled=true WHERE room_id=$1 AND user_id='u-owner'`, cmd.RoomID); err != nil {
		t.Fatal(err)
	}
	for _, resolve := range []bool{false, true} {
		var result Result
		var err error
		if resolve {
			result, err = f.store.ResolveRoomMembership(context.Background(), changed.Auth, changed.RoomID, changed.ClientID, changed.Intent)
		} else {
			result, err = f.store.CommitRoomMembership(context.Background(), changed)
		}
		if got := kernelErr(t, result, err); got.Class != ClassAuthorization || result.Fact.ID != "" || result.Revision != 0 {
			t.Fatalf("revoked receipt disclosure: %+v", result)
		}
	}
	if counts := creationCounts(t, f, cmd.RoomID); counts != before {
		t.Fatalf("replay/refusal wrote rows: %v -> %v", before, counts)
	}
	requireMembership(t, f, cmd.RoomID, "u-target", "viewer", false)
}

func TestRoomMembershipRefusesUnauthorizedAndInvalidState(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, *fixture, *RoomMembershipCommand)
		class  string
	}{
		{"disabled-owner", func(t *testing.T, f *fixture, _ *RoomMembershipCommand) {
			membershipExec(t, f.admin, `UPDATE users SET disabled=true WHERE user_id='u-owner'`)
		}, ClassAuthorization},
		{"owner-role-removed", func(t *testing.T, f *fixture, c *RoomMembershipCommand) {
			membershipExec(t, f.admin, `UPDATE room_members SET role='viewer' WHERE room_id=$1 AND user_id='u-owner'`, c.RoomID)
		}, ClassAuthorization},
		{"ordinary-member", func(t *testing.T, f *fixture, c *RoomMembershipCommand) {
			membershipExec(t, f.admin, `INSERT INTO room_members(room_id,org_id,user_id,role) VALUES($1,'org-test','u-viewer','member')`, c.RoomID)
			c.Auth.UserID = "u-viewer"
		}, ClassAuthorization},
		{"owner-target", func(_ *testing.T, _ *fixture, c *RoomMembershipCommand) { c.Intent.TargetUserID = "u-owner" }, ClassInvalidControlState},
		{"disabled-target", func(t *testing.T, f *fixture, _ *RoomMembershipCommand) {
			membershipExec(t, f.admin, `UPDATE users SET disabled=true WHERE user_id='u-target'`)
		}, ClassInvalidControlState},
		{"missing-target", func(_ *testing.T, _ *fixture, c *RoomMembershipCommand) { c.Intent.TargetUserID = "u-missing" }, ClassInvalidControlState},
		{"change-missing-member", func(_ *testing.T, _ *fixture, c *RoomMembershipCommand) { c.Intent.Action = MembershipChangeRole }, ClassInvalidControlState},
		{"remove-missing-member", func(_ *testing.T, _ *fixture, c *RoomMembershipCommand) {
			c.Intent.Action, c.Intent.Role = MembershipRemove, ""
		}, ClassInvalidControlState},
		{"stale-revision", func(_ *testing.T, _ *fixture, c *RoomMembershipCommand) { c.ExpectedRevision = 0 }, ClassRevisionConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, cmd := membershipFixture(t)
			test.change(t, f, &cmd)
			before := creationCounts(t, f, cmd.RoomID)
			result, err := f.store.CommitRoomMembership(context.Background(), cmd)
			if got := kernelErr(t, result, err); got.Class != test.class {
				t.Fatalf("refusal: %+v %v", result, err)
			}
			if counts := creationCounts(t, f, cmd.RoomID); counts != before {
				t.Fatalf("refused change wrote rows: %v -> %v", before, counts)
			}
		})
	}
}

func TestRoomMembershipAuditFailureRollsBackControlAndFacts(t *testing.T) {
	f, cmd := membershipFixture(t)
	before := creationCounts(t, f, cmd.RoomID)
	membershipExec(t, f.admin, `ALTER TABLE audit ADD CONSTRAINT reject_membership_audit CHECK (kind NOT LIKE 'room.membership.%')`)
	result, err := f.store.CommitRoomMembership(context.Background(), cmd)
	if got := kernelErr(t, result, err); got.Outcome != OutcomeNotCommitted || got.Class != ClassConstraintViolation {
		t.Fatalf("audit failure outcome: %+v", result)
	}
	if counts := creationCounts(t, f, cmd.RoomID); counts != before {
		t.Fatalf("audit failure left partial rows: %v -> %v", before, counts)
	}
}

func TestRoomMembershipRemoveRefusesUnreplayableBindingWithoutPartialRevocation(t *testing.T) {
	f, cmd := membershipFixture(t)
	commitMembership(t, f, cmd)
	membershipExec(t, f.admin, `INSERT INTO devices(device_id,org_id,user_id) VALUES('device-target','org-test','u-target')`)
	membershipExec(t, f.admin, `INSERT INTO agents(agent_id,org_id,device_id,stable_source_key) VALUES('agent-target','org-test','device-target','source-target')`)
	membershipExec(t, f.admin, `INSERT INTO bindings(binding_id,org_id,room_id,slot,generation,agent_id) VALUES('binding-target','org-test',$1,'slot1',1,'agent-target')`, cmd.RoomID)
	cmd.ClientID, cmd.ExpectedRevision, cmd.Intent.Action, cmd.Intent.Role = "remove", 2, MembershipRemove, ""
	before := creationCounts(t, f, cmd.RoomID)
	result, err := f.store.CommitRoomMembership(context.Background(), cmd)
	if got := kernelErr(t, result, err); got.Class != ClassStoredFactUnreadable {
		t.Fatalf("unreplayable retirement must be explicit: %+v", result)
	}
	requireMembership(t, f, cmd.RoomID, "u-target", "member", false)
	if counts := creationCounts(t, f, cmd.RoomID); counts != before {
		t.Fatalf("partial revocation: %v -> %v", before, counts)
	}
	// A deliberately retired fixture Binding no longer blocks membership
	// removal; the historical Binding and generation rows remain in place.
	membershipExec(t, f.admin, `UPDATE bindings SET retired_at=now(), retired_reason='explicit fixture retirement' WHERE binding_id='binding-target'`)
	commitMembership(t, f, cmd)
	requireMembership(t, f, cmd.RoomID, "u-target", "member", true)
}

func TestRoomMembershipCommitResponseLossResolvesWithoutRepeatingEffect(t *testing.T) {
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			proxy := startProxy(t, mode)
			f := newFixtureProxy(t, func(dsn string) string { return proxy.dsn(dsn) })
			cmd := prepareMembershipFixture(t, f)
			proxy.arm()
			result, err := f.store.CommitRoomMembership(context.Background(), cmd)
			if got := kernelErr(t, result, err); got.Outcome != OutcomeUnknown {
				t.Fatalf("lost signal: %+v", result)
			}
			before := creationCounts(t, f, cmd.RoomID)
			resolved, err := f.store.ResolveRoomMembership(context.Background(), cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "drop-response" {
				if resolved.Outcome != OutcomeReplayed || resolved.Seq != 2 || resolved.Revision != 2 || before != [7]int64{1, 2, 2, 2, 2, 2, 2} {
					t.Fatalf("accepted membership: %+v %v", resolved, before)
				}
			} else if resolved.Outcome != OutcomeNotCommitted || resolved.Seq != 0 || resolved.Revision != 0 || before != [7]int64{1, 1, 2, 2, 1, 1, 1} {
				t.Fatalf("absent membership: %+v %v", resolved, before)
			}
			if after := creationCounts(t, f, cmd.RoomID); after != before {
				t.Fatalf("verification repeated effect: %v -> %v", before, after)
			}
		})
	}
}

func TestRoomMembershipCorruptReceiptIsRefusedWithoutDisclosure(t *testing.T) {
	f, cmd := membershipFixture(t)
	original := commitMembership(t, f, cmd)
	change, err := model.DecodeEnterpriseMembershipChange(original.Fact)
	if err != nil {
		t.Fatal(err)
	}
	change.Role = RoomViewer
	original.Fact.Data, err = json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	encoding, err := encodeStoredFact(original.Fact, userFactOrigin(cmd.Auth.UserID))
	if err != nil {
		t.Fatal(err)
	}
	membershipExec(t, f.admin, `UPDATE room_events SET event_encoding=$1 WHERE room_id=$2 AND seq=2`, encoding, cmd.RoomID)
	for _, resolve := range []bool{false, true} {
		var result Result
		if resolve {
			result, err = f.store.ResolveRoomMembership(context.Background(), cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
		} else {
			result, err = f.store.CommitRoomMembership(context.Background(), cmd)
		}
		if got := kernelErr(t, result, err); got.Class != ClassStoredFactUnreadable || result.Fact.ID != "" {
			t.Fatalf("corrupt receipt disclosure: %+v %v", result, err)
		}
	}
}

type membershipLockTrace struct {
	mu      sync.Mutex
	armed   bool
	queries []string
}

func (r *membershipLockTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.armed || !strings.Contains(query.SQL, " FOR ") {
		return ctx
	}
	mode := "share"
	if strings.HasSuffix(query.SQL, "FOR UPDATE") {
		mode = "update"
	}
	switch {
	case strings.Contains(query.SQL, `.ownership`):
		r.queries = append(r.queries, "ownership:"+mode)
	case strings.Contains(query.SQL, `.users`):
		r.queries = append(r.queries, "user:"+query.Args[0].(string)+":"+mode)
	case strings.Contains(query.SQL, `.room_members`):
		r.queries = append(r.queries, "member:"+query.Args[1].(string)+":"+mode)
	case strings.Contains(query.SQL, `.rooms`):
		r.queries = append(r.queries, "room:"+mode)
	}
	return ctx
}

func (*membershipLockTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRoomMembershipLocksUsersThenMembersInStableOrder(t *testing.T) {
	for _, target := range []string{"a-target", "z-target"} {
		t.Run(target, func(t *testing.T) {
			f := newRawFixture(t)
			trace := &membershipLockTrace{}
			f.store = openStoreWithTracer(t, f, trace)
			cmd := prepareMembershipFixture(t, f)
			cmd.Intent.TargetUserID = target
			trace.armed = true
			commitMembership(t, f, cmd)
			want := "ownership:update,user:a-target:share,user:u-owner:share,member:a-target:update,member:u-owner:share,room:update"
			if target == "z-target" {
				want = "ownership:update,user:u-owner:share,user:z-target:share,member:u-owner:share,member:z-target:update,room:update"
			}
			if got := strings.Join(trace.queries, ","); got != want {
				t.Fatalf("lock order/modes: %s, want %s", got, want)
			}
		})
	}
}

func TestRoomMembershipAbsentTargetWaitsForOwnership(t *testing.T) {
	f, cmd := membershipFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	blocker := schemaConn(t, f.dsn, f.schema)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT epoch FROM ownership WHERE id FOR SHARE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.store.CommitRoomMembership(ctx, cmd); done <- err }()
	waitForControlLockWait(t, f.admin, f.store.work.PgConn().PID(), blocker.PgConn().PID())
	select {
	case err := <-done:
		t.Fatalf("admission passed unsettled ownership: %v", err)
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRoomMembershipRemovalSerializesWithAcceptedHumanSend(t *testing.T) {
	f, cmd := membershipFixture(t)
	commitMembership(t, f, cmd)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	barrier := &creationCommitBarrier{arrived: make(chan struct{}), release: make(chan struct{})}
	f.store = openStoreWithTracer(t, f, barrier)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	work := schemaConn(t, f.dsn, f.schema)
	if err := configureSession(ctx, work, f.schema); err != nil {
		t.Fatal(err)
	}
	// Test-only second work connection with the same admitted incarnation:
	// this proves database ordering independently of the local Store semaphore.
	controller := &Store{cfg: f.cfg, lease: f.store.lease, work: work, sem: make(chan struct{}, 1)}
	send := humanSendCommand("u-target", "before-removal", "slot2", "accepted before membership removal")
	send.RoomID, send.Fact.RoomID, send.ExpectedRevision = cmd.RoomID, cmd.RoomID, 2
	type reply struct {
		result Result
		err    error
	}
	sent := make(chan reply, 1)
	go func() { result, err := f.store.CommitHumanSend(ctx, send); sent <- reply{result, err} }()
	select {
	case <-barrier.arrived:
	case result := <-sent:
		t.Fatalf("send did not reach COMMIT: %+v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cmd.ClientID, cmd.ExpectedRevision, cmd.Intent.Action, cmd.Intent.Role = "remove-race", 2, MembershipRemove, ""
	removed := make(chan reply, 1)
	go func() { result, err := controller.CommitRoomMembership(ctx, cmd); removed <- reply{result, err} }()
	waitForControlLockWait(t, f.admin, controller.work.PgConn().PID(), f.store.work.PgConn().PID())
	release()
	first := <-sent
	if first.err != nil || first.result.Outcome != OutcomeCommitted || first.result.Seq != 3 {
		t.Fatalf("accepted earlier send: %+v", first)
	}
	stale := <-removed
	if got := kernelErr(t, stale.result, stale.err); got.Class != ClassRevisionConflict || stale.result.Revision != 3 {
		t.Fatalf("stale removal crossed accepted send: %+v", stale)
	}
	requireMembership(t, f, cmd.RoomID, "u-target", "member", false)
	// A fresh caller plan explicitly retries after the known CAS refusal.
	cmd.ExpectedRevision = 3
	result, err := controller.CommitRoomMembership(ctx, cmd)
	if err != nil || result.Outcome != OutcomeCommitted || result.Seq != 4 {
		t.Fatalf("fresh removal: %+v %v", result, err)
	}
	send = humanSendCommand("u-target", "after-removal", "slot2", "must not be accepted")
	send.RoomID, send.Fact.RoomID, send.ExpectedRevision = cmd.RoomID, cmd.RoomID, 4
	result, err = f.store.CommitHumanSend(ctx, send)
	if got := kernelErr(t, result, err); got.Class != ClassAuthorization {
		t.Fatalf("removed member wrote again: %+v", result)
	}
	if counts := creationCounts(t, f, cmd.RoomID); counts != [7]int64{1, 2, 2, 2, 4, 4, 4} {
		t.Fatalf("ordering left extra/partial effects: %v", counts)
	}
	requireMembership(t, f, cmd.RoomID, "u-target", "member", true)
}

func TestRoomMembershipDisabledOwnerStopsControlButNotExistingMember(t *testing.T) {
	f, cmd := membershipFixture(t)
	commitMembership(t, f, cmd)
	membershipExec(t, f.admin, `UPDATE users SET disabled=true WHERE user_id='u-owner'`)
	cmd.ClientID, cmd.ExpectedRevision, cmd.Intent.Action, cmd.Intent.Role = "disabled-owner-role", 2, MembershipChangeRole, RoomViewer
	result, err := f.store.CommitRoomMembership(context.Background(), cmd)
	if got := kernelErr(t, result, err); got.Class != ClassAuthorization {
		t.Fatalf("disabled owner retained control: %+v", result)
	}
	send := humanSendCommand("u-target", "member-continues", "slot2", "existing member authority survives owner disable")
	send.RoomID, send.Fact.RoomID, send.ExpectedRevision = cmd.RoomID, cmd.RoomID, 2
	result, err = f.store.CommitHumanSend(context.Background(), send)
	if err != nil || result.Outcome != OutcomeCommitted || result.Seq != 3 {
		t.Fatalf("unaffected member was stopped: %+v %v", result, err)
	}
	var owner string
	if err := f.admin.QueryRow(context.Background(), `SELECT owner_user_id FROM rooms WHERE room_id=$1`, cmd.RoomID).Scan(&owner); err != nil || owner != "u-owner" {
		t.Fatalf("owner provenance changed: %s %v", owner, err)
	}
	requireMembership(t, f, cmd.RoomID, "u-target", "member", false)
}
