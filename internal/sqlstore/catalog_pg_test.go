package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func catalogPGCommit(t *testing.T, f *fixture, auth AuthContext, id string, revision uint64, intent CatalogIntent) Result {
	t.Helper()
	result, err := f.store.CommitCatalog(context.Background(), CatalogCommand{Auth: auth, ClientID: id, ExpectedRevision: revision, Intent: intent})
	if err != nil || result.Outcome != OutcomeCommitted || result.Catalog == nil {
		t.Fatalf("catalog %s: %+v %v", id, result, err)
	}
	return result
}
func catalogPGRead(t *testing.T, f *fixture, auth AuthContext, query CatalogQuery) CatalogPage {
	t.Helper()
	page, err := f.store.ReadCatalog(context.Background(), auth, query)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestCatalogPGProjectMembershipAndRoomInvitationStayIndependent(t *testing.T) {
	g := newGovernanceFixture(t, false)
	f := g.f
	ctx := context.Background()
	catalogPGCommit(t, f, g.owner.Auth, "project-create", 0, CatalogIntent{Kind: "project.create", ProjectID: "project-1", Name: "Logical research"})
	catalogPGCommit(t, f, g.owner.Auth, "room-project", 2, CatalogIntent{Kind: "room.project", RoomID: g.intent.RoomID, ProjectID: "project-1"})
	if p := catalogPGRead(t, f, g.target.Auth, CatalogQuery{Resource: "projects"}); len(p.Projects) != 0 {
		t.Fatal("Room invitation granted Project discovery")
	}
	if p := catalogPGRead(t, f, g.target.Auth, CatalogQuery{Resource: "rooms"}); len(p.Rooms) != 1 || p.Rooms[0].ProjectID != "" || p.Rooms[0].ProjectName != "" {
		t.Fatal("Room-only member saw private Project metadata")
	}
	if _, e := f.store.ReadCatalog(ctx, g.target.Auth, CatalogQuery{Resource: "rooms", ProjectID: "project-1"}); e == nil {
		t.Fatal("known Project ID bypassed explicit membership")
	}
	catalogPGCommit(t, f, g.owner.Auth, "admit-project-admin", 1, CatalogIntent{Kind: "project.member.add", ProjectID: "project-1", TargetUserID: g.admin.Auth.UserID})
	if p := catalogPGRead(t, f, g.admin.Auth, CatalogQuery{Resource: "projects"}); len(p.Projects) != 1 {
		t.Fatal("explicit Project member could not discover Project")
	}
	if p := catalogPGRead(t, f, g.admin.Auth, CatalogQuery{Resource: "rooms", ProjectID: "project-1"}); len(p.Rooms) != 0 {
		t.Fatal("Project membership or administrator role granted Room access")
	}
	if _, e := f.store.CommitCatalog(ctx, CatalogCommand{Auth: g.admin.Auth, ClientID: "admin-retitle", ExpectedRevision: 3, Intent: CatalogIntent{Kind: "room.rename", RoomID: g.intent.RoomID, Name: "Seized"}}); e == nil {
		t.Fatal("administrator retitled unreadable Room")
	}
	if p := catalogPGRead(t, f, g.owner.Auth, CatalogQuery{Resource: "room", RoomID: g.intent.RoomID}); len(p.Rooms) != 1 || p.Rooms[0].Name == "" || p.Rooms[0].ProjectID != "project-1" {
		t.Fatal("Owner metadata did not preserve name/project")
	}
}

func TestCatalogPGMetadataFactsArchiveReservationAndExactResolution(t *testing.T) {
	b := bindingPGFixture(t, nil)
	ctx := context.Background()
	command := bindingPGCommand(b, "catalog-binding", model.ActorSlot1, 1, false)
	bound, _ := bindingPGComplete(t, b, command)
	rename := CatalogCommand{Auth: b.owner.Auth, ClientID: "rename-room", ExpectedRevision: bound.Revision, Intent: CatalogIntent{Kind: "room.rename", RoomID: "binding-room", Name: "Renamed Native Room"}}
	r, e := b.f.store.CommitCatalog(ctx, rename)
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("rename: %+v %v", r, e)
	}
	change, e := model.DecodeEnterpriseRoomMetadata(r.Fact)
	if e != nil || change.Name != "Renamed Native Room" {
		t.Fatal("rename did not append authoritative metadata fact")
	}
	resolved, e := b.f.store.ResolveCatalog(ctx, rename)
	if e != nil || resolved.Outcome != OutcomeReplayed || resolved.Fact.ID != r.Fact.ID {
		t.Fatal("Resolve replaced original metadata fact")
	}
	archive := catalogPGCommit(t, b.f, b.owner.Auth, "archive-room", r.Revision, CatalogIntent{Kind: "room.archive", RoomID: "binding-room"})
	var retired bool
	var reservations int
	if e = b.f.admin.QueryRow(ctx, `SELECT retired_at IS NOT NULL,(SELECT count(*) FROM bindings WHERE agent_id=$1 AND retired_at IS NULL) FROM bindings WHERE binding_id=$2`, b.agentID, command.BindingID).Scan(&retired, &reservations); e != nil || retired || reservations != 1 {
		t.Fatal("archive retired an Agent reservation")
	}
	if p := catalogPGRead(t, b.f, b.owner.Auth, CatalogQuery{Resource: "rooms"}); len(p.Rooms) != 0 {
		t.Fatal("archived Room appeared in active list")
	}
	if p := catalogPGRead(t, b.f, b.owner.Auth, CatalogQuery{Resource: "rooms", State: "archived"}); len(p.Rooms) != 1 || !p.Rooms[0].Archived {
		t.Fatal("archive was not discoverable to its live member")
	}
	catalogPGCommit(t, b.f, b.owner.Auth, "restore-room", archive.Revision, CatalogIntent{Kind: "room.restore", RoomID: "binding-room"})
	snap, e := b.f.store.ReadonlySnapshot(ctx, b.owner.Auth, "binding-room")
	if e != nil {
		t.Fatal(e)
	}
	projection, e := relay.ProjectEnterpriseSnapshot(toRelaySnapshot(snap))
	if e != nil || projection.Bindings[model.ActorSlot1].BindID != command.BindingID {
		t.Fatalf("metadata broke existing Binding replay: %v", e)
	}
	if resolved, e = b.f.store.ResolveCatalog(ctx, rename); e != nil || resolved.Outcome != OutcomeReplayed || resolved.Fact.ID != r.Fact.ID {
		t.Fatal("later metadata invalidated original accepted identity")
	}
}

