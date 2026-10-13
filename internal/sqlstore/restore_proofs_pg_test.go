package sqlstore

import (
	"crypto/sha256"
	"encoding/base64"
	"github.com/sean2077/pairroom/internal/enterprisebackup"
	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"github.com/sean2077/pairroom/internal/model"
	"path/filepath"
	"testing"
)

func TestRestorePreservesACLHistoryAndInvalidatesOldProofsWithoutFakeDecisions(t *testing.T) {
	b, objects := backupPGSource(t)
	source := sha256.Sum256([]byte("unbound-old-request-agent"))
	agent, e := b.f.store.RegisterAgent(t.Context(), b.device.Auth, "proofs-agent", base64.RawURLEncoding.EncodeToString(source[:]), model.RuntimeCodex)
	if e != nil {
		t.Fatal(e)
	}
	b.agentID = agent.Control.AgentID
	approved := bindingPGCommand(b, "old-approved", model.ActorSlot2, 1, false)
	bindingPGApprove(t, b, approved)
	pending := bindingPGCommand(b, "old-pending", model.ActorSlot2, 1, false)
	if _, e = b.f.store.CommitBindingRequest(t.Context(), pending); e != nil {
		t.Fatal(e)
	}
	ownerLogin, disabledOwner := identityPGLogin(t, b.f, "member-two", "proofs-disabled-owner")
	intent, _ := model.PrepareEnterpriseRoomIntent("Ownership pending recovery", model.Collaboration{})
	if _, e = b.f.store.CommitRoomCreate(t.Context(), RoomCreateCommand{Auth: disabledOwner.Auth, RoomID: "governance-room", ClientID: "proofs-create", Intent: intent}); e != nil {
		t.Fatal(e)
	}
	if _, e = b.f.store.CommitRoomMembership(t.Context(), RoomMembershipCommand{Auth: disabledOwner.Auth, RoomID: "governance-room", ClientID: "proofs-admit", ExpectedRevision: 1, Intent: RoomMembershipIntent{TargetUserID: b.owner.Auth.UserID, Action: MembershipAdmit, Role: RoomMember}}); e != nil {
		t.Fatal(e)
	}
	if _, e = b.f.store.SetUserEnabled(t.Context(), b.admin.Auth, "proofs-disable-owner", disabledOwner.Auth.UserID, false); e != nil {
		t.Fatal(e)
	}
	g := governanceFixture{f: b.f, admin: b.admin, owner: disabledOwner, target: b.owner, ownerLogin: ownerLogin, intent: GovernanceIntent{RoomID: "governance-room", TargetUserID: b.owner.Auth.UserID, ExpectedOwnerUserID: disabledOwner.Auth.UserID, ExpectedRevision: 2, Reason: "Recover disabled Owner after verified operator request"}}
	ready := readyGovernance(t, g, "old-ready-proof")
	unready, _ := prepareGovernance(t, g, "old-unready-proof")
	catalogPGCommit(t, b.f, b.owner.Auth, "restore-project", 0, CatalogIntent{Kind: "project.create", ProjectID: "backup-project", Name: "Logical project"})
	catalogPGCommit(t, b.f, b.owner.Auth, "restore-project-archive", 1, CatalogIntent{Kind: "project.archive", ProjectID: "backup-project"})
	path := filepath.Join(t.TempDir(), "bundle")
	if _, e = b.f.store.BackupOffline(t.Context(), objects, path, "proofs-backup"); e != nil {
		t.Fatal(e)
	}
	bundle, e := enterprisebackup.Open(t.Context(), path, "dep-test", "org-test")
	if e != nil {
		t.Fatal(e)
	}
	_ = b.f.store.Close()
	target := backupPGPristine(t)
	targetObjects, e := enterpriseobject.Open(filepath.Join(t.TempDir(), "objects"), "dep-test")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = target.store.RestoreOffline(t.Context(), targetObjects, bundle, "proofs-restore"); e != nil {
		t.Fatal(e)
	}
	var pendingStatus, approvedStatus, approver string
	var pendingApprover *string
	var pendingDecided, pendingConsumed, approvedConsumed *string
	if e = target.admin.QueryRow(t.Context(), `SELECT status,approved_by,decided_at::text,consumed_at::text FROM binding_requests WHERE request_id=$1`, pending.RequestID).Scan(&pendingStatus, &pendingApprover, &pendingDecided, &pendingConsumed); e != nil || pendingStatus != "invalidated" || pendingApprover != nil || pendingDecided != nil || pendingConsumed != nil {
		t.Fatal("pending Binding proof gained fabricated approval", e)
	}
	if e = target.admin.QueryRow(t.Context(), `SELECT status,approved_by,consumed_at::text FROM binding_requests WHERE request_id=$1`, approved.RequestID).Scan(&approvedStatus, &approver, &approvedConsumed); e != nil || approvedStatus != "invalidated" || approver != b.owner.Auth.UserID || approvedConsumed != nil {
		t.Fatal("approved Binding receipt provenance changed", e)
	}
	for _, action := range []GovernanceAction{ready, unready} {
		var invalidated, readyState, consumed bool
		if e = target.admin.QueryRow(t.Context(), `SELECT invalidated_at IS NOT NULL,ready_at IS NOT NULL,consumed_at IS NOT NULL FROM governance_actions WHERE action_id=$1`, action.ID).Scan(&invalidated, &readyState, &consumed); e != nil || !invalidated || readyState != action.Ready || consumed {
			t.Fatal("governance invalidation manufactured proof consumption", e)
		}
	}
	for _, name := range []string{"users", "organization_admins", "identity_admissions", "rooms", "room_members", "projects", "project_members", "bindings", "room_slots", "slot_generations", "room_events"} {
		var original, restored string
		q := `SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM `
		if e = b.f.admin.QueryRow(t.Context(), q+name+` t`).Scan(&original); e != nil {
			t.Fatal(e)
		}
		if e = target.admin.QueryRow(t.Context(), q+name+` t`).Scan(&restored); e != nil || original != restored {
			t.Fatalf("restore relabeled %s history/authority: %v", name, e)
		}
	}
	if _, e = target.store.RecoverDeliveries(t.Context()); e != nil {
		t.Fatal(e)
	}
	if _, e = target.store.CompleteRestore(t.Context(), targetObjects, "proofs-restore", bundle.Digest()); e != nil {
		t.Fatal(e)
	}
	_, fresh := identityPGLogin(t, target, "member-one", "proofs-fresh-login")
	catalog, e := target.store.ReadCatalog(t.Context(), fresh.Auth, CatalogQuery{Resource: "rooms", Limit: 32})
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, room := range catalog.Rooms {
		if room.RoomID == "governance-room" {
			found = true
			if !room.OwnershipPendingRecovery || room.OwnerUserID != disabledOwner.Auth.UserID || room.Role != "member" {
				t.Fatal("restored disabled Owner/ACL changed")
			}
		}
	}
	if !found {
		t.Fatal("existing Member lost Room ACL")
	}
}
