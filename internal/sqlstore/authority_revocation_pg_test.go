package sqlstore

import (
	"context"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestUnapprovedBindingRequestDoesNotBlockRevocationOrBecomeApproved(t *testing.T) {
	for _, kind := range []string{"user", "device", "member"} {
		t.Run(kind, func(t *testing.T) {
			b := bindingPGFixture(t, nil)
			ctx := context.Background()
			request := bindingPGCommand(b, "pending-"+kind, model.ActorSlot1, 1, false)
			if r, e := b.f.store.CommitBindingRequest(ctx, request); e != nil || r.Outcome != OutcomeCommitted {
				t.Fatalf("pending request: %+v %v", r, e)
			}
			var r Result
			var e error
			switch kind {
			case "user":
				r, e = b.f.store.SetUserEnabled(ctx, b.admin.Auth, "disable-pending-user", b.member.Auth.UserID, false)
			case "device":
				r, e = b.f.store.RevokeDevice(ctx, b.member.Auth, "revoke-pending-device", b.device.DeviceID)
			case "member":
				var revision uint64
				if e = b.f.admin.QueryRow(ctx, `SELECT revision FROM rooms WHERE room_id='binding-room'`).Scan(&revision); e != nil {
					t.Fatal(e)
				}
				r, e = b.f.store.CommitRoomMembership(ctx, RoomMembershipCommand{Auth: b.owner.Auth, RoomID: "binding-room", ClientID: "remove-pending-member", ExpectedRevision: revision, Intent: RoomMembershipIntent{Action: MembershipRemove, TargetUserID: b.member.Auth.UserID}})
			}
			if e != nil || r.Outcome != OutcomeCommitted {
				t.Fatalf("unapproved request blocked %s revocation: %+v %v", kind, r, e)
			}
			var status string
			var approved *string
			var grants int
			if e = b.f.admin.QueryRow(ctx, `SELECT status,approved_by,(SELECT count(*) FROM binding_grants) FROM binding_requests WHERE request_id=$1`, request.RequestID).Scan(&status, &approved, &grants); e != nil || status != "pending" || approved != nil || grants != 0 {
				t.Fatal("revocation fabricated approval/denial/grant")
			}
			if kind == "member" {
				var revision uint64
				if e = b.f.admin.QueryRow(ctx, `SELECT revision FROM rooms WHERE room_id='binding-room'`).Scan(&revision); e != nil {
					t.Fatal(e)
				}
				if _, e = b.f.store.CommitRoomMembership(ctx, RoomMembershipCommand{Auth: b.owner.Auth, RoomID: "binding-room", ClientID: "readmit-pending-member", ExpectedRevision: revision, Intent: RoomMembershipIntent{Action: MembershipAdmit, Role: RoomMember, TargetUserID: b.member.Auth.UserID}}); e != nil {
					t.Fatal(e)
				}
				complete := BindingCompletionCommand{Auth: b.device.Auth, RoomID: request.RoomID, RequestID: request.RequestID, ClientID: "complete-unapproved", ProofHash: request.ProofHash}
				result, e := b.f.store.CompleteBindingRequest(ctx, complete)
				if e == nil || result.Outcome != OutcomeRefused {
					t.Fatal("readmission implicitly approved old pending request")
				}
				if _, e = b.f.store.DecideBindingRequest(ctx, BindingDecisionCommand{Auth: b.owner.Auth, RoomID: request.RoomID, RequestID: request.RequestID, ClientID: "new-explicit-owner-decision", Approve: true}); e != nil {
					t.Fatal(e)
				}
				if result, e = b.f.store.CompleteBindingRequest(ctx, complete); e != nil || result.Outcome != OutcomeCommitted {
					t.Fatalf("new explicit approval failed: %+v %v", result, e)
				}
			}
		})
	}
}
func TestSingleAgentRetirementPreservesOtherAgentAndDeviceFamily(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "agent-owner")
	c, device := devicePGEnroll(t, f, owner, "agent-device")
	_, first := devicePGSeedBinding(t, f, owner, c, device, "agent-first", true)
	_, second := devicePGSeedBinding(t, f, owner, c, device, "agent-second", true)
	var agentID string
	if e := f.admin.QueryRow(ctx, `SELECT agent_id FROM bindings WHERE room_id='agent-first'`).Scan(&agentID); e != nil {
		t.Fatal(e)
	}
	r, e := f.store.CommitCatalog(ctx, CatalogCommand{Auth: owner.Auth, ClientID: "retire-one-agent", Intent: CatalogIntent{Kind: "agent.retire", AgentID: agentID}})
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("retire Agent: %+v %v", r, e)
	}
	if _, e = f.store.AuthenticateBinding(ctx, first, c.Origin); e == nil {
		t.Fatal("retired Agent kept Binding credential")
	}
	if _, e = f.store.AuthenticateBinding(ctx, second, c.Origin); e != nil {
		t.Fatalf("Agent retirement widened to another Agent: %v", e)
	}
	if _, e = f.store.AuthenticateDevice(ctx, c.Credential.AccessHash, c.Origin); e != nil {
		t.Fatalf("Agent retirement revoked parent Device family: %v", e)
	}
	var live int
	if e = f.admin.QueryRow(ctx, `SELECT count(*) FROM bindings WHERE retired_at IS NULL`).Scan(&live); e != nil || live != 1 {
		t.Fatal("Agent scope touched another reservation")
	}
}
func TestDeviceRevocationResolveIsReadOnlyAndChecksCurrentBrowser(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	login, owner := identityPGLogin(t, f, "member-one", "resolve-owner")
	c, _ := devicePGEnroll(t, f, owner, "resolve-device")
	before := devicePGCounts(t, f)
	r, e := f.store.ResolveDeviceRevocation(ctx, owner.Auth, "revoke-device", c.DeviceID)
	if e != nil || r.Outcome != OutcomeNotCommitted || r.Control != nil || devicePGCounts(t, f) != before {
		t.Fatal("absent device revocation Resolve produced effects")
	}
	if _, e = f.store.RevokeDevice(ctx, owner.Auth, "revoke-device", c.DeviceID); e != nil {
		t.Fatal(e)
	}
	before = devicePGCounts(t, f)
	r, e = f.store.ResolveDeviceRevocation(ctx, owner.Auth, "revoke-device", c.DeviceID)
	if e != nil || r.Outcome != OutcomeReplayed || r.Control == nil || r.Control.DeviceID != c.DeviceID || devicePGCounts(t, f) != before {
		t.Fatalf("device revocation recovery: %+v %v", r, e)
	}
	if _, e = f.store.RevokeSession(ctx, owner.Auth, "revoke-browser"); e != nil {
		t.Fatal(e)
	}
	r, e = f.store.ResolveDeviceRevocation(ctx, owner.Auth, "revoke-device", c.DeviceID)
	assertIdentityAuthorization(t, r, e)
	assertSessionRefused(t, f, login.Session.TokenHash)
}