func TestCatalogPGPaginationSessionRevocationAndNoForeignMetadata(t *testing.T) {
	g := newGovernanceFixture(t, false)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		intent, e := model.PrepareEnterpriseRoomIntent(fmt.Sprintf("Room %d", i), model.Collaboration{})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = g.f.store.CommitRoomCreate(ctx, RoomCreateCommand{Auth: g.owner.Auth, ClientID: fmt.Sprintf("create-page-%d", i), RoomID: fmt.Sprintf("page-%d", i), Intent: intent}); e != nil {
			t.Fatal(e)
		}
	}
	first := catalogPGRead(t, g.f, g.owner.Auth, CatalogQuery{Resource: "rooms", Limit: 2})
	if len(first.Rooms) != 2 || first.Next == "" {
		t.Fatal("Room keyset page not bounded")
	}
	second := catalogPGRead(t, g.f, g.owner.Auth, CatalogQuery{Resource: "rooms", Limit: 2, After: first.Next})
	if len(second.Rooms) != 2 || second.Rooms[0].RoomID <= first.Next || second.Next != "" {
		t.Fatal("Room page repeated or omitted cursor boundary")
	}
	if p := catalogPGRead(t, g.f, g.admin.Auth, CatalogQuery{Resource: "rooms"}); len(p.Rooms) != 0 {
		t.Fatal("admin listed non-member Rooms")
	}
	otherLogin, otherSession := identityPGLogin(t, g.f, "member-one", "catalog-other-browser")
	catalogPGCommit(t, g.f, g.owner.Auth, "revoke-other-session", 0, CatalogIntent{Kind: "session.revoke", SessionID: otherSession.Auth.SessionID})
	if _, e := g.f.store.AuthenticateSession(ctx, otherLogin.Session.TokenHash); e == nil {
		t.Fatal("revoked browser session remained valid")
	}
	if _, e := g.f.store.CommitCatalog(ctx, CatalogCommand{Auth: g.owner.Auth, ClientID: "revoke-foreign", Intent: CatalogIntent{Kind: "session.revoke", SessionID: g.admin.Auth.SessionID}}); e == nil {
		t.Fatal("user revoked another user's browser session")
	}
	for _, entry := range catalogPGRead(t, g.f, g.owner.Auth, CatalogQuery{Resource: "sessions"}).Sessions {
		if entry.SessionID == g.admin.Auth.SessionID {
			t.Fatal("foreign session metadata disclosed")
		}
	}
}

