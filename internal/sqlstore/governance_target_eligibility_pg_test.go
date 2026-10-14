package sqlstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"
)

func targetGateState(t *testing.T, g governanceFixture, governanceAuditOnly bool) [32]byte {
	t.Helper()
	var state string
	if err := g.f.admin.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'room',(SELECT to_jsonb(r) FROM rooms r WHERE room_id=$1),
 'members',(SELECT jsonb_agg(to_jsonb(m) ORDER BY user_id) FROM room_members m WHERE room_id=$1),
 'actions',(SELECT jsonb_agg(to_jsonb(a) ORDER BY action_id) FROM governance_actions a),
 'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY op_kind,identifier) FROM governance_operations o),
 'facts',(SELECT jsonb_agg(to_jsonb(e) ORDER BY seq) FROM room_events e WHERE room_id=$1),
 'audit',(SELECT jsonb_agg(to_jsonb(a) ORDER BY audit_seq) FROM audit a WHERE NOT $2 OR kind LIKE 'governance.%')
)::text`, g.intent.RoomID, governanceAuditOnly).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256([]byte(state))
}

func targetGateRevision(t *testing.T, g *governanceFixture) {
	t.Helper()
	if err := g.f.admin.QueryRow(t.Context(), `SELECT revision FROM rooms WHERE room_id=$1`, g.intent.RoomID).Scan(&g.intent.ExpectedRevision); err != nil {
		t.Fatal(err)
	}
}

func targetGateMembership(t *testing.T, g *governanceFixture, key string, action MembershipAction, role RoomRole) {
	t.Helper()
	targetGateRevision(t, g)
	_, err := g.f.store.CommitRoomMembership(t.Context(), RoomMembershipCommand{Auth: g.owner.Auth, RoomID: g.intent.RoomID, ClientID: key, ExpectedRevision: g.intent.ExpectedRevision, Intent: RoomMembershipIntent{TargetUserID: g.target.Auth.UserID, Action: action, Role: role}})
	if err != nil {
		t.Fatal("typed target membership transition failed", err)
	}
	targetGateRevision(t, g)
}

func targetGateDeny(t *testing.T, err error) {
	t.Helper()
	var kernel *KernelError
	if !errors.As(err, &kernel) || kernel.Outcome != OutcomeRefused || kernel.Class != ClassAuthorization {
		t.Fatalf("expected live target authorization refusal: %v", err)
	}
}

func targetGateNoAdminContent(t *testing.T, g governanceFixture) {
	t.Helper()
	var memberships int
	if err := g.f.admin.QueryRow(t.Context(), `SELECT count(*) FROM room_members WHERE room_id=$1 AND user_id=$2`, g.intent.RoomID, g.admin.Auth.UserID).Scan(&memberships); err != nil || memberships != 0 {
		t.Fatal("governance silently admitted administrator", err)
	}
	if snap, err := g.f.store.ReadonlySnapshot(t.Context(), g.admin.Auth, g.intent.RoomID); err == nil || len(snap.Events) != 0 {
		t.Fatal("administrator obtained Room contents")
	}
}

func TestGovernanceTargetPrepareRejectsEveryIneligibleTargetWithoutEffects(t *testing.T) {
	for _, kind := range []string{"viewer", "disabled-user", "removed-membership", "no-membership", "missing-user", "no-qualified-member"} {
		t.Run(kind, func(t *testing.T) {
			g := newGovernanceFixture(t, false)
			switch kind {
			case "viewer":
				targetGateMembership(t, &g, "make-viewer", MembershipChangeRole, RoomViewer)
			case "disabled-user":
				if _, err := g.f.store.SetUserEnabled(t.Context(), g.admin.Auth, "disable-target", g.target.Auth.UserID, false); err != nil {
					t.Fatal(err)
				}
			case "removed-membership":
				targetGateMembership(t, &g, "remove-target", MembershipRemove, "")
			case "no-membership":
				g.intent.TargetUserID = g.admin.Auth.UserID
			case "missing-user":
				g.intent.TargetUserID = "user-not-present"
			case "no-qualified-member":
				targetGateMembership(t, &g, "remove-only-member", MembershipRemove, "")
				g.intent.TargetUserID = g.admin.Auth.UserID
			}
			if kind == "no-qualified-member" {
				var eligible int
				if err := g.f.admin.QueryRow(t.Context(), `SELECT count(*) FROM room_members m JOIN users u USING(user_id) WHERE room_id=$1 AND role='member' AND NOT m.disabled AND NOT u.disabled`, g.intent.RoomID).Scan(&eligible); err != nil || eligible != 0 {
					t.Fatal("fixture still has qualified member", err)
				}
			}
			if _, err := g.f.store.SetUserEnabled(t.Context(), g.admin.Auth, "disable-owner-for-prepare", g.owner.Auth.UserID, false); err != nil {
				t.Fatal(err)
			}
			before := targetGateState(t, g, false)
			action, err := g.f.store.PrepareGovernanceReauthentication(t.Context(), g.admin.Auth, "ineligible-prepare", g.intent)
			targetGateDeny(t, err)
			if action != (GovernanceAction{}) || targetGateState(t, g, false) != before {
				t.Fatal("ineligible prepare left authority, proof, result, audit or fact")
			}
			targetGateNoAdminContent(t, g)
		})
	}
}

func TestGovernanceTargetFreshProofRechecksChangedTargetBeforeConsumption(t *testing.T) {
	for _, kind := range []string{"demoted", "removed", "disabled"} {
		t.Run(kind, func(t *testing.T) {
			g := newGovernanceFixture(t, true)
			action := readyGovernance(t, g, "ready-before-target-change")
			original := governanceTransferCommand(g, action, "consume-after-target-change")
			if kind == "disabled" {
				if _, err := g.f.store.SetUserEnabled(t.Context(), g.admin.Auth, "disable-ready-target", g.target.Auth.UserID, false); err != nil {
					t.Fatal(err)
				}
			} else {
				// Owner actions remain forbidden while disabled. The legal typed
				// path re-enables, signs in anew, changes membership and disables
				// again; it never relabels the prepared action's original intent.
				if _, err := g.f.store.SetUserEnabled(t.Context(), g.admin.Auth, "enable-owner-for-target-change", g.owner.Auth.UserID, true); err != nil {
					t.Fatal(err)
				}
				_, fresh := identityPGLogin(t, g.f, "member-one", "owner-target-change")
				g.owner = fresh
				if kind == "demoted" {
					targetGateMembership(t, &g, "demote-ready-target", MembershipChangeRole, RoomViewer)
				} else {
					targetGateMembership(t, &g, "remove-ready-target", MembershipRemove, "")
				}
				if _, err := g.f.store.SetUserEnabled(t.Context(), g.admin.Auth, "disable-owner-after-target-change", g.owner.Auth.UserID, false); err != nil {
					t.Fatal(err)
				}
			}
			before := targetGateState(t, g, false)
			_, err := g.f.store.TransferRoomOwnership(t.Context(), original)
			targetGateDeny(t, err)
			if targetGateState(t, g, false) != before {
				t.Fatal("changed target consumed proof or changed Room/results/audit/facts")
			}
			resolved, err := g.f.store.ResolveRoomOwnership(t.Context(), original)
			if err != nil || resolved.Outcome != OutcomeNotCommitted || targetGateState(t, g, false) != before {
				t.Fatal("rejected target operation gained a result", err)
			}
			targetGateNoAdminContent(t, g)
		})
	}
}

func TestGovernanceTargetDisableWinsRealGuardRaceAgainstPreparedConsume(t *testing.T) {
	g := newGovernanceFixture(t, true)
	action := readyGovernance(t, g, "target-disable-race-proof")
	before := targetGateState(t, g, true)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
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
	defer func() { c, cancel := cleanupCtx(); defer cancel(); _ = tx.Rollback(c) }()
	if _, err = tx.Exec(ctx, `SELECT epoch FROM ownership WHERE id FOR SHARE`); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result Result
		err    error
	}
	disabled, consumed := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		r, e := g.f.store.SetUserEnabled(ctx, g.admin.Auth, "race-disable-target", g.target.Auth.UserID, false)
		disabled <- outcome{r, e}
	}()
	waitIdentityControlGuardChain(t, g.f, []uint32{g.f.store.work.PgConn().PID()}, blocker.PgConn().PID())
	go func() {
		r, e := other.TransferRoomOwnership(ctx, governanceTransferCommand(g, action, "race-consume-target"))
		consumed <- outcome{r, e}
	}()
	waitIdentityControlGuardChain(t, g.f, []uint32{other.work.PgConn().PID()}, g.f.store.work.PgConn().PID())
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	first, second := <-disabled, <-consumed
	if first.err != nil || first.result.Outcome != OutcomeCommitted {
		t.Fatal("typed target disable did not win lawful serialization", first.err)
	}
	targetGateDeny(t, second.err)
	if targetGateState(t, g, true) != before {
		t.Fatal("racing ineligible consume changed governance proof, Room, results or audit")
	}
	var targetDisabled bool
	if err = g.f.admin.QueryRow(ctx, `SELECT disabled FROM users WHERE user_id=$1`, g.target.Auth.UserID).Scan(&targetDisabled); err != nil || !targetDisabled {
		t.Fatal("race did not actually disable target", err)
	}
}

func TestGovernanceTargetEnabledOwnerMissingMembershipNeverAuthorizesRecovery(t *testing.T) {
	for _, damage := range []string{"disabled-membership", "missing-row"} {
		t.Run(damage, func(t *testing.T) {
			g := newGovernanceFixture(t, false)
			// Explicit storage-corruption counterexample: no typed operation
			// permits removing the current Owner's own membership.
			if damage == "missing-row" {
				if _, err := g.f.admin.Exec(t.Context(), `ALTER TABLE room_members DISABLE TRIGGER USER`); err != nil {
					t.Fatal(err)
				}
				if _, err := g.f.admin.Exec(t.Context(), `DELETE FROM room_members WHERE room_id=$1 AND user_id=$2`, g.intent.RoomID, g.owner.Auth.UserID); err != nil {
					t.Fatal(err)
				}
			} else if _, err := g.f.admin.Exec(t.Context(), `UPDATE room_members SET disabled=true WHERE room_id=$1 AND user_id=$2`, g.intent.RoomID, g.owner.Auth.UserID); err != nil {
				t.Fatal(err)
			}
			before := targetGateState(t, g, false)
			_, err := g.f.store.PrepareGovernanceReauthentication(t.Context(), g.admin.Auth, "enabled-owner-not-seizable", g.intent)
			targetGateDeny(t, err)
			if targetGateState(t, g, false) != before {
				t.Fatal("damaged membership justified enabled Owner seizure")
			}
		})
	}
}

func TestGovernanceTargetQualifiedMemberAcceptedResolveSurvivesLaterIneligibility(t *testing.T) {
	g := newGovernanceFixture(t, true)
	action := readyGovernance(t, g, "qualified-live-member")
	cmd := governanceTransferCommand(g, action, "accepted-qualified-transfer")
	accepted, err := g.f.store.TransferRoomOwnership(t.Context(), cmd)
	if err != nil || accepted.Outcome != OutcomeCommitted || accepted.Governance == nil || accepted.Governance.OwnerUserID != g.target.Auth.UserID {
		t.Fatal("qualified Member positive control failed", err)
	}
	targetGateNoAdminContent(t, g)
	if _, err = g.f.store.SetUserEnabled(t.Context(), g.admin.Auth, "enable-original-owner", g.owner.Auth.UserID, true); err != nil {
		t.Fatal(err)
	}
	_, fresh := identityPGLogin(t, g.f, "member-one", "owner-after-qualified-transfer")
	var revision uint64
	if err = g.f.admin.QueryRow(t.Context(), `SELECT revision FROM rooms WHERE room_id=$1`, g.intent.RoomID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err = g.f.store.TransferRoomOwnership(t.Context(), OwnershipTransferCommand{Auth: g.target.Auth, ClientID: "normal-transfer-away", Intent: GovernanceIntent{RoomID: g.intent.RoomID, TargetUserID: fresh.Auth.UserID, ExpectedOwnerUserID: g.target.Auth.UserID, ExpectedRevision: revision}}); err != nil {
		t.Fatal(err)
	}
	g.owner = fresh
	targetGateMembership(t, &g, "demote-original-target", MembershipChangeRole, RoomViewer)
	if _, err = g.f.store.SetUserEnabled(t.Context(), g.admin.Auth, "disable-original-target", g.target.Auth.UserID, false); err != nil {
		t.Fatal(err)
	}
	before := targetGateState(t, g, false)
	for _, resolve := range []bool{true, false} {
		var got Result
		if resolve {
			got, err = g.f.store.ResolveRoomOwnership(t.Context(), cmd)
		} else {
			got, err = g.f.store.TransferRoomOwnership(t.Context(), cmd)
		}
		if err != nil || got.Outcome != OutcomeReplayed || !reflect.DeepEqual(got.Governance, accepted.Governance) || got.Fact.ID != accepted.Fact.ID || targetGateState(t, g, false) != before {
			t.Fatal("accepted original result incorrectly re-evaluated current target/ownership", err)
		}
	}
	targetGateNoAdminContent(t, g)
}
