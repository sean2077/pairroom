package sqlstore

import (
	"crypto/sha256"
	"github.com/sean2077/pairroom/internal/model"
	"regexp"
	"testing"
)

func auditPGApprovedRequests(t *testing.T, b *bindingPGState) {
	t.Helper()
	for _, id := range []string{"audit-approved-one", "audit-approved-two"} {
		bindingPGApprove(t, b, bindingPGCommand(b, id, model.ActorSlot1, 1, false))
	}
	c := devicePGCommand("audit-device-proof")
	c.DeviceID = b.device.DeviceID
	if _, e := b.f.store.BeginDeviceEnrollment(t.Context(), c); e != nil {
		t.Fatal(e)
	}
	code := sha256.Sum256([]byte("private-audit-approval-code"))
	if _, e := b.f.store.DecideDeviceEnrollment(t.Context(), DeviceEnrollmentDecision{Auth: b.member.Auth, ClientID: "audit-device-approval", RequestID: c.RequestID, Approve: true, CodeHash: code}); e != nil {
		t.Fatal(e)
	}
}
func TestMutationAuditCountsActualApprovalInvalidationsAndReplay(t *testing.T) {
	for _, kind := range []string{"user", "device", "member-remove", "member-demote", "agent", "archive", "transfer"} {
		t.Run(kind, func(t *testing.T) {
			b := bindingPGFixture(t, nil)
			auditPGApprovedRequests(t, b)
			id := "audit-operation-" + kind
			var r Result
			var e error
			var target, auditKind string
			var expected int64 = 2
			switch kind {
			case "user":
				target, auditKind, expected = b.member.Auth.UserID, controlUserStatus, 3
				r, e = b.f.store.SetUserEnabled(t.Context(), b.admin.Auth, id, target, false)
			case "device":
				target, auditKind, expected = b.device.DeviceID, ControlDeviceRevoke, 3
				r, e = b.f.store.RevokeDevice(t.Context(), b.member.Auth, id, target)
			case "member-remove", "member-demote":
				target, auditKind = b.member.Auth.UserID, "room.membership."+string(MembershipRemove)
				action := MembershipRemove
				role := RoomRole("")
				if kind == "member-demote" {
					action, role = MembershipChangeRole, RoomViewer
					auditKind = "room.membership." + string(action)
				}
				r, e = b.f.store.CommitRoomMembership(t.Context(), RoomMembershipCommand{Auth: b.owner.Auth, RoomID: "binding-room", ClientID: id, ExpectedRevision: 2, Intent: RoomMembershipIntent{Action: action, TargetUserID: target, Role: role}})
			case "agent":
				target, auditKind = b.agentID, "catalog.agent.retire"
				r, e = b.f.store.CommitCatalog(t.Context(), CatalogCommand{Auth: b.member.Auth, ClientID: id, Intent: CatalogIntent{Kind: "agent.retire", AgentID: target}})
			case "archive":
				target, auditKind = "binding-room", "catalog.room.archive"
				r, e = b.f.store.CommitCatalog(t.Context(), CatalogCommand{Auth: b.owner.Auth, ClientID: id, ExpectedRevision: 2, Intent: CatalogIntent{Kind: "room.archive", RoomID: target}})
			case "transfer":
				target, auditKind = "binding-room", "governance.transfer"
				r, e = b.f.store.TransferRoomOwnership(t.Context(), OwnershipTransferCommand{Auth: b.owner.Auth, ClientID: id, Intent: GovernanceIntent{RoomID: target, TargetUserID: b.member.Auth.UserID, ExpectedOwnerUserID: b.owner.Auth.UserID, ExpectedRevision: 2}})
			}
			if e != nil || r.Outcome != OutcomeCommitted {
				t.Fatalf("%s: %+v %v", kind, r, e)
			}
			// The membership audit's actual fixed kind is owned by its typed producer.
			if kind == "member-remove" || kind == "member-demote" {
				if e = b.f.admin.QueryRow(t.Context(), `SELECT kind FROM audit WHERE target_id=$1 AND invalidated_approval_count IS NOT NULL ORDER BY audit_seq DESC LIMIT 1`, target).Scan(&auditKind); e != nil {
					t.Fatal(e)
				}
			}
			var storedTarget, op string
			var count int64
			if e = b.f.admin.QueryRow(t.Context(), `SELECT target_id,operation_id,invalidated_approval_count FROM audit WHERE kind=$1 AND target_id=$2 ORDER BY audit_seq DESC LIMIT 1`, auditKind, target).Scan(&storedTarget, &op, &count); e != nil || storedTarget != target || count != expected || !regexp.MustCompile(`^operation-[a-f0-9]{64}$`).MatchString(op) {
				t.Fatalf("audit target/count: %s %s %d expected%d: %v", storedTarget, op, count, expected, e)
			}
			var binding, device int
			if e = b.f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM binding_requests WHERE status='approved'),(SELECT count(*) FROM device_enrollments WHERE status='approved')`).Scan(&binding, &device); e != nil || binding != 0 || device != map[bool]int{true: 0, false: 1}[kind == "user" || kind == "device"] {
				t.Fatal("count disagrees with actual invalidation", e)
			}
			if kind == "user" || kind == "device" || kind == "archive" {
				var replay Result
				switch kind {
				case "user":
					replay, e = b.f.store.SetUserEnabled(t.Context(), b.admin.Auth, id, target, false)
				case "device":
					replay, e = b.f.store.RevokeDevice(t.Context(), b.member.Auth, id, target)
				case "archive":
					replay, e = b.f.store.CommitCatalog(t.Context(), CatalogCommand{Auth: b.owner.Auth, ClientID: id, ExpectedRevision: 2, Intent: CatalogIntent{Kind: "room.archive", RoomID: target}})
				}
				if e != nil || replay.Outcome != OutcomeReplayed {
					t.Fatal("accepted receipt not recovered", replay, e)
				}
				var rows int
				var preserved int64
				if e = b.f.admin.QueryRow(t.Context(), `SELECT count(*),min(invalidated_approval_count) FROM audit WHERE operation_id=$1`, op).Scan(&rows, &preserved); e != nil || rows != 1 || preserved != expected {
					t.Fatal("replay rewrote measured audit", rows, preserved, e)
				}
			}
		})
	}
}
func TestMutationAuditFailureRollsBackInvalidationAndPreservesUnrecordedHistory(t *testing.T) {
	b := bindingPGFixture(t, nil)
	auditPGApprovedRequests(t, b)
	mustExec(t, b.f.admin, `INSERT INTO audit(deployment_id,user_id,kind,outcome) SELECT 'dep-test',user_id,'historical.unrecorded','accepted' FROM users ORDER BY user_id LIMIT 1`)
	var target, operation *string
	var count *int64
	if e := b.f.admin.QueryRow(t.Context(), `SELECT target_id,operation_id,invalidated_approval_count FROM audit WHERE kind='historical.unrecorded'`).Scan(&target, &operation, &count); e != nil || target != nil || operation != nil || count != nil {
		t.Fatal("historical NULL count invented", e)
	}
	mustExec(t, b.f.admin, `CREATE FUNCTION reject_counted_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='user.status' AND NEW.invalidated_approval_count IS NOT NULL THEN RAISE EXCEPTION 'injected counted audit failure'; END IF; RETURN NEW; END $$`)
	mustExec(t, b.f.admin, `CREATE TRIGGER reject_counted_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION reject_counted_audit()`)
	r, e := b.f.store.SetUserEnabled(t.Context(), b.admin.Auth, "counted-audit-rollback", b.member.Auth.UserID, false)
	if e == nil || r.Outcome != OutcomeNotCommitted {
		t.Fatal(r, e)
	}
	var disabled bool
	var approved, records int
	if e = b.f.admin.QueryRow(t.Context(), `SELECT (SELECT disabled FROM users WHERE user_id=$1),(SELECT count(*) FROM binding_requests WHERE status='approved')+(SELECT count(*) FROM device_enrollments WHERE status='approved'),(SELECT count(*) FROM audit WHERE invalidated_approval_count IS NOT NULL)`, b.member.Auth.UserID).Scan(&disabled, &approved, &records); e != nil || disabled || approved != 3 || records != 0 {
		t.Fatal("audit failure left partial authority/count", disabled, approved, records, e)
	}
	if _, e = b.f.admin.Exec(t.Context(), `UPDATE audit SET invalidated_approval_count=-1 WHERE kind='historical.unrecorded'`); e == nil {
		t.Fatal("negative approval count accepted")
	}
}

func TestMutationAuditRetirementUsesParentCorrelationAndMeasuredZero(t *testing.T) {
	b := bindingPGFixture(t, nil)
	request := bindingPGCommand(b, "audit-active-retirement", model.ActorSlot1, 1, false)
	bindingPGComplete(t, b, request)
	if _, e := b.f.store.SetUserEnabled(t.Context(), b.admin.Auth, "retirement-parent-op", b.member.Auth.UserID, false); e != nil {
		t.Fatal(e)
	}
	var primary, retirement string
	var measured int64
	var childCount *int64
	if e := b.f.admin.QueryRow(t.Context(), `SELECT operation_id,invalidated_approval_count FROM audit WHERE kind='user.status' AND target_id=$1 ORDER BY audit_seq DESC LIMIT 1`, b.member.Auth.UserID).Scan(&primary, &measured); e != nil || measured != 0 {
		t.Fatal("actual zero was not measured", e)
	}
	if e := b.f.admin.QueryRow(t.Context(), `SELECT operation_id,invalidated_approval_count FROM audit WHERE kind='binding.retire' AND target_id=$1`, request.BindingID).Scan(&retirement, &childCount); e != nil || retirement != primary || childCount != nil {
		t.Fatal("retirement lost parent correlation or invented count", e)
	}
}

func TestMutationAuditAlphaShapeRefusesMissingOrDefaultedHistoryFields(t *testing.T) {
	for _, alter := range []string{`ALTER TABLE audit DROP COLUMN operation_id`, `ALTER TABLE audit ALTER COLUMN invalidated_approval_count SET DEFAULT 0`} {
		t.Run(alter, func(t *testing.T) {
			f := newRawFixture(t)
			mustExec(t, f.admin, alter)
			if s, e := Open(t.Context(), f.cfg); e == nil {
				_ = s.Close()
				t.Fatal("unsupported audit shape admitted")
			}
			var owners int
			if e := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM ownership`).Scan(&owners); e != nil || owners != 0 {
				t.Fatal("shape refusal acquired ownership", e)
			}
		})
	}
}
