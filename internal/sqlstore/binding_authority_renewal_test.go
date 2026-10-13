package sqlstore

import (
	"context"
	"crypto/sha256"
	"reflect"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func bindingPGRenewal(c BindingRequestCommand, id string) BindingRenewalCommand {
	return BindingRenewalCommand{Auth: c.Auth, ClientID: id, RoomID: c.RoomID, BindingID: c.BindingID, Generation: 1, Grant: BindingGrantCandidate{GrantID: "renewed-grant-" + id, TokenHash: sha256.Sum256([]byte("private-renewal-token-" + id))}}
}

func TestBindingAuthorityRenewalAdmissionAndHashAreClosed(t *testing.T) {
	s := &Store{cfg: Config{DeploymentID: "dep"}}
	cmd := bindingPGRenewal(bindingUnitCommand(), "renewal")
	plan, class := s.bindingRenewalPlan(cmd)
	if class != "" {
		t.Fatal(class)
	}
	d := &bindingDependencies{credential: sessionDependency{deviceID: "device", familyID: "family"}}
	original := bindingIntentHash(plan, d)
	for _, change := range []func(*BindingRenewalCommand){func(c *BindingRenewalCommand) { c.BindingID = "other" }, func(c *BindingRenewalCommand) { c.Generation++ }, func(c *BindingRenewalCommand) { c.Grant.GrantID = "other-grant" }, func(c *BindingRenewalCommand) { c.Grant.TokenHash[0]++ }} {
		copy := cmd
		change(&copy)
		p, class := s.bindingRenewalPlan(copy)
		if class != "" {
			t.Fatal(class)
		}
		if bindingIntentHash(p, d) == original {
			t.Fatal("renewal hash omitted exact target or candidate")
		}
	}
	for _, auth := range []AuthContext{{Deployment: "dep", UserID: "user", SessionID: "browser"}, {Deployment: "dep", UserID: "user", BindingGrantID: "narrow"}} {
		copy := cmd
		copy.Auth = auth
		if _, class := s.bindingRenewalPlan(copy); class != ClassAuthorization {
			t.Fatal("non-Device became renewal authority")
		}
	}
	at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	r := BindingAuthorityReceipt{Version: 1, Kind: bindingRenew, Binding: &model.EnterpriseBinding{Version: 1, RoomID: "room", Slot: model.ActorSlot1, BindID: "binding", Generation: 1, AgentID: "agent", Runtime: model.RuntimeClaude, Active: true, ParkEnabled: true}, GrantID: "new-grant", ExpiresAt: at.Add(time.Hour), Seq: 2, Revision: 3}
	if err := validateBindingAuthorityReceipt(r); err != nil {
		t.Fatal(err)
	}
	r.Request = &BindingRequestInfo{}
	if validateBindingAuthorityReceipt(r) == nil {
		t.Fatal("renewal fabricated a fresh approval")
	}
}

func TestBindingAuthorityRenewalAfterExpiryKeepsBindingGenerationAndHistory(t *testing.T) {
	b := bindingPGFixture(t, nil)
	ctx := context.Background()
	c := bindingPGCommand(b, "renew-expired", model.ActorSlot2, 1, false)
	original, _ := bindingPGComplete(t, b, c)
	// Construct an expired credential row in the isolated schema instead of
	// mutating immutable expiry fields or sleeping for the production TTL.
	expiredHash := sha256.Sum256([]byte("synthetic-expired-narrow-token"))
	if _, e := b.f.admin.Exec(ctx, `UPDATE binding_grants SET revoked_at=now() WHERE binding_id=$1`, c.BindingID); e != nil {
		t.Fatal(e)
	}
	if _, e := b.f.admin.Exec(ctx, `INSERT INTO binding_grants(grant_id,binding_id,family_id,token_hash,restore_epoch,created_at,expires_at) SELECT 'expired-narrow-grant',binding_id,family_id,$2,restore_epoch,now()-interval '2 hours',now()-interval '1 hour' FROM binding_grants WHERE grant_id=$1`, c.Grant.GrantID, expiredHash[:]); e != nil {
		t.Fatal(e)
	}
	if _, e := b.f.store.AuthenticateBinding(ctx, expiredHash, b.enrollment.Origin); e == nil {
		t.Fatal("expired narrow token authenticated")
	}
	before := bindingPGCounts(t, b)
	renew := bindingPGRenewal(c, "renew-expired")
	r, e := b.f.store.RenewBindingCredential(ctx, renew)
	if e != nil || r.Outcome != OutcomeCommitted || r.BindingAuthority.Kind != bindingRenew || r.BindingAuthority.Request != nil || r.BindingAuthority.Binding.Generation != 1 || !reflect.DeepEqual(r.BindingAuthority.Binding, original.BindingAuthority.Binding) || len(r.Facts) != 0 || r.Fact.ID != "" || r.Seq != uint64(before[6]) || r.Revision != uint64(before[7])+1 {
		t.Fatalf("renewal changed original binding/history: %+v %v", r, e)
	}
	if r.BindingAuthority.ExpiresAt.After(b.device.RefreshExpiresAt) {
		t.Fatal("narrow grant outlived Device family")
	}
	after := bindingPGCounts(t, b)
	if after[2] != before[2] || after[3] != before[3]+1 || after[4] != before[4] || after[6] != before[6] || after[7] != before[7]+1 || after[8] != before[8]+1 {
		t.Fatalf("renewal altered Binding facts or failed to revoke predecessor: %v -> %v", before, after)
	}
	if _, e := b.f.store.AuthenticateBinding(ctx, renew.Grant.TokenHash, b.enrollment.Origin); e != nil {
		t.Fatal(e)
	}
	for _, hash := range [][32]byte{c.Grant.TokenHash, expiredHash} {
		if _, e := b.f.store.AuthenticateBinding(ctx, hash, b.enrollment.Origin); e == nil {
			t.Fatal("old narrow token revived")
		}
	}
	again, e := b.f.store.ResolveBindingRenewal(ctx, renew)
	if e != nil || again.Outcome != OutcomeReplayed || !reflect.DeepEqual(r.BindingAuthority, again.BindingAuthority) || bindingPGCounts(t, b) != after {
		t.Fatalf("read-only renewal replay: %+v %v", again, e)
	}
	changed := renew
	changed.Grant.TokenHash[0]++
	bad, e := b.f.store.RenewBindingCredential(ctx, changed)
	if e == nil || bad.Outcome != OutcomeConflict || bad.BindingAuthority != nil {
		t.Fatalf("changed renewal candidate replayed: %+v %v", bad, e)
	}
	second := bindingPGRenewal(c, "second-renewal")
	if _, e := b.f.store.RenewBindingCredential(ctx, second); e != nil {
		t.Fatal(e)
	}
	if _, e := b.f.store.AuthenticateBinding(ctx, renew.Grant.TokenHash, b.enrollment.Origin); e == nil {
		t.Fatal("second renewal did not revoke first renewed token")
	}
	again, e = b.f.store.ResolveBindingRenewal(ctx, renew)
	if e != nil || !reflect.DeepEqual(r.BindingAuthority, again.BindingAuthority) {
		t.Fatal("later renewal relabelled original accepted result")
	}
}

func TestBindingAuthorityRenewalDoesNotReapproveAfterOwnerChange(t *testing.T) {
	for _, change := range []string{"disabled", "transferred"} {
		t.Run(change, func(t *testing.T) {
			b := bindingPGFixture(t, nil)
			ctx := context.Background()
			c := bindingPGCommand(b, "renew-owner", model.ActorSlot1, 1, false)
			bindingPGComplete(t, b, c)
			if change == "disabled" {
				if _, e := b.f.store.SetUserEnabled(ctx, b.admin.Auth, "disable-renew-owner", b.owner.Auth.UserID, false); e != nil {
					t.Fatal(e)
				}
			} else {
				var revision uint64
				if e := b.f.admin.QueryRow(ctx, `SELECT revision FROM rooms WHERE room_id=$1`, c.RoomID).Scan(&revision); e != nil {
					t.Fatal(e)
				}
				if _, e := b.f.store.TransferRoomOwnership(ctx, OwnershipTransferCommand{Auth: b.owner.Auth, ClientID: "transfer-renew-owner", Intent: GovernanceIntent{RoomID: c.RoomID, TargetUserID: b.member.Auth.UserID, ExpectedOwnerUserID: b.owner.Auth.UserID, ExpectedRevision: revision}}); e != nil {
					t.Fatal(e)
				}
			}
			renew := bindingPGRenewal(c, "after-owner-change")
			r, e := b.f.store.RenewBindingCredential(ctx, renew)
			if e != nil || r.Outcome != OutcomeCommitted || r.BindingAuthority.Binding.Generation != 1 || len(r.Facts) != 0 {
				t.Fatalf("renewal required new Owner admission: %+v %v", r, e)
			}
			if _, e := b.f.store.AuthenticateBinding(ctx, renew.Grant.TokenHash, b.enrollment.Origin); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestBindingAuthorityRenewalRejectsWrongScopeAndRevokedAuthority(t *testing.T) {
	for _, change := range []string{"wrong-generation", "wrong-device", "wrong-room", "user", "device", "viewer"} {
		t.Run(change, func(t *testing.T) {
			b := bindingPGFixture(t, nil)
			ctx := context.Background()
			c := bindingPGCommand(b, "renew-scope", model.ActorSlot1, 1, false)
			bindingPGComplete(t, b, c)
			renew := bindingPGRenewal(c, "new-candidate")
			switch change {
			case "wrong-generation":
				renew.Generation++
			case "wrong-device":
				_, other := devicePGEnroll(t, b.f, b.member, "renew-other-device")
				renew.Auth = other.Auth
			case "wrong-room":
				renew.RoomID = "room-1"
			case "user":
				if _, e := b.f.store.SetUserEnabled(ctx, b.admin.Auth, "disable-renew-user", b.member.Auth.UserID, false); e != nil {
					t.Fatal(e)
				}
			case "device":
				if _, e := b.f.store.RevokeDevice(ctx, b.member.Auth, "revoke-renew-device", b.device.DeviceID); e != nil {
					t.Fatal(e)
				}
			case "viewer":
				var revision uint64
				if e := b.f.admin.QueryRow(ctx, `SELECT revision FROM rooms WHERE room_id=$1`, c.RoomID).Scan(&revision); e != nil {
					t.Fatal(e)
				}
				if _, e := b.f.store.CommitRoomMembership(ctx, RoomMembershipCommand{Auth: b.owner.Auth, RoomID: c.RoomID, ClientID: "renew-demote-viewer", ExpectedRevision: revision, Intent: RoomMembershipIntent{TargetUserID: b.member.Auth.UserID, Action: MembershipChangeRole, Role: RoomViewer}}); e != nil {
					t.Fatal(e)
				}
			}
			before := bindingPGCounts(t, b)
			r, e := b.f.store.RenewBindingCredential(ctx, renew)
			bindingPGRefused(t, r, e)
			if bindingPGCounts(t, b) != before {
				t.Fatal("invalid renewal scope changed authority")
			}
		})
	}
}

func TestBindingAuthorityRenewalAuditFailureKeepsOldGrantLive(t *testing.T) {
	b := bindingPGFixture(t, nil)
	ctx := context.Background()
	c := bindingPGCommand(b, "renew-audit", model.ActorSlot1, 1, false)
	bindingPGComplete(t, b, c)
	renew := bindingPGRenewal(c, "audit-candidate")
	before := bindingPGCounts(t, b)
	if _, e := b.f.admin.Exec(ctx, `ALTER TABLE audit ADD CONSTRAINT reject_binding_renewal CHECK(kind<>'binding.renew')`); e != nil {
		t.Fatal(e)
	}
	r, e := b.f.store.RenewBindingCredential(ctx, renew)
	if k := kernelErr(t, r, e); k.Outcome != OutcomeNotCommitted || k.Class != ClassConstraintViolation {
		t.Fatalf("renewal audit failure: %+v", r)
	}
	if bindingPGCounts(t, b) != before {
		t.Fatal("failed audit partially replaced credential/revision")
	}
	if _, e := b.f.store.AuthenticateBinding(ctx, c.Grant.TokenHash, b.enrollment.Origin); e != nil {
		t.Fatal("rollback revoked the original credential")
	}
	if _, e := b.f.store.AuthenticateBinding(ctx, renew.Grant.TokenHash, b.enrollment.Origin); e == nil {
		t.Fatal("rollback kept new credential alive")
	}
	r, e = b.f.store.ResolveBindingRenewal(ctx, renew)
	if e != nil || r.Outcome != OutcomeNotCommitted || r.BindingAuthority != nil {
		t.Fatalf("failed renewal has receipt: %+v %v", r, e)
	}
}

func TestBindingAuthorityUnknownRenewalHasTwoRealCommitOutcomes(t *testing.T) {
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			proxy := startProxy(t, mode)
			b := bindingPGFixture(t, proxy.dsn)
			ctx := context.Background()
			c := bindingPGCommand(b, "renew-loss", model.ActorSlot1, 1, false)
			bindingPGComplete(t, b, c)
			renew := bindingPGRenewal(c, "uncertain-renewal")
			before := bindingPGCounts(t, b)
			proxy.arm()
			r, e := b.f.store.RenewBindingCredential(ctx, renew)
			if k := kernelErr(t, r, e); k.Outcome != OutcomeUnknown || r.BindingAuthority != nil || len(r.Facts) != 0 {
				t.Fatalf("unknown renewal disclosed candidate: %+v", r)
			}
			resolved, e := b.f.store.ResolveBindingRenewal(ctx, renew)
			if e != nil {
				t.Fatal(e)
			}
			after := bindingPGCounts(t, b)
			if mode == "drop-response" {
				if resolved.Outcome != OutcomeReplayed || resolved.BindingAuthority == nil || after[2] != before[2] || after[3] != before[3]+1 || after[6] != before[6] || after[7] != before[7]+1 || after[8] != before[8]+1 {
					t.Fatalf("committed renewal: %+v %v -> %v", resolved, before, after)
				}
			} else if resolved.Outcome != OutcomeNotCommitted || resolved.BindingAuthority != nil || after != before {
				t.Fatalf("absent renewal: %+v %v -> %v", resolved, before, after)
			}
			if e := b.f.store.Close(); e != nil {
				t.Fatal(e)
			}
			b.f.store, e = Open(ctx, b.f.cfg)
			if e != nil {
				t.Fatal(e)
			}
			explicit, e := b.f.store.RenewBindingCredential(ctx, renew)
			if e != nil || explicit.BindingAuthority == nil {
				t.Fatalf("explicit renewal retry: %+v %v", explicit, e)
			}
			if mode == "drop-response" && !reflect.DeepEqual(resolved.BindingAuthority, explicit.BindingAuthority) {
				t.Fatal("explicit renewal retry changed original receipt")
			}
			if _, e := b.f.store.AuthenticateBinding(ctx, renew.Grant.TokenHash, b.enrollment.Origin); e != nil {
				t.Fatal(e)
			}
			if _, e := b.f.store.AuthenticateBinding(ctx, c.Grant.TokenHash, b.enrollment.Origin); e == nil {
				t.Fatal("uncertain renewal revived old grant")
			}
		})
	}
}
