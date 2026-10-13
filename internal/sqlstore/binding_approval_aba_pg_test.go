package sqlstore

import (
	"context"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestBindingApprovalCannotReviveThroughOwnerOrMemberABA(t *testing.T) {
	for _, path := range []string{"owner-disable-enable", "member-remove-admit", "member-viewer-member", "owner-away-and-back"} {
		t.Run(path, func(t *testing.T) {
			b := bindingPGFixture(t, nil)
			ctx := context.Background()
			request := bindingPGCommand(b, "original-approval", model.ActorSlot1, 1, false)
			completion := bindingPGApprove(t, b, request)
			switch path {
			case "owner-disable-enable":
				if _, err := b.f.store.SetUserEnabled(ctx, b.admin.Auth, "disable-approver", b.owner.Auth.UserID, false); err != nil {
					t.Fatal(err)
				}
				if _, err := b.f.store.SetUserEnabled(ctx, b.admin.Auth, "reenable-approver", b.owner.Auth.UserID, true); err != nil {
					t.Fatal(err)
				}
			case "member-remove-admit", "member-viewer-member":
				first := RoomMembershipIntent{TargetUserID: b.member.Auth.UserID, Action: MembershipRemove}
				second := RoomMembershipIntent{TargetUserID: b.member.Auth.UserID, Action: MembershipAdmit, Role: RoomMember}
				if path == "member-viewer-member" {
					first.Action, first.Role, second.Action = MembershipChangeRole, RoomViewer, MembershipChangeRole
				}
				if _, err := b.f.store.CommitRoomMembership(ctx, RoomMembershipCommand{Auth: b.owner.Auth, RoomID: request.RoomID, ClientID: "revoke-member", ExpectedRevision: 2, Intent: first}); err != nil {
					t.Fatal(err)
				}
				if _, err := b.f.store.CommitRoomMembership(ctx, RoomMembershipCommand{Auth: b.owner.Auth, RoomID: request.RoomID, ClientID: "restore-member", ExpectedRevision: 3, Intent: second}); err != nil {
					t.Fatal(err)
				}
			case "owner-away-and-back":
				if _, err := b.f.store.TransferRoomOwnership(ctx, OwnershipTransferCommand{Auth: b.owner.Auth, ClientID: "owner-away", Intent: GovernanceIntent{RoomID: request.RoomID, TargetUserID: b.member.Auth.UserID, ExpectedOwnerUserID: b.owner.Auth.UserID, ExpectedRevision: 2}}); err != nil {
					t.Fatal(err)
				}
				if _, err := b.f.store.TransferRoomOwnership(ctx, OwnershipTransferCommand{Auth: b.member.Auth, ClientID: "owner-back", Intent: GovernanceIntent{RoomID: request.RoomID, TargetUserID: b.owner.Auth.UserID, ExpectedOwnerUserID: b.member.Auth.UserID, ExpectedRevision: 3}}); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := b.f.store.CompleteBindingRequest(ctx, completion); err == nil || result.BindingAuthority != nil || len(result.Facts) != 0 {
				t.Fatalf("old approval revived after %s: %+v %v", path, result, err)
			}
			var status string
			var bindings, grants int
			if err := b.f.admin.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM bindings),(SELECT count(*) FROM binding_grants) FROM binding_requests WHERE request_id=$1`, request.RequestID).Scan(&status, &bindings, &grants); err != nil {
				t.Fatal(err)
			}
			if status != "denied" || bindings != 0 || grants != 0 {
				t.Fatalf("revoked request created authority: %s/%d/%d", status, bindings, grants)
			}
			// Member removal is Room-scoped: its Device family stays live, so
			// denial is due to terminal approval invalidation, not a false
			// positive caused by disabling all of this User's credentials.
			if _, err := b.f.store.AuthenticateDevice(ctx, b.enrollment.Credential.AccessHash, b.enrollment.Origin); err != nil {
				t.Fatalf("unrelated applicant Device was revoked: %v", err)
			}
		})
	}
}
