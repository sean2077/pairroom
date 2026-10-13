package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

type governanceFixture struct {
	f                    *fixture
	admin, owner, target BrowserSession
	ownerLogin           IdentityLoginCommand
	intent               GovernanceIntent
}

func newGovernanceFixture(t *testing.T, disabled bool) governanceFixture {
	t.Helper()
	f := identityPGFixture(t, nil)
	_, admin := identityPGLogin(t, f, "operator-selected", "governance-admin")
	ownerLogin, owner := identityPGLogin(t, f, "member-one", "governance-owner")
	_, target := identityPGLogin(t, f, "member-two", "governance-target")
	create := roomCreateCommand(t)
	create.Auth, create.RoomID = owner.Auth, "governance-room"
	if _, err := f.store.CommitRoomCreate(context.Background(), create); err != nil {
		t.Fatal(err)
	}
	member := RoomMembershipCommand{Auth: owner.Auth, RoomID: create.RoomID, ClientID: "admit-governance-target", ExpectedRevision: 1, Intent: RoomMembershipIntent{TargetUserID: target.Auth.UserID, Action: MembershipAdmit, Role: RoomMember}}
	if _, err := f.store.CommitRoomMembership(context.Background(), member); err != nil {
		t.Fatal(err)
	}
	g := governanceFixture{f: f, admin: admin, owner: owner, target: target, ownerLogin: ownerLogin, intent: GovernanceIntent{RoomID: create.RoomID, TargetUserID: target.Auth.UserID, ExpectedOwnerUserID: owner.Auth.UserID, ExpectedRevision: 2, Reason: "Recover disabled Owner after verified operator request"}}
	if disabled {
		if _, err := f.store.SetUserEnabled(context.Background(), admin.Auth, "disable-owner", owner.Auth.UserID, false); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func prepareGovernance(t *testing.T, g governanceFixture, id string) (GovernanceAction, enterpriseauth.Completion) {
	t.Helper()
	action, err := g.f.store.PrepareGovernanceReauthentication(context.Background(), g.admin.Auth, id, g.intent)
	if err != nil || action.Ready || action.Intent != g.intent {
		t.Fatalf("prepare governance: %+v %v", action, err)
	}
	identity := identityPGSubject("operator-selected")
	identity.AuthTime = time.Now().UTC().Truncate(time.Second)
	return action, enterpriseauth.Completion{Identity: identity, Purpose: enterpriseauth.PurposeReauthenticate, FreshReauthentication: true, CorrelationID: action.ID, ReturnPath: "/enterprise/"}
}

func readyGovernance(t *testing.T, g governanceFixture, id string) GovernanceAction {
	t.Helper()
	action, completion := prepareGovernance(t, g, id)
	ready, err := g.f.store.FinishGovernanceReauthentication(context.Background(), g.admin.Auth, completion)
	if err != nil || !ready.Ready || ready.ID != action.ID || !ready.ExpiresAt.Equal(action.ExpiresAt) {
		t.Fatalf("finish governance: %+v %v", ready, err)
	}
	return ready
}

func governanceTransferCommand(g governanceFixture, action GovernanceAction, id string) OwnershipTransferCommand {
	return OwnershipTransferCommand{Auth: g.admin.Auth, ClientID: id, Intent: g.intent, GovernanceActionID: action.ID}
}

func assertGovernanceDenied(t *testing.T, err error) {
	t.Helper()
	var kernel *KernelError
	if !errors.As(err, &kernel) || kernel.Outcome != OutcomeRefused {
		t.Fatalf("wanted definite governance refusal: %v", err)
	}
}

func TestGovernanceDisabledOwnerTransferNeverGrantsAdministratorContent(t *testing.T) {
	g := newGovernanceFixture(t, true)
	ctx := context.Background()
	for _, auth := range []AuthContext{g.admin.Auth, g.owner.Auth} {
		if snapshot, err := g.f.store.ReadonlySnapshot(ctx, auth, g.intent.RoomID); err == nil || len(snapshot.Events) != 0 {
			t.Fatal("disabled Owner or unrelated admin read Room content")
		}
	}
	action := readyGovernance(t, g, "prepare-transfer")
	command := governanceTransferCommand(g, action, "transfer")
	result, err := g.f.store.TransferRoomOwnership(ctx, command)
	if err != nil || result.Outcome != OutcomeCommitted || result.Governance == nil || result.Governance.OwnerUserID != g.target.Auth.UserID || result.Seq != 3 || result.Revision != 3 {
		t.Fatalf("governance transfer: %+v %v", result, err)
	}
	change, err := model.DecodeEnterpriseOwnershipChange(result.Fact)
	if err != nil || !change.Governance || change.FromUserID != g.owner.Auth.UserID || change.ToUserID != g.target.Auth.UserID {
		t.Fatalf("ownership history: %+v %v", change, err)
	}
	requireMembership(t, g.f, g.intent.RoomID, g.owner.Auth.UserID, "member", false)
	requireMembership(t, g.f, g.intent.RoomID, g.target.Auth.UserID, "owner", false)
	var consumed bool
	var audits int
	if err := g.f.admin.QueryRow(ctx, `SELECT consumed_at IS NOT NULL,(SELECT count(*) FROM audit WHERE kind='governance.transfer') FROM governance_actions WHERE action_id=$1`, action.ID).Scan(&consumed, &audits); err != nil || !consumed || audits != 1 {
		t.Fatalf("proof/audit not committed with transfer: %v", err)
	}
	if snapshot, err := g.f.store.ReadonlySnapshot(ctx, g.admin.Auth, g.intent.RoomID); err == nil || len(snapshot.Events) != 0 {
		t.Fatal("governance granted admin content membership")
	}
	read, err := g.f.store.ReadonlySnapshot(ctx, g.target.Auth, g.intent.RoomID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay.ProjectEnterpriseSnapshot(toRelaySnapshot(read)); err != nil {
		t.Fatalf("valid ownership history cannot be projected: %v", err)
	}
	result, err = g.f.store.TransferRoomOwnership(ctx, command)
	if err != nil || result.Outcome != OutcomeReplayed || result.Revision != 3 {
		t.Fatalf("original transfer not recoverable: %+v %v", result, err)
	}
	command.ClientID = "attempt-second-use"
	if _, err := g.f.store.TransferRoomOwnership(ctx, command); err == nil {
		t.Fatal("consumed proof transferred twice")
	}
	if _, err := g.f.store.SetUserEnabled(ctx, g.admin.Auth, "reenable-transferred-owner", g.owner.Auth.UserID, true); err != nil {
		t.Fatal(err)
	}
	assertSessionRefused(t, g.f, g.ownerLogin.Session.TokenHash)
	_, reenabled := identityPGLogin(t, g.f, "member-one", "reenabled-transferred")
	member := RoomMembershipCommand{Auth: reenabled.Auth, RoomID: g.intent.RoomID, ClientID: "reclaim-owner", ExpectedRevision: 3, Intent: RoomMembershipIntent{TargetUserID: g.target.Auth.UserID, Action: MembershipRemove}}
	if _, err := g.f.store.CommitRoomMembership(ctx, member); err == nil {
		t.Fatal("reenabling reclaimed transferred Owner authority")
	}
}

func TestGovernanceMissingOwnerMembershipDoesNotAuthorizeSeizure(t *testing.T) {
	g := newGovernanceFixture(t, false)
	ctx := context.Background()
	if _, err := g.f.admin.Exec(ctx, `UPDATE room_members SET disabled=true WHERE room_id=$1 AND user_id=$2`, g.intent.RoomID, g.owner.Auth.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := g.f.store.PrepareGovernanceReauthentication(ctx, g.admin.Auth, "enabled-owner-seizure", g.intent); err == nil {
		t.Fatal("missing live owner membership authorized seizing enabled Owner")
	} else {
		assertGovernanceDenied(t, err)
	}
	var actions int
	if err := g.f.admin.QueryRow(ctx, `SELECT count(*) FROM governance_actions`).Scan(&actions); err != nil || actions != 0 {
		t.Fatal("refused seizure created proof state")
	}
}

func TestGovernanceReenableRestoresOnlyUntransferredOwnerAuthority(t *testing.T) {
	g := newGovernanceFixture(t, true)
	ctx := context.Background()
	action := readyGovernance(t, g, "prepared-before-reenable")
	if _, err := g.f.store.SetUserEnabled(ctx, g.admin.Auth, "reenable-untransferred-owner", g.owner.Auth.UserID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := g.f.store.TransferRoomOwnership(ctx, governanceTransferCommand(g, action, "stale-disabled-owner-proof")); err == nil {
		t.Fatal("proof survived Owner being reenabled")
	}
	assertSessionRefused(t, g.f, g.ownerLogin.Session.TokenHash)
	_, reenabled := identityPGLogin(t, g.f, "member-one", "reenabled-untransferred")
	member := RoomMembershipCommand{Auth: reenabled.Auth, RoomID: g.intent.RoomID, ClientID: "still-owner", ExpectedRevision: 2, Intent: RoomMembershipIntent{TargetUserID: g.target.Auth.UserID, Action: MembershipChangeRole, Role: RoomViewer}}
	if _, err := g.f.store.CommitRoomMembership(ctx, member); err != nil {
		t.Fatalf("untransferred Owner authority was not restored: %v", err)
	}
}

func TestGovernanceStepUpRejectsForgedPurposeIdentityFreshnessAndSession(t *testing.T) {
	g := newGovernanceFixture(t, true)
	ctx := context.Background()
	_, otherSession := identityPGLogin(t, g.f, "operator-selected", "different-admin-session")
	for _, tc := range []struct {
		name   string
		mutate func(*enterpriseauth.Completion, *AuthContext)
	}{
		{"login-is-not-stepup", func(c *enterpriseauth.Completion, _ *AuthContext) { c.Purpose = enterpriseauth.PurposeLogin }},
		{"unverified-freshness", func(c *enterpriseauth.Completion, _ *AuthContext) { c.FreshReauthentication = false }},
		{"old-auth-time", func(c *enterpriseauth.Completion, _ *AuthContext) { c.Identity.AuthTime = time.Now().Add(-time.Hour) }},
		{"wrong-subject", func(c *enterpriseauth.Completion, _ *AuthContext) { c.Identity.Subject = "member-two" }},
		{"wrong-issuer", func(c *enterpriseauth.Completion, _ *AuthContext) { c.Identity.Issuer = "https://other.example.test" }},
		{"same-user-new-session", func(_ *enterpriseauth.Completion, auth *AuthContext) { *auth = otherSession.Auth }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			action, completion := prepareGovernance(t, g, tc.name)
			auth := g.admin.Auth
			tc.mutate(&completion, &auth)
			if got, err := g.f.store.FinishGovernanceReauthentication(ctx, auth, completion); err == nil || got != (GovernanceAction{}) {
				t.Fatalf("forged step-up accepted: %+v %v", got, err)
			}
			var ready bool
			if err := g.f.admin.QueryRow(ctx, `SELECT ready_at IS NOT NULL FROM governance_actions WHERE action_id=$1`, action.ID).Scan(&ready); err != nil || ready {
				t.Fatal("refused step-up left usable proof")
			}
		})
	}
	actions, err := g.f.store.ListGovernanceActions(ctx, otherSession.Auth)
	if err != nil || len(actions) != 0 {
		t.Fatal("another Session discovered pending proofs")
	}
}

func TestGovernanceTransferAuditFailureRollsBackProofAndOwnership(t *testing.T) {
	g := newGovernanceFixture(t, true)
	ctx := context.Background()
	action := readyGovernance(t, g, "audit-proof")
	if _, err := g.f.admin.Exec(ctx, `ALTER TABLE audit ADD CONSTRAINT reject_governance_transfer CHECK(kind <> 'governance.transfer')`); err != nil {
		t.Fatal(err)
	}
	command := governanceTransferCommand(g, action, "audit-transfer")
	result, err := g.f.store.TransferRoomOwnership(ctx, command)
	if kerr := kernelErr(t, result, err); kerr.Outcome != OutcomeNotCommitted || kerr.Class != ClassConstraintViolation {
		t.Fatalf("audit failure not definite rollback: %+v", result)
	}
	var owner string
	var consumed bool
	var seq, revision, operations int64
	if err := g.f.admin.QueryRow(ctx, `SELECT owner_user_id,current_seq,revision,(SELECT consumed_at IS NOT NULL FROM governance_actions WHERE action_id=$2),(SELECT count(*) FROM governance_operations WHERE op_kind='transfer') FROM rooms WHERE room_id=$1`, g.intent.RoomID, action.ID).Scan(&owner, &seq, &revision, &consumed, &operations); err != nil {
		t.Fatal(err)
	}
	if owner != g.owner.Auth.UserID || seq != 2 || revision != 2 || consumed || operations != 0 {
		t.Fatal("failed audit left partial ownership, proof or receipt")
	}
	requireMembership(t, g.f, g.intent.RoomID, g.owner.Auth.UserID, "owner", false)
	requireMembership(t, g.f, g.intent.RoomID, g.target.Auth.UserID, "member", false)
	if result, err := g.f.store.ResolveRoomOwnership(ctx, command); err != nil || result.Outcome != OutcomeNotCommitted || result.Fact.ID != "" || result.Governance != nil {
		t.Fatal("rolled-back transfer was recoverable")
	}
	if _, err := g.f.admin.Exec(ctx, `ALTER TABLE audit DROP CONSTRAINT reject_governance_transfer`); err != nil {
		t.Fatal(err)
	}
	if _, err := g.f.store.TransferRoomOwnership(ctx, command); err != nil {
		t.Fatalf("explicit retry after definite rollback: %v", err)
	}
}

func TestGovernanceTwoTransfersHaveOneWinnerUnderDatabaseGuard(t *testing.T) {
	g := newGovernanceFixture(t, true)
	ctx := context.Background()
	actions := []GovernanceAction{readyGovernance(t, g, "race-one"), readyGovernance(t, g, "race-two")}
	work := schemaConn(t, g.f.dsn, g.f.schema)
	if err := configureSession(ctx, work, g.f.schema); err != nil {
		t.Fatal(err)
	}
	other := &Store{cfg: g.f.cfg, lease: g.f.store.lease, work: work, sem: make(chan struct{}, 1)}
	blocker := schemaConn(t, g.f.dsn, g.f.schema)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT epoch FROM ownership WHERE id FOR SHARE`); err != nil {
		t.Fatal(err)
	}
	var results [2]Result
	var errs [2]error
	var wg sync.WaitGroup
	for i, store := range []*Store{g.f.store, other} {
		wg.Add(1)
		go func(i int, store *Store) {
			defer wg.Done()
			results[i], errs[i] = store.TransferRoomOwnership(ctx, governanceTransferCommand(g, actions[i], fmt.Sprintf("race-transfer-%d", i)))
		}(i, store)
	}
	defer func() { _ = tx.Rollback(ctx); wg.Wait() }()
	waitIdentityControlGuardChain(t, g.f, []uint32{g.f.store.work.PgConn().PID(), other.work.PgConn().PID()}, blocker.PgConn().PID())
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	committed := 0
	for i := range results {
		if errs[i] == nil && results[i].Outcome == OutcomeCommitted {
			committed++
		} else {
			assertGovernanceDenied(t, errs[i])
		}
	}
	if committed != 1 {
		t.Fatalf("transfer winners: %d", committed)
	}
	var used, facts int
	if err := g.f.admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM governance_actions WHERE consumed_at IS NOT NULL),(SELECT count(*) FROM room_events WHERE kind=$1)`, model.EnterpriseOwnershipKind).Scan(&used, &facts); err != nil || used != 1 || facts != 1 {
		t.Fatal("racing proofs produced partial or duplicate effects")
	}
}

func TestGovernanceNormalOwnerTransferNeedsNoAdministratorOrStepUp(t *testing.T) {
	g := newGovernanceFixture(t, false)
	command := OwnershipTransferCommand{Auth: g.owner.Auth, ClientID: "normal-owner-transfer", Intent: g.intent}
	result, err := g.f.store.TransferRoomOwnership(context.Background(), command)
	if err != nil || result.Outcome != OutcomeCommitted {
		t.Fatalf("normal Owner transfer: %+v %v", result, err)
	}
	change, err := model.DecodeEnterpriseOwnershipChange(result.Fact)
	if err != nil || change.Governance {
		t.Fatal("normal transfer invented governance authority")
	}
	encoded, _ := json.Marshal(result.Governance)
	for _, private := range []string{g.owner.Auth.SessionID, "token", "csrf", "auth_time"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("governance result contains private credential context")
		}
	}
}
