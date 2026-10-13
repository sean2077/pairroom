package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// All login/enrollment inputs are synthetic trusted test identities; the
// fixture uses real PostgreSQL, not a vendor or browser acceptance claim.
type bindingPGState struct {
	f                    *fixture
	owner, member, admin BrowserSession
	device               DeviceCredential
	enrollment           DeviceEnrollmentCommand
	agentID              string
}

func bindingPGFixture(t *testing.T, transform func(string) string) *bindingPGState {
	t.Helper()
	f := identityPGFixture(t, transform)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "binding-owner")
	_, member := identityPGLogin(t, f, "member-two", "binding-member")
	_, admin := identityPGLogin(t, f, "operator-selected", "binding-admin")
	intent, err := model.PrepareEnterpriseRoomIntent("Binding Room", model.Collaboration{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.store.CommitRoomCreate(ctx, RoomCreateCommand{Auth: owner.Auth, ClientID: "create-binding-room", RoomID: "binding-room", Intent: intent})
	if err != nil {
		t.Fatal(err)
	}
	r, err = f.store.CommitRoomMembership(ctx, RoomMembershipCommand{Auth: owner.Auth, RoomID: "binding-room", ClientID: "admit-member", ExpectedRevision: r.Revision, Intent: RoomMembershipIntent{TargetUserID: member.Auth.UserID, Action: MembershipAdmit, Role: RoomMember}})
	if err != nil {
		t.Fatal(err)
	}
	enrollment, device := devicePGEnroll(t, f, member, "binding-endpoint")
	source := sha256.Sum256([]byte("opaque-binding-agent"))
	r, err = f.store.RegisterAgent(ctx, device.Auth, "register-binding-agent", base64.RawURLEncoding.EncodeToString(source[:]), model.RuntimeClaude)
	if err != nil {
		t.Fatal(err)
	}
	return &bindingPGState{f: f, owner: owner, member: member, admin: admin, device: device, enrollment: enrollment, agentID: r.Control.AgentID}
}
func bindingPGCommand(b *bindingPGState, id string, slot model.ActorID, observed uint64, replace bool) BindingRequestCommand {
	return BindingRequestCommand{Auth: b.device.Auth, ClientID: "request-op-" + id, RequestID: "binding-request-" + id, RoomID: "binding-room", AgentID: b.agentID, BindingID: "binding-" + id, Slot: slot, ObservedGeneration: observed, Replace: replace, ParkEnabled: true, ProofHash: sha256.Sum256([]byte("private-request-proof-" + id)), Grant: BindingGrantCandidate{GrantID: "binding-grant-" + id, TokenHash: sha256.Sum256([]byte("private-binding-token-" + id))}}
}
func bindingPGApprove(t *testing.T, b *bindingPGState, c BindingRequestCommand) BindingCompletionCommand {
	t.Helper()
	ctx := context.Background()
	if r, e := b.f.store.CommitBindingRequest(ctx, c); e != nil || r.Outcome != OutcomeCommitted || r.BindingAuthority == nil || r.BindingAuthority.Request.Status != "pending" {
		t.Fatalf("request: %+v %v", r, e)
	}
	decision := BindingDecisionCommand{Auth: b.owner.Auth, ClientID: "approve-" + c.RequestID, RoomID: c.RoomID, RequestID: c.RequestID, Approve: true}
	if r, e := b.f.store.DecideBindingRequest(ctx, decision); e != nil || r.Outcome != OutcomeCommitted || r.BindingAuthority.Request.Status != "approved" {
		t.Fatalf("approve: %+v %v", r, e)
	}
	return BindingCompletionCommand{Auth: c.Auth, ClientID: "complete-" + c.RequestID, RoomID: c.RoomID, RequestID: c.RequestID, ProofHash: c.ProofHash}
}
func bindingPGComplete(t *testing.T, b *bindingPGState, c BindingRequestCommand) (Result, BindingCredential) {
	t.Helper()
	completion := bindingPGApprove(t, b, c)
	r, e := b.f.store.CompleteBindingRequest(context.Background(), completion)
	if e != nil || r.Outcome != OutcomeCommitted || r.BindingAuthority == nil || r.BindingAuthority.Binding == nil {
		t.Fatalf("complete: %+v %v", r, e)
	}
	credential, e := b.f.store.AuthenticateBinding(context.Background(), c.Grant.TokenHash, b.enrollment.Origin)
	if e != nil {
		t.Fatal(e)
	}
	return r, credential
}
func bindingPGCounts(t *testing.T, b *bindingPGState) [9]int64 {
	t.Helper()
	var n [9]int64
	err := b.f.admin.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM binding_requests),(SELECT count(*) FROM binding_operations),(SELECT count(*) FROM bindings),(SELECT count(*) FROM binding_grants),(SELECT count(*) FROM slot_generations),(SELECT count(*) FROM audit),current_seq,revision,(SELECT count(*) FROM binding_grants WHERE revoked_at IS NOT NULL) FROM rooms WHERE room_id='binding-room'`).Scan(&n[0], &n[1], &n[2], &n[3], &n[4], &n[5], &n[6], &n[7], &n[8])
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func bindingPGRefused(t *testing.T, r Result, err error) {
	t.Helper()
	if err == nil || r.Outcome != OutcomeRefused && r.Outcome != OutcomeConflict || r.BindingAuthority != nil || r.Fact.ID != "" || len(r.Facts) != 0 {
		t.Fatalf("refusal leaked Binding result: %+v %v", r, err)
	}
}

func TestBindingAuthorityRequiresSeparateOwnerAndOriginalDeviceConsent(t *testing.T) {
	b := bindingPGFixture(t, nil)
	ctx := context.Background()
	c := bindingPGCommand(b, "consent", model.ActorSlot1, 1, false)
	r, e := b.f.store.CommitBindingRequest(ctx, c)
	if e != nil || r.BindingAuthority.Request.Status != "pending" {
		t.Fatalf("request: %+v %v", r, e)
	}
	before := bindingPGCounts(t, b)
	completion := BindingCompletionCommand{Auth: c.Auth, ClientID: "complete", RoomID: c.RoomID, RequestID: c.RequestID, ProofHash: c.ProofHash}
	r, e = b.f.store.CompleteBindingRequest(ctx, completion)
	bindingPGRefused(t, r, e)
	for _, auth := range []AuthContext{b.member.Auth, b.admin.Auth, b.device.Auth} {
		r, e = b.f.store.DecideBindingRequest(ctx, BindingDecisionCommand{Auth: auth, ClientID: "not-owner", RoomID: c.RoomID, RequestID: c.RequestID, Approve: true})
		bindingPGRefused(t, r, e)
	}
	if got := bindingPGCounts(t, b); got != before {
		t.Fatalf("unapproved authority mutated rows: %v -> %v", before, got)
	}
	if requests, e := b.f.store.ListBindingRequests(ctx, b.owner.Auth, c.RoomID); e != nil || len(requests) != 1 || requests[0].AgentID != c.AgentID {
		t.Fatalf("Owner cannot inspect request: %+v %v", requests, e)
	}
	if _, e := b.f.store.ListBindingRequests(ctx, b.admin.Auth, c.RoomID); e == nil {
		t.Fatal("administrator acquired Room approval access")
	}
	decision := BindingDecisionCommand{Auth: b.owner.Auth, ClientID: "owner-approve", RoomID: c.RoomID, RequestID: c.RequestID, Approve: true}
	r, e = b.f.store.DecideBindingRequest(ctx, decision)
	if e != nil || r.BindingAuthority.Request.ApprovedBy != b.owner.Auth.UserID {
		t.Fatal(e)
	}
	bad := completion
	bad.ProofHash = sha256.Sum256([]byte("wrong-proof"))
	r, e = b.f.store.CompleteBindingRequest(ctx, bad)
	bindingPGRefused(t, r, e)
	_, other := devicePGEnroll(t, b.f, b.member, "other-endpoint")
	bad = completion
	bad.Auth = other.Auth
	r, e = b.f.store.CompleteBindingRequest(ctx, bad)
	bindingPGRefused(t, r, e)
	r, e = b.f.store.CompleteBindingRequest(ctx, completion)
	if e != nil || r.BindingAuthority.Binding.Generation != 1 || len(r.Facts) != 1 {
		t.Fatalf("original endpoint completion: %+v %v", r, e)
	}
	credential, e := b.f.store.AuthenticateBinding(ctx, c.Grant.TokenHash, b.enrollment.Origin)
	if e != nil || credential.RoomID != c.RoomID || credential.Generation != 1 || credential.AgentID != c.AgentID {
		t.Fatalf("narrow grant: %+v %v", credential, e)
	}
	encoded, _ := json.Marshal(r.BindingAuthority)
	for _, secret := range []string{hex.EncodeToString(c.ProofHash[:]), hex.EncodeToString(c.Grant.TokenHash[:]), "proof_hash", "token_hash", "family_id", "session_id", "transcript"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("Binding receipt exposes %q", secret)
		}
	}
	before = bindingPGCounts(t, b)
	again, e := b.f.store.CompleteBindingRequest(ctx, completion)
	if e != nil || again.Outcome != OutcomeReplayed || !reflect.DeepEqual(r.BindingAuthority, again.BindingAuthority) || !reflect.DeepEqual(r.Facts, again.Facts) || bindingPGCounts(t, b) != before {
		t.Fatalf("original result replay changed state: %+v %v", again, e)
	}
	changed := c
	changed.ParkEnabled = false
	r, e = b.f.store.CommitBindingRequest(ctx, changed)
	if e == nil || r.Outcome != OutcomeConflict {
		t.Fatalf("changed frozen intent replayed: %+v %v", r, e)
	}
}

func TestBindingAuthorityPendingApprovalCannotOutliveItsOwner(t *testing.T) {
	for _, change := range []string{"disabled", "transferred"} {
		t.Run(change, func(t *testing.T) {
			b := bindingPGFixture(t, nil)
			ctx := context.Background()
			c := bindingPGCommand(b, change, model.ActorSlot1, 1, false)
			completion := bindingPGApprove(t, b, c)
			if change == "disabled" {
				if _, e := b.f.store.SetUserEnabled(ctx, b.admin.Auth, "disable-owner", b.owner.Auth.UserID, false); e != nil {
					t.Fatal(e)
				}
			} else {
				var revision uint64
				if e := b.f.admin.QueryRow(ctx, `SELECT revision FROM rooms WHERE room_id=$1`, c.RoomID).Scan(&revision); e != nil {
					t.Fatal(e)
				}
				if r, e := b.f.store.TransferRoomOwnership(ctx, OwnershipTransferCommand{Auth: b.owner.Auth, ClientID: "owner-transfer", Intent: GovernanceIntent{RoomID: c.RoomID, TargetUserID: b.member.Auth.UserID, ExpectedOwnerUserID: b.owner.Auth.UserID, ExpectedRevision: revision}}); e != nil {
					t.Fatalf("transfer: %+v %v", r, e)
				}
			}
			before := bindingPGCounts(t, b)
			r, e := b.f.store.CompleteBindingRequest(ctx, completion)
			bindingPGRefused(t, r, e)
			if bindingPGCounts(t, b) != before {
				t.Fatal("stale Owner approval created authority")
			}
		})
	}
}

func TestBindingAuthorityRechecksDeviceAndMembershipInsideTransaction(t *testing.T) {
	for _, change := range []string{"device", "membership"} {
		t.Run(change, func(t *testing.T) {
			b := bindingPGFixture(t, nil)
			ctx := context.Background()
			c := bindingPGCommand(b, change, model.ActorSlot1, 1, false)
			completion := bindingPGApprove(t, b, c)
			// completion.Auth was obtained before revocation, deliberately reproducing
			// the middleware/transaction gap without sleeps or a forged principal.
			if change == "device" {
				if _, e := b.f.store.RevokeDevice(ctx, b.member.Auth, "revoke-original", b.device.DeviceID); e != nil {
					t.Fatal(e)
				}
			} else {
				var revision uint64
				if e := b.f.admin.QueryRow(ctx, `SELECT revision FROM rooms WHERE room_id=$1`, c.RoomID).Scan(&revision); e != nil {
					t.Fatal(e)
				}
				if _, e := b.f.store.CommitRoomMembership(ctx, RoomMembershipCommand{Auth: b.owner.Auth, RoomID: c.RoomID, ClientID: "remove-member", ExpectedRevision: revision, Intent: RoomMembershipIntent{TargetUserID: b.member.Auth.UserID, Action: MembershipRemove}}); e != nil {
					t.Fatal(e)
				}
			}
			before := bindingPGCounts(t, b)
			r, e := b.f.store.CompleteBindingRequest(ctx, completion)
			bindingPGRefused(t, r, e)
			r, e = b.f.store.ResolveBindingCompletion(ctx, completion)
			bindingPGRefused(t, r, e)
			if bindingPGCounts(t, b) != before {
				t.Fatal("stale authenticated context bypassed live authority")
			}
		})
	}
}

func TestBindingAuthorityReplacementRetiresOnlyExactBindingAndGrant(t *testing.T) {
	b := bindingPGFixture(t, nil)
	ctx := context.Background()
	first := bindingPGCommand(b, "first", model.ActorSlot1, 1, false)
	original, oldCredential := bindingPGComplete(t, b, first)
	replacement := bindingPGCommand(b, "replacement", model.ActorSlot1, 1, true)
	completion := bindingPGApprove(t, b, replacement)
	result, e := b.f.store.CompleteBindingRequest(ctx, completion)
	if e != nil || len(result.Facts) != 2 || result.BindingAuthority.Binding.Generation != 2 {
		t.Fatalf("replace: %+v %v", result, e)
	}
	retired, e := model.DecodeEnterpriseBindingUpdate(result.Facts[0])
	if e != nil || retired.Active || retired.BindID != first.BindingID || retired.Generation != 1 || retired.RetiredReason != "binding.replaced" {
		t.Fatalf("old retirement: %+v %v", retired, e)
	}
	if _, e := b.f.store.AuthenticateBinding(ctx, first.Grant.TokenHash, b.enrollment.Origin); e == nil {
		t.Fatal("retired Binding grant still authenticates")
	}
	if _, e := b.f.store.AuthenticateDevice(ctx, b.enrollment.Credential.AccessHash, b.enrollment.Origin); e != nil {
		t.Fatal("Binding replacement revoked Device authority")
	}
	if _, e := b.f.store.AuthenticateBinding(ctx, replacement.Grant.TokenHash, b.enrollment.Origin); e != nil {
		t.Fatal(e)
	}
	before := bindingPGCounts(t, b)
	// Accepted results remain original even when mutable slot/head state has
	// changed; resolution uses the original Device scope, not old narrow grant.
	again, e := b.f.store.ResolveBindingCompletion(ctx, BindingCompletionCommand{Auth: first.Auth, ClientID: "complete-" + first.RequestID, RoomID: first.RoomID, RequestID: first.RequestID, ProofHash: first.ProofHash})
	if e != nil || again.Outcome != OutcomeReplayed || !reflect.DeepEqual(original.BindingAuthority, again.BindingAuthority) || bindingPGCounts(t, b) != before {
		t.Fatalf("original receipt was state-revalidated: %+v %v", again, e)
	}
	_, e = b.f.store.ReadonlySnapshot(ctx, oldCredential.Auth, first.RoomID)
	if e == nil {
		t.Fatal("stale narrow credential read content after replacement")
	}
	// A retired vacant slot allocates generation +1, never reuses history.
	if _, e := b.f.store.RevokeDevice(ctx, b.member.Auth, "retire-device", b.device.DeviceID); e != nil {
		t.Fatal(e)
	}
	enrollment, device := devicePGEnroll(t, b.f, b.member, "new-device")
	source := sha256.Sum256([]byte("new-agent-source"))
	registered, e := b.f.store.RegisterAgent(ctx, device.Auth, "new-agent", base64.RawURLEncoding.EncodeToString(source[:]), model.RuntimeClaude)
	if e != nil {
		t.Fatal(e)
	}
	b.device, b.enrollment, b.agentID = device, enrollment, registered.Control.AgentID
	vacant := bindingPGCommand(b, "after-retirement", model.ActorSlot1, 2, false)
	newResult, _ := bindingPGComplete(t, b, vacant)
	if newResult.BindingAuthority.Binding.Generation != 3 {
		t.Fatal("retired generation was reused")
	}
}

func TestBindingAuthorityCrossRoomAgentReservationIncludesArchive(t *testing.T) {
	b := bindingPGFixture(t, nil)
	ctx := context.Background()
	first := bindingPGCommand(b, "reserved", model.ActorSlot1, 1, false)
	bindingPGComplete(t, b, first)
	if _, e := b.f.admin.Exec(ctx, `UPDATE rooms SET archived_at=now() WHERE room_id=$1`, first.RoomID); e != nil {
		t.Fatal(e)
	}
	intent, _ := model.PrepareEnterpriseRoomIntent("Second Room", model.Collaboration{})
	r, e := b.f.store.CommitRoomCreate(ctx, RoomCreateCommand{Auth: b.owner.Auth, RoomID: "second-binding-room", ClientID: "create-second", Intent: intent})
	if e != nil {
		t.Fatal(e)
	}
	if _, e := b.f.store.CommitRoomMembership(ctx, RoomMembershipCommand{Auth: b.owner.Auth, RoomID: "second-binding-room", ClientID: "admit-second", ExpectedRevision: r.Revision, Intent: RoomMembershipIntent{TargetUserID: b.member.Auth.UserID, Action: MembershipAdmit, Role: RoomMember}}); e != nil {
		t.Fatal(e)
	}
	c := bindingPGCommand(b, "cross-room", model.ActorSlot2, 1, false)
	c.RoomID = "second-binding-room"
	r, e = b.f.store.CommitBindingRequest(ctx, c)
	bindingPGRefused(t, r, e)
	var active int
	if e := b.f.admin.QueryRow(ctx, `SELECT count(*) FROM bindings WHERE agent_id=$1 AND retired_at IS NULL`, b.agentID).Scan(&active); e != nil || active != 1 {
		t.Fatalf("archive freed canonical Agent: %d %v", active, e)
	}
}

func TestBindingAuthorityConcurrentApprovedRequestsHaveOneSlotWinner(t *testing.T) {
	b := bindingPGFixture(t, nil)
	first := bindingPGCommand(b, "race-one", model.ActorSlot1, 1, false)
	second := bindingPGCommand(b, "race-two", model.ActorSlot1, 1, false)
	c1, c2 := bindingPGApprove(t, b, first), bindingPGApprove(t, b, second)
	start := make(chan struct{})
	results := make(chan Result, 2)
	var wg sync.WaitGroup
	for _, command := range []BindingCompletionCommand{c1, c2} {
		wg.Add(1)
		go func(c BindingCompletionCommand) {
			defer wg.Done()
			<-start
			r, _ := b.f.store.CompleteBindingRequest(context.Background(), c)
			results <- r
		}(command)
	}
	close(start)
	wg.Wait()
	close(results)
	committed, refused := 0, 0
	for r := range results {
		if r.Outcome == OutcomeCommitted {
			committed++
		} else if r.Outcome == OutcomeRefused {
			refused++
		}
	}
	if committed != 1 || refused != 1 {
		t.Fatalf("same generation admitted %d winners and %d refusals", committed, refused)
	}
	var active int
	if e := b.f.admin.QueryRow(context.Background(), `SELECT count(*) FROM bindings WHERE room_id=$1 AND retired_at IS NULL`, first.RoomID).Scan(&active); e != nil || active != 1 {
		t.Fatalf("slot has %d active bindings: %v", active, e)
	}
}

func TestBindingAuthorityAuditFailureRollsBackGenerationGrantAndFacts(t *testing.T) {
	b := bindingPGFixture(t, nil)
	ctx := context.Background()
	first := bindingPGCommand(b, "original", model.ActorSlot1, 1, false)
	bindingPGComplete(t, b, first)
	c := bindingPGCommand(b, "audit", model.ActorSlot1, 1, true)
	completion := bindingPGApprove(t, b, c)
	before := bindingPGCounts(t, b)
	if _, e := b.f.admin.Exec(ctx, `ALTER TABLE audit ADD CONSTRAINT binding_audit_reject CHECK(kind<>'binding.complete') NOT VALID`); e != nil {
		t.Fatal(e)
	}
	r, e := b.f.store.CompleteBindingRequest(ctx, completion)
	if k := kernelErr(t, r, e); k.Outcome != OutcomeNotCommitted || k.Class != ClassConstraintViolation {
		t.Fatalf("audit result: %+v", r)
	}
	if after := bindingPGCounts(t, b); after != before {
		t.Fatalf("audit failure left partial Binding state: %v -> %v", before, after)
	}
	if _, e := b.f.store.AuthenticateBinding(ctx, first.Grant.TokenHash, b.enrollment.Origin); e != nil {
		t.Fatal("failed replacement revoked original grant")
	}
	r, e = b.f.store.ResolveBindingCompletion(ctx, completion)
	if e != nil || r.Outcome != OutcomeNotCommitted || r.BindingAuthority != nil {
		t.Fatalf("failed replacement resolved as accepted: %+v %v", r, e)
	}
}

func TestBindingAuthorityUnknownCompletionHasTwoRealCommitOutcomes(t *testing.T) {
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			proxy := startProxy(t, mode)
			b := bindingPGFixture(t, proxy.dsn)
			ctx := context.Background()
			c := bindingPGCommand(b, "uncertain-binding", model.ActorSlot2, 1, false)
			completion := bindingPGApprove(t, b, c)
			before := bindingPGCounts(t, b)
			proxy.arm()
			r, e := b.f.store.CompleteBindingRequest(ctx, completion)
			if k := kernelErr(t, r, e); k.Outcome != OutcomeUnknown || r.BindingAuthority != nil || r.Fact.ID != "" {
				t.Fatalf("unknown disclosed authority: %+v", r)
			}
			resolved, e := b.f.store.ResolveBindingCompletion(ctx, completion)
			if e != nil {
				t.Fatal(e)
			}
			after := bindingPGCounts(t, b)
			if mode == "drop-response" {
				if resolved.Outcome != OutcomeReplayed || resolved.BindingAuthority == nil || after[2] != before[2]+1 || after[3] != before[3]+1 || after[6] != before[6]+1 {
					t.Fatalf("committed verification: %+v %v -> %v", resolved, before, after)
				}
			} else if resolved.Outcome != OutcomeNotCommitted || resolved.BindingAuthority != nil || after != before {
				t.Fatalf("absent verification: %+v %v -> %v", resolved, before, after)
			}
			if e := b.f.store.Close(); e != nil {
				t.Fatal(e)
			}
			b.f.store, e = Open(ctx, b.f.cfg)
			if e != nil {
				t.Fatal(e)
			}
			explicit, e := b.f.store.CompleteBindingRequest(ctx, completion)
			if e != nil || explicit.BindingAuthority == nil {
				t.Fatalf("explicit retry: %+v %v", explicit, e)
			}
			if mode == "drop-response" && !reflect.DeepEqual(explicit.BindingAuthority, resolved.BindingAuthority) {
				t.Fatal("explicit replay changed original result")
			}
			if _, e := b.f.store.AuthenticateBinding(ctx, c.Grant.TokenHash, b.enrollment.Origin); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestBindingAuthorityNarrowGrantFeedsSharedRelayAndRetirementProjection(t *testing.T) {
	b := bindingPGFixture(t, nil)
	ctx := context.Background()
	// The owner endpoint uses a different Agent with the same actual Runtime.
	ownerEnrollment, ownerDevice := devicePGEnroll(t, b.f, b.owner, "owner-endpoint")
	source := sha256.Sum256([]byte("owner-agent-source"))
	registered, e := b.f.store.RegisterAgent(ctx, ownerDevice.Auth, "register-owner-agent", base64.RawURLEncoding.EncodeToString(source[:]), model.RuntimeClaude)
	if e != nil {
		t.Fatal(e)
	}
	ownerState := *b
	ownerState.device, ownerState.enrollment, ownerState.agentID = ownerDevice, ownerEnrollment, registered.Control.AgentID
	ownerRequest := bindingPGCommand(&ownerState, "owner-slot", model.ActorSlot1, 1, false)
	_, ownerGrant := bindingPGComplete(t, &ownerState, ownerRequest)
	memberRequest := bindingPGCommand(b, "member-slot", model.ActorSlot2, 1, false)
	_, memberGrant := bindingPGComplete(t, b, memberRequest)
	run := func(credential BindingCredential, id string, intent relay.EnterpriseOperationIntent) Result {
		var revision uint64
		if e := b.f.admin.QueryRow(ctx, `SELECT revision FROM rooms WHERE room_id='binding-room'`).Scan(&revision); e != nil {
			t.Fatal(e)
		}
		r, e := b.f.store.CommitRelayOperation(ctx, RelayCommand{Auth: credential.Auth, RoomID: "binding-room", ClientID: id, BindingID: credential.BindingID, Generation: credential.Generation, ExpectedRevision: revision, Intent: intent})
		if e != nil {
			t.Fatalf("Relay %s: %+v %v", id, r, e)
		}
		return r
	}
	sent := run(ownerGrant, "send-delivering", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "first full body", ExpectedPeerGeneration: 1})
	claim := run(memberGrant, "claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim})
	if claim.Relay.Claim == nil || claim.Relay.Claim.ID != sent.Relay.Message.ID {
		t.Fatal("claim did not release stored first message")
	}
	queued := run(ownerGrant, "send-queued", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "second full body", ExpectedPeerGeneration: 1})
	replacement := bindingPGCommand(b, "replace-member", model.ActorSlot2, 1, true)
	bindingPGComplete(t, b, replacement)
	snapshot, e := b.f.store.ReadonlySnapshot(ctx, b.owner.Auth, "binding-room")
	if e != nil {
		t.Fatal(e)
	}
	projection, e := relay.ProjectEnterpriseSnapshot(toRelaySnapshot(snapshot))
	if e != nil {
		t.Fatal(e)
	}
	states := map[string]string{}
	for _, message := range projection.Messages {
		states[message.ID] = message.State
	}
	if states[sent.Relay.Message.ID] != "unknown" || states[queued.Relay.Message.ID] != "cancelled" {
		t.Fatalf("replacement changed accepted delivery meaning: %+v", states)
	}
	if _, e := b.f.store.AuthenticateBinding(ctx, memberRequest.Grant.TokenHash, b.enrollment.Origin); e == nil {
		t.Fatal("old collector credential remained live")
	}
	var kernel *KernelError
	_, e = b.f.store.ReadonlySnapshot(ctx, memberGrant.Auth, "binding-room")
	if !errors.As(e, &kernel) || kernel.Class != ClassAuthorization {
		t.Fatalf("old collector content access: %v", e)
	}
}

func TestBindingAuthorityContextIsFencedMetadataAndDoesNotGrantContent(t *testing.T) {
	b := bindingPGFixture(t, nil)
	ctx := context.Background()
	for _, auth := range []AuthContext{b.member.Auth, b.device.Auth} {
		view, e := b.f.store.ReadBindingContext(ctx, auth, "binding-room")
		if e != nil || view.Role != "member" || view.UserID != b.member.Auth.UserID || view.OwnerUserID != b.owner.Auth.UserID || view.OwnershipPending || len(view.Slots) != 2 {
			t.Fatalf("context: %+v %v", view, e)
		}
		for _, slot := range model.SlotActors() {
			entry := view.Slots[slot]
			if entry.Slot != slot || entry.HeadGeneration != 1 || !entry.Vacant || entry.BindingID != "" || entry.Runtime != "" {
				t.Fatalf("vacant context invented binding/default: %+v", entry)
			}
		}
	}
	if view, e := b.f.store.ReadBindingContext(ctx, b.admin.Auth, "binding-room"); e == nil || view.RoomID != "" || view.Slots != nil {
		t.Fatalf("admin without membership read context: %+v %v", view, e)
	}
	c := bindingPGCommand(b, "context-active", model.ActorSlot2, 1, false)
	accepted, grant := bindingPGComplete(t, b, c)
	view, e := b.f.store.ReadBindingContext(ctx, b.device.Auth, c.RoomID)
	if e != nil || view.Slots[c.Slot].Vacant || view.Slots[c.Slot].BindingID != c.BindingID || view.Slots[c.Slot].Runtime != model.RuntimeClaude {
		t.Fatalf("actual endpoint omitted: %+v %v", view, e)
	}
	if _, e := b.f.store.ReadBindingContext(ctx, grant.Auth, c.RoomID); e == nil {
		t.Fatal("narrow Binding grant became Device admission authority")
	}
	if _, e := b.f.store.SetUserEnabled(ctx, b.admin.Auth, "disable-context-owner", b.owner.Auth.UserID, false); e != nil {
		t.Fatal(e)
	}
	view, e = b.f.store.ReadBindingContext(ctx, b.device.Auth, c.RoomID)
	if e != nil || !view.OwnershipPending {
		t.Fatalf("owner governance state omitted: %+v %v", view, e)
	}
	// Completed Member authority is independent of a later-disabled Owner.
	resolved, e := b.f.store.ResolveBindingCompletion(ctx, BindingCompletionCommand{Auth: c.Auth, ClientID: "complete-" + c.RequestID, RoomID: c.RoomID, RequestID: c.RequestID, ProofHash: c.ProofHash})
	if e != nil || !reflect.DeepEqual(accepted.BindingAuthority, resolved.BindingAuthority) {
		t.Fatalf("old receipt was retroactively denied by Owner state: %+v %v", resolved, e)
	}
	if _, e := b.f.store.AuthenticateBinding(ctx, c.Grant.TokenHash, b.enrollment.Origin); e != nil {
		t.Fatal("disabled Owner revoked Member Binding")
	}
	if _, e := b.f.store.RevokeDevice(ctx, b.member.Auth, "revoke-context-device", b.device.DeviceID); e != nil {
		t.Fatal(e)
	}
	if view, e := b.f.store.ReadBindingContext(ctx, b.device.Auth, c.RoomID); e == nil || view.RoomID != "" || view.Slots != nil {
		t.Fatalf("old Device context escaped live revoke check: %+v %v", view, e)
	}
}

func TestBindingAuthorityRejectsRehashedReceiptWithWrongApprovalOwner(t *testing.T) {
	for _, kind := range []string{bindingDecide, bindingComplete} {
		t.Run(kind, func(t *testing.T) {
			b := bindingPGFixture(t, nil)
			ctx := context.Background()
			c := bindingPGCommand(b, "receipt-owner", model.ActorSlot1, 1, false)
			completion := bindingPGApprove(t, b, c)
			principal, identifier := b.owner.Auth.UserID, "approve-"+c.RequestID
			if kind == bindingComplete {
				if _, e := b.f.store.CompleteBindingRequest(ctx, completion); e != nil {
					t.Fatal(e)
				}
				principal, identifier = c.Auth.UserID, completion.ClientID
			}
			var encoding []byte
			if e := b.f.admin.QueryRow(ctx, `SELECT result_encoding FROM binding_operations WHERE room_id=$1 AND principal=$2 AND op_kind=$3 AND identifier=$4`, c.RoomID, principal, kind, identifier).Scan(&encoding); e != nil {
				t.Fatal(e)
			}
			var original BindingAuthorityReceipt
			if e := json.Unmarshal(encoding, &original); e != nil {
				t.Fatal(e)
			}
			original.Request.ApprovedBy = b.member.Auth.UserID
			encoding, e := json.Marshal(original)
			if e != nil {
				t.Fatal(e)
			}
			digest := sha256.Sum256(encoding)
			// Deliberate corruption of this isolated test schema, retaining a valid
			// result hash, proves semantic validation beyond JSON/checksum validity.
			if _, e := b.f.admin.Exec(ctx, `ALTER TABLE binding_operations DISABLE TRIGGER binding_operations_immutable`); e != nil {
				t.Fatal(e)
			}
			if _, e := b.f.admin.Exec(ctx, `UPDATE binding_operations SET result_encoding=$5,result_hash=$6 WHERE room_id=$1 AND principal=$2 AND op_kind=$3 AND identifier=$4`, c.RoomID, principal, kind, identifier, encoding, digest[:]); e != nil {
				t.Fatal(e)
			}
			var r Result
			if kind == bindingComplete {
				r, e = b.f.store.ResolveBindingCompletion(ctx, completion)
			} else {
				r, e = b.f.store.ResolveBindingDecision(ctx, BindingDecisionCommand{Auth: b.owner.Auth, ClientID: identifier, RoomID: c.RoomID, RequestID: c.RequestID, Approve: true})
			}
			if e == nil || r.Class != ClassStoredFactUnreadable || r.BindingAuthority != nil || r.Fact.ID != "" {
				t.Fatalf("rehashed receipt falsely attributed Owner approval: %+v %v", r, e)
			}
		})
	}
}
