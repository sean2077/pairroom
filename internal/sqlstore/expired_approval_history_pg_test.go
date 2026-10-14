package sqlstore

import (
	"crypto/sha256"
	"github.com/sean2077/pairroom/internal/model"
	"testing"
)

func TestExpiredApprovedHistoryDoesNotBlockAuthorityWriters(t *testing.T) {
	for _, kind := range []string{"user", "device", "member", "agent", "archive", "transfer"} {
		t.Run(kind, func(t *testing.T) {
			b := bindingPGFixture(t, nil)
			binding := bindingPGCommand(b, "one-live-approval", model.ActorSlot1, 1, false)
			bindingPGApprove(t, b, binding)
			device := devicePGCommand("one-live-device-approval")
			device.DeviceID = b.device.DeviceID
			if _, e := b.f.store.BeginDeviceEnrollment(t.Context(), device); e != nil {
				t.Fatal(e)
			}
			code := sha256.Sum256([]byte("expiry-live-code"))
			if _, e := b.f.store.DecideDeviceEnrollment(t.Context(), DeviceEnrollmentDecision{Auth: b.member.Auth, ClientID: "approve-live-device", RequestID: device.RequestID, Approve: true, CodeHash: code}); e != nil {
				t.Fatal(e)
			}
			// History is inserted as immutable historical rows, never by rewriting
			// expiry or deleting history to satisfy the transaction boundary.
			for _, query := range []string{
				`INSERT INTO binding_requests SELECT copy.* FROM binding_requests source CROSS JOIN generate_series(1,10001) i CROSS JOIN LATERAL jsonb_populate_record(NULL::binding_requests,to_jsonb(source)||jsonb_build_object('request_id','expired-binding-'||i,'binding_id','expired-binding-id-'||i,'grant_id','expired-binding-grant-'||i,'token_hash','\x'||md5('expired-binding-token-'||i)||md5('expired-binding-token-'||i),'requested_at',now()-interval '1 hour','expires_at',now()-interval '50 minutes','decided_at',now()-interval '59 minutes')) copy WHERE source.request_id='binding-request-one-live-approval'`,
				`INSERT INTO device_enrollments SELECT copy.* FROM device_enrollments source CROSS JOIN generate_series(1,10001) i CROSS JOIN LATERAL jsonb_populate_record(NULL::device_enrollments,to_jsonb(source)||jsonb_build_object('request_id','expired-device-'||i,'grant_id','expired-device-grant-'||i,'access_hash','\x'||md5('expired-access-'||i)||md5('expired-access-'||i),'refresh_hash','\x'||md5('expired-refresh-'||i)||md5('expired-refresh-'||i),'created_at',now()-interval '1 hour','expires_at',now()-interval '50 minutes')) copy WHERE source.request_id='request-one-live-device-approval'`,
			} {
				mustExec(t, b.f.admin, query)
			}
			// Both completion paths independently reject immutable expired approvals.
			if _, e := b.f.store.CompleteBindingRequest(t.Context(), BindingCompletionCommand{Auth: b.device.Auth, RoomID: "binding-room", RequestID: "expired-binding-1", ClientID: "expired-binding-completion", ProofHash: binding.ProofHash}); e == nil {
				t.Fatal("expired Binding approval remained usable")
			}
			if _, e := b.f.store.CompleteDeviceEnrollment(t.Context(), DeviceEnrollmentCompletion{RequestID: "expired-device-1", ClientID: "expired-device-completion", Origin: device.Origin, ProofHash: device.ProofHash, CodeHash: code}); e == nil {
				t.Fatal("expired Device approval remained usable")
			}
			var r Result
			var e error
			var auditKind, target string
			expected := int64(1)
			switch kind {
			case "user":
				auditKind, target, expected = controlUserStatus, b.member.Auth.UserID, 2
				r, e = b.f.store.SetUserEnabled(t.Context(), b.admin.Auth, "expired-history-user", target, false)
			case "device":
				auditKind, target, expected = ControlDeviceRevoke, b.device.DeviceID, 2
				r, e = b.f.store.RevokeDevice(t.Context(), b.member.Auth, "expired-history-device", target)
			case "member":
				target = b.member.Auth.UserID
				r, e = b.f.store.CommitRoomMembership(t.Context(), RoomMembershipCommand{Auth: b.owner.Auth, RoomID: "binding-room", ClientID: "expired-history-member", ExpectedRevision: 2, Intent: RoomMembershipIntent{Action: MembershipChangeRole, TargetUserID: target, Role: RoomViewer}})
			case "agent":
				auditKind, target = "catalog.agent.retire", b.agentID
				r, e = b.f.store.CommitCatalog(t.Context(), CatalogCommand{Auth: b.member.Auth, ClientID: "expired-history-agent", Intent: CatalogIntent{Kind: "agent.retire", AgentID: target}})
			case "archive":
				auditKind, target = "catalog.room.archive", "binding-room"
				r, e = b.f.store.CommitCatalog(t.Context(), CatalogCommand{Auth: b.owner.Auth, ClientID: "expired-history-archive", ExpectedRevision: 2, Intent: CatalogIntent{Kind: "room.archive", RoomID: target}})
			case "transfer":
				auditKind, target = "governance.transfer", "binding-room"
				r, e = b.f.store.TransferRoomOwnership(t.Context(), OwnershipTransferCommand{Auth: b.owner.Auth, ClientID: "expired-history-transfer", Intent: GovernanceIntent{RoomID: target, TargetUserID: b.member.Auth.UserID, ExpectedOwnerUserID: b.owner.Auth.UserID, ExpectedRevision: 2}})
			}
			if e != nil || r.Outcome != OutcomeCommitted {
				t.Fatalf("expired history blocked %s: %+v %v", kind, r, e)
			}
			var affected int64
			query := `SELECT invalidated_approval_count FROM audit WHERE target_id=$1 AND invalidated_approval_count IS NOT NULL ORDER BY audit_seq DESC LIMIT 1`
			args := []any{target}
			if auditKind != "" {
				query = `SELECT invalidated_approval_count FROM audit WHERE target_id=$1 AND kind=$2 ORDER BY audit_seq DESC LIMIT 1`
				args = append(args, auditKind)
			}
			if e = b.f.admin.QueryRow(t.Context(), query, args...).Scan(&affected); e != nil || affected != expected {
				t.Fatal("expired rows were included in measured count", affected, expected, e)
			}
			var expiredBinding, expiredDevice, liveBinding, liveDevice int
			if e = b.f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM binding_requests WHERE request_id LIKE 'expired-binding-%' AND status='approved'),(SELECT count(*) FROM device_enrollments WHERE request_id LIKE 'expired-device-%' AND status='approved'),(SELECT count(*) FROM binding_requests WHERE request_id='binding-request-one-live-approval' AND status='approved'),(SELECT count(*) FROM device_enrollments WHERE request_id='request-one-live-device-approval' AND status='approved')`).Scan(&expiredBinding, &expiredDevice, &liveBinding, &liveDevice); e != nil || expiredBinding != 10001 || expiredDevice != 10001 || liveBinding != 0 || liveDevice != map[bool]int{true: 0, false: 1}[kind == "user" || kind == "device"] {
				t.Fatal("expired history changed or live approvals omitted", expiredBinding, expiredDevice, liveBinding, liveDevice, e)
			}
		})
	}
}