func TestCatalogPGAgentRetirementKeepsDeviceAndOtherAgentsUsable(t *testing.T) {
	b := bindingPGFixture(t, nil)
	ctx := context.Background()
	command := bindingPGCommand(b, "retire-agent", model.ActorSlot1, 1, false)
	bindingPGComplete(t, b, command)
	source := sha256.Sum256([]byte("another-independent-Agent"))
	registered, e := b.f.store.RegisterAgent(ctx, b.device.Auth, "register-independent-agent", base64.RawURLEncoding.EncodeToString(source[:]), model.RuntimeCodex)
	if e != nil || registered.Control == nil {
		t.Fatalf("register sibling Agent: %v", e)
	}
	catalogPGCommit(t, b.f, b.member.Auth, "retire-own-agent", 0, CatalogIntent{Kind: "agent.retire", AgentID: b.agentID})
	if _, e := b.f.store.AuthenticateDevice(ctx, b.enrollment.Credential.AccessHash, b.enrollment.Origin); e != nil {
		t.Fatalf("Agent retirement revoked its entire Device: %v", e)
	}
	if _, e := b.f.store.AuthenticateBinding(ctx, command.Grant.TokenHash, b.enrollment.Origin); e == nil {
		t.Fatal("retired Agent retained narrow Binding grant")
	}
	var retired bool
	if e := b.f.admin.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM agents WHERE agent_id=$1`, b.agentID).Scan(&retired); e != nil || !retired {
		t.Fatal("Agent identity did not retire")
	}
	if p := catalogPGRead(t, b.f, b.owner.Auth, CatalogQuery{Resource: "agents"}); len(p.Agents) != 0 {
		t.Fatal("Room ownership disclosed another User's Agent catalog")
	}
	b.agentID = registered.Control.AgentID
	other := bindingPGCommand(b, "other-agent-survived", model.ActorSlot2, 1, false)
	if accepted, _ := bindingPGComplete(t, b, other); accepted.BindingAuthority.Binding.AgentID != registered.Control.AgentID {
		t.Fatal("retiring one Agent prevented another Agent from binding")
	}
}

func TestCatalogPGAuditFailureRollsBackRoomFactIndexAndReceipt(t *testing.T) {
	g := newGovernanceFixture(t, false)
	ctx := context.Background()
	if _, e := g.f.admin.Exec(ctx, `CREATE FUNCTION catalog_audit_fail() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN RAISE EXCEPTION 'injected catalog audit failure'; END $body$`); e != nil {
		t.Fatal(e)
	}
	if _, e := g.f.admin.Exec(ctx, `CREATE TRIGGER catalog_audit_fail BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION catalog_audit_fail()`); e != nil {
		t.Fatal(e)
	}
	cmd := CatalogCommand{Auth: g.owner.Auth, ClientID: "audit-failed-rename", ExpectedRevision: 2, Intent: CatalogIntent{Kind: "room.rename", RoomID: g.intent.RoomID, Name: "Must roll back"}}
	r, e := g.f.store.CommitCatalog(ctx, cmd)
	if e == nil || r.Outcome != OutcomeNotCommitted {
		t.Fatalf("audit failure outcome: %+v %v", r, e)
	}
	var name string
	var seq, revision, receipts int
	if e = g.f.admin.QueryRow(ctx, `SELECT display_name,current_seq,revision,(SELECT count(*) FROM catalog_operations) FROM rooms WHERE room_id=$1`, g.intent.RoomID).Scan(&name, &seq, &revision, &receipts); e != nil || name == "Must roll back" || seq != 2 || revision != 2 || receipts != 0 {
		t.Fatal("failed audit left partial catalog state")
	}
	if r, e = g.f.store.ResolveCatalog(ctx, cmd); e != nil || r.Outcome != OutcomeNotCommitted {
		t.Fatal("failed catalog mutation was recoverable as accepted")
	}
}

func TestCatalogPGArchiveInvalidatesApprovedRequestsWithoutActivatingPendingOnRestore(t *testing.T) {
	b := bindingPGFixture(t, nil)
	ctx := context.Background()
	approved := bindingPGCommand(b, "approved-before-archive", model.ActorSlot1, 1, false)
	completion := bindingPGApprove(t, b, approved)
	pending := bindingPGCommand(b, "pending-before-archive", model.ActorSlot1, 1, false)
	if _, e := b.f.store.CommitBindingRequest(ctx, pending); e != nil {
		t.Fatal(e)
	}
	archived := catalogPGCommit(t, b.f, b.owner.Auth, "invalidate-on-archive", 2, CatalogIntent{Kind: "room.archive", RoomID: "binding-room"})
	var oldState, pendingState string
	if e := b.f.admin.QueryRow(ctx, `SELECT (SELECT status FROM binding_requests WHERE request_id=$1),(SELECT status FROM binding_requests WHERE request_id=$2)`, approved.RequestID, pending.RequestID).Scan(&oldState, &pendingState); e != nil || oldState != "denied" || pendingState != "pending" {
		t.Fatalf("archive approval policy: %q %q %v", oldState, pendingState, e)
	}
	catalogPGCommit(t, b.f, b.owner.Auth, "restore-without-approval", archived.Revision, CatalogIntent{Kind: "room.restore", RoomID: "binding-room"})
	if _, e := b.f.store.CompleteBindingRequest(ctx, completion); e == nil {
		t.Fatal("restore resurrected the old Owner approval")
	}
	if _, e := b.f.store.CompleteBindingRequest(ctx, BindingCompletionCommand{Auth: b.device.Auth, RoomID: pending.RoomID, RequestID: pending.RequestID, ClientID: "unapproved-complete", ProofHash: pending.ProofHash}); e == nil {
		t.Fatal("restoring a pending request implicitly approved it")
	}
	if _, e := b.f.store.DecideBindingRequest(ctx, BindingDecisionCommand{Auth: b.owner.Auth, ClientID: "new-explicit-approval", RoomID: pending.RoomID, RequestID: pending.RequestID, Approve: true}); e != nil {
		t.Fatal(e)
	}
	if r, e := b.f.store.CompleteBindingRequest(ctx, BindingCompletionCommand{Auth: b.device.Auth, RoomID: pending.RoomID, RequestID: pending.RequestID, ClientID: "new-approved-complete", ProofHash: pending.ProofHash}); e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("new explicit Owner approval was unusable: %+v %v", r, e)
	}
}

func TestCatalogPGArchiveRechecksExpandedApprovalSetAfterOwnershipGuard(t *testing.T) {
	b := bindingPGFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pending := bindingPGCommand(b, "approved-during-discovery", model.ActorSlot1, 1, false)
	if _, e := b.f.store.CommitBindingRequest(ctx, pending); e != nil {
		t.Fatal(e)
	}
	blocker := schemaConn(t, b.f.dsn, b.f.schema)
	tx, e := blocker.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT epoch FROM ownership WHERE id FOR UPDATE`); e != nil {
		t.Fatal(e)
	}
	type result struct {
		value Result
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := b.f.store.CommitCatalog(ctx, CatalogCommand{Auth: b.owner.Auth, ClientID: "archive-expanded-set", ExpectedRevision: 2, Intent: CatalogIntent{Kind: "room.archive", RoomID: "binding-room"}})
		done <- result{value, err}
	}()
	waitForControlLockWait(t, b.f.admin, b.f.store.work.PgConn().PID(), blocker.PgConn().PID())
	// A controlled competing DB writer expands the approved set after the
	// Store's side-effect-free discovery but before its ownership lock.
	if _, e = tx.Exec(ctx, `UPDATE binding_requests SET status='approved',approved_by=$2,decided_at=clock_timestamp() WHERE request_id=$1`, pending.RequestID, b.owner.Auth.UserID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	got := <-done
	if got.err != nil || got.value.Outcome != OutcomeCommitted {
		t.Fatalf("expanded discovery: %+v %v", got.value, got.err)
	}
	var state string
	if e = b.f.admin.QueryRow(ctx, `SELECT status FROM binding_requests WHERE request_id=$1`, pending.RequestID).Scan(&state); e != nil || state != "denied" {
		t.Fatal("approval added during discovery escaped archive invalidation")
	}
}
