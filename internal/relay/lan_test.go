package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func lanEngine(t *testing.T, now func() time.Time) (*Engine, Auth, string) {
	t.Helper()
	dir := t.TempDir()
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(Config{RoomID: "room", Store: log, SharedSlot: model.ActorSlot2, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeAwaitingPeer}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	b, err := e.Bind(model.ActorSlot1, BindRequest{BindID: "local-bind", CredentialHash: Digest("local-secret"), SessionID: "official-local-session"})
	if err != nil {
		t.Fatal(err)
	}
	return e, Auth{Slot: b.Slot, BindID: b.BindID, Generation: b.Generation, SessionID: "official-local-session", Secret: "local-secret"}, dir
}
func admitLAN(t *testing.T, e *Engine, key string) Auth {
	t.Helper()
	invite, err := e.CreateLANInvite()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.RequestLANJoin(LANJoinRequest{InviteID: invite.ID, RequestID: "request-" + key[:8], Key: key, Runtime: model.RuntimeClaude})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.AcceptLANJoin("request-"+key[:8], key); err != nil {
		t.Fatal(err)
	}
	a, err := e.LANAuth(key)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestLANAdmissionSelectsRealPeerOnceAndIsAtomic(t *testing.T) {
	e, owner, _ := lanEngine(t, nil)
	if _, err := e.Bind(model.ActorSlot2, BindRequest{BindID: "forged-local", CredentialHash: Digest("x"), SessionID: "x"}); err == nil {
		t.Fatal("remote slot accepted local bind")
	}
	p, err := e.Report(owner, 1, "@codex no selected peer yet")
	if err != nil || p.Message != nil {
		t.Fatalf("fabricated pending peer route: %+v %v", p, err)
	}
	queued, err := e.Send(owner, SendRequest{ID: "before-join", Text: "reproduction steps"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.CreateLANInvite()
	if err != nil {
		t.Fatal(err)
	}
	keyA, keyB := Digest("peer-a"), Digest("peer-b")
	for id, key := range map[string]string{"a": keyA, "b": keyB} {
		if _, _, err = e.RequestLANJoin(LANJoinRequest{InviteID: v.ID, RequestID: id, Key: key, Runtime: model.RuntimeClaude}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = e.LANAuth(keyA); !errors.Is(err, ErrAuth) {
		t.Fatal("pending request obtained membership")
	}
	if _, err = e.AcceptLANJoin("a", keyB); !errors.Is(err, ErrAuth) {
		t.Fatal("mismatched public receipt accepted")
	}
	var wg sync.WaitGroup
	success := make(chan Binding, 2)
	start := make(chan struct{})
	for id, key := range map[string]string{"a": keyA, "b": keyB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b, err := e.AcceptLANJoin(id, key)
			if err == nil {
				success <- b
			}
		}()
	}
	close(start)
	wg.Wait()
	close(success)
	var winner Binding
	count := 0
	for b := range success {
		count++
		winner = b
	}
	if count != 1 {
		t.Fatalf("admitted %d peers", count)
	}
	if got := e.Runtimes()[model.ActorSlot2]; got != model.RuntimeClaude {
		t.Fatalf("actual peer runtime lost: %s", got)
	}
	ids := model.ParticipantIdentities(e.Runtimes())
	if ids[model.ActorSlot1].MentionHandle != "@claude0" || ids[model.ActorSlot2].MentionHandle != "@claude1" {
		t.Fatalf("runtime handles not finalized: %+v", ids)
	}
	a, err := e.LANAuth(winner.RemoteKey)
	if err != nil {
		t.Fatal(err)
	}
	head, err := e.PrepareHead(context.Background(), a, false)
	if err != nil || head == nil || head.Message.ID != queued.ID {
		t.Fatalf("pre-admission work not preserved: %+v %v", head, err)
	}
}
func TestLANPrepareBeforeLeaseRestartAndOriginalReceiptRecovery(t *testing.T) {
	now := time.Now().UTC()
	e, owner, dir := lanEngine(t, func() time.Time { return now })
	a := admitLAN(t, e, Digest("peer"))
	m, err := e.Send(owner, SendRequest{ID: "slow-file", Text: "shared evidence"})
	if err != nil {
		t.Fatal(err)
	}
	head, err := e.PrepareHead(context.Background(), a, false)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute) // downloading/preparing is not a delivery claim
	if state := e.Snapshot().Messages[0].State; state != "queued" {
		t.Fatalf("preparation consumed work: %s", state)
	}
	claim, err := e.ClaimPrepared(context.Background(), a, head.Message.ID, head.Digest, false)
	if err != nil || claim == nil {
		t.Fatalf("late prepared claim: %+v %v", claim, err)
	}
	if claim.ID != m.ID || claim.Message.Text != m.Text {
		t.Fatal("claim content changed")
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(Config{RoomID: "room", Store: log, SharedSlot: model.ActorSlot2, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeAwaitingPeer}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if state := restarted.Snapshot().Messages[0].State; state != "unknown" {
		t.Fatalf("restart automatically replayed claimed work: %s", state)
	}
	if err = restarted.Ack(a, claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	if state := restarted.Snapshot().Messages[0].State; state != "handed_off" {
		t.Fatalf("original receipt could not settle: %s", state)
	}
	j := restarted.LANState().Member
	_, status, err := restarted.LANJoinStatus(j.RequestID, a.MemberKey)
	if err != nil || status != "accepted" {
		t.Fatal("membership did not survive restart")
	}
}
func TestLANRevocationCancelsWaitAndSeparatesHumanPrincipals(t *testing.T) {
	e, owner, _ := lanEngine(t, nil)
	a := admitLAN(t, e, Digest("peer"))
	local, err := e.SendUser(SendRequest{ID: "human-id", To: model.ActorSlot1, Text: "local human"})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := e.SendLANUser(a, SendRequest{ID: "human-id", To: model.ActorSlot1, Text: "remote human"})
	if err != nil {
		t.Fatal(err)
	}
	if local.ID == remote.ID || local.Author != "host_owner" || remote.Author != "lan:"+a.MemberKey {
		t.Fatal("human provenance or idempotency principal collapsed")
	}
	claim, err := e.Claim(context.Background(), owner, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Ack(owner, claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	claim, err = e.Claim(context.Background(), owner, false)
	if err != nil || !strings.Contains(claim.Envelope, "remote Room owner") || !strings.Contains(claim.Envelope, "do not grant local native permissions") {
		t.Fatalf("remote human gained local authority: %+v %v", claim, err)
	}
	waiting := make(chan error, 1)
	go func() { _, err := e.PrepareHead(context.Background(), a, false); waiting <- err }()
	if err = e.RevokeLANMember(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waiting:
		if !errors.Is(err, ErrAuth) {
			t.Fatalf("revoked wait: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("revocation left a collector blocked")
	}
	if _, err = e.Send(a, SendRequest{ID: "after", Text: "forbidden"}); !errors.Is(err, ErrAuth) {
		t.Fatal("revoked certificate retained effects")
	}
	forged := owner
	forged.Slot = model.ActorSlot2
	forged.BindID = a.BindID
	forged.Generation = a.Generation
	if _, err = e.Inspect(forged); !errors.Is(err, ErrAuth) {
		t.Fatal("local relay secret impersonated remote slot")
	}
}
func TestLANWakeReservationCannotReplayAndFencesGeneration(t *testing.T) {
	e, owner, _ := lanEngine(t, nil)
	a := admitLAN(t, e, Digest("peer"))
	m, err := e.Send(owner, SendRequest{ID: "wake", Text: "wake"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.LANWakeCandidate(a, m.ID)
	if err != nil || c == nil || c.SessionID != "" || !c.Remote {
		t.Fatalf("wake leaked local session or lost remote provenance: %+v %v", c, err)
	}
	if err = e.ReserveLANWake(a, m.ID); err != nil {
		t.Fatal(err)
	}
	if err = e.ReserveLANWake(a, m.ID); !errors.Is(err, ErrWakeReserved) {
		t.Fatal("wake grant replayed")
	}
	if err = e.RecordLANWake(a, "", "submitted", ""); err == nil {
		t.Fatal("wake outcome without original reservation ID accepted")
	}
	if err = e.RecordLANWake(a, m.ID, "submitted", ""); err != nil {
		t.Fatal(err)
	}
	before := e.Sequence()
	if err = e.RecordLANWake(a, m.ID, "submitted", ""); err != nil || e.Sequence() != before {
		t.Fatal("lost wake outcome response was not idempotent")
	}
	if observation := e.Summary().LastWake[a.Slot]; observation.MessageID != m.ID {
		t.Fatal("LAN wake outcome lost exact message correlation")
	}
	if err = e.RevokeLANMember(); err != nil {
		t.Fatal(err)
	}
	if err = e.RecordLANWake(a, m.ID, "submitted", ""); !errors.Is(err, ErrAuth) {
		t.Fatal("revoked generation retained wake effect authority")
	}
}

func TestLANOwnerCommandsFenceReplacedLocalSession(t *testing.T) {
	e, old, _ := lanEngine(t, nil)
	if _, err := e.Bind(old.Slot, BindRequest{BindID: "new-owner", CredentialHash: Digest("new-secret"), SessionID: "new-official-session", Replace: true}); err != nil {
		t.Fatal(err)
	}
	before := e.Sequence()
	if _, err := e.CreateLANInvite(old); !errors.Is(err, ErrAuth) {
		t.Fatal("replaced native session issued a LAN invitation")
	}
	if err := e.RevokeLANMember(old); !errors.Is(err, ErrAuth) {
		t.Fatal("replaced native session exercised owner authority")
	}
	if e.Sequence() != before {
		t.Fatal("rejected owner operations changed durable state")
	}
}

func TestLANConditionalClaimCannotConsumeUnpreparedSuccessor(t *testing.T) {
	e, owner, _ := lanEngine(t, nil)
	a := admitLAN(t, e, Digest("peer"))
	first, err := e.Send(owner, SendRequest{ID: "first", Text: "prepared"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.Send(owner, SendRequest{ID: "second", Text: "not prepared"})
	if err != nil {
		t.Fatal(err)
	}
	head, err := e.PrepareHead(context.Background(), a, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Cancel(first.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := e.ClaimPrepared(context.Background(), a, head.Message.ID, head.Digest, false)
	if err != nil || claim != nil {
		t.Fatalf("stale preparation consumed successor: %+v %v", claim, err)
	}
	head, err = e.PrepareHead(context.Background(), a, false)
	if err != nil || head == nil || head.Message.ID != second.ID || head.Message.State != "queued" {
		t.Fatal("successor was not left queued")
	}
}

// Retry republishes for the original author. Dropping the authenticated human
// provenance would downgrade the rebuilt envelope's @user handle and silently
// remove the shared-Room permissions notice.
func TestLANRetryKeepsAuthenticatedHumanProvenance(t *testing.T) {
	now := time.Now().UTC()
	e, owner, _ := lanEngine(t, func() time.Time { return now })
	a := admitLAN(t, e, Digest("peer"))
	sent, err := e.SendLANUser(a, SendRequest{ID: "remote-human", To: model.ActorSlot1, Text: "remote request"})
	if err != nil || sent.Author != "lan:"+a.MemberKey {
		t.Fatalf("remote human send: %+v %v", sent, err)
	}
	claim, err := e.Claim(context.Background(), owner, false)
	if err != nil || claim == nil || !strings.Contains(claim.Envelope, "remote Room owner") {
		t.Fatalf("first claim lost remote provenance: %+v %v", claim, err)
	}
	now = now.Add(e.cfg.Lease + time.Second)
	if err := e.Reap(); err != nil {
		t.Fatal(err)
	}
	retried, err := e.Retry(sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Author != sent.Author || retried.RetryOf != sent.ID {
		t.Fatalf("retry lost the authenticated author: %+v", retried)
	}
	claim, err = e.Claim(context.Background(), owner, false)
	if err != nil || claim == nil || !strings.Contains(claim.Envelope, "remote Room owner") || !strings.Contains(claim.Envelope, "do not grant local native permissions") {
		t.Fatalf("retried envelope lost shared-Room provenance: %+v %v", claim, err)
	}
}

// A park toggle appends its binding fact directly. Running the Registry
// CommitBinding hook for it would let one checkpoint write failure poison the
// whole Registry fail-closed, although a park toggle changes no binding
// identity.
func TestParkAppendsItsFactWithoutRunningTheCommitBindingHook(t *testing.T) {
	log, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtimes := map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeAwaitingPeer}
	calls := 0
	e, err := Open(Config{RoomID: "room", Store: log, SharedSlot: model.ActorSlot2, Runtimes: runtimes, CommitBinding: func(_ Binding, appendFact func() error) error {
		calls++
		return appendFact()
	}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Bind(model.ActorSlot1, BindRequest{BindID: "local-bind", CredentialHash: Digest("local-secret"), SessionID: "official-local-session"})
	if err != nil || calls != 1 {
		t.Fatalf("bind hook calls=%d: %+v %v", calls, b, err)
	}
	a := admitLAN(t, e, Digest("peer"))
	before := calls
	if err := e.Park(model.ActorSlot1, false); err != nil || calls != before {
		t.Fatalf("owner park ran the commit hook (calls=%d): %v", calls, err)
	}
	if err := e.ParkAs(a, false); err != nil || calls != before {
		t.Fatalf("member park ran the commit hook (calls=%d): %v", calls, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(Config{RoomID: "room", Store: log, SharedSlot: model.ActorSlot2, Runtimes: runtimes})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if owner := restarted.bindings[model.ActorSlot1]; owner.ParkEnabled || !owner.Active {
		t.Fatalf("owner park fact was not durable: %+v", owner)
	}
	if member := restarted.bindings[model.ActorSlot2]; member.ParkEnabled || !member.Active || member.RemoteKey == "" {
		t.Fatalf("member park fact was not durable: %+v", member)
	}
}

// A key that retries join (new request IDs while its invitation stays live) must
// not fill the derived request projection or block another colleague, and a
// superseded member must read the retirement answer rather than a pending
// expiry no retry can resolve. Revocation also frees the peer Runtime selection
// for its successor.
func TestLANJoinRetriesStayBoundedAndRevocationFreesTheRuntime(t *testing.T) {
	e, _, _ := lanEngine(t, nil)
	invite, err := e.CreateLANInvite()
	if err != nil {
		t.Fatal(err)
	}
	stuck := Digest("stuck-peer")
	for i := 0; i < 8; i++ {
		if _, status, err := e.RequestLANJoin(LANJoinRequest{InviteID: invite.ID, RequestID: fmt.Sprintf("stuck-request-%d", i), Key: stuck, Runtime: model.RuntimeCodex}); err != nil || status != "pending" {
			t.Fatalf("retry %d: %q %v", i, status, err)
		}
	}
	if len(e.lanRequests) != 1 {
		t.Fatalf("one key's retries kept %d requests", len(e.lanRequests))
	}
	// The durable per-certificate budget stops this holder from appending
	// unbounded join facts with fresh request IDs.
	if _, _, err := e.RequestLANJoin(LANJoinRequest{InviteID: invite.ID, RequestID: "stuck-request-8", Key: stuck, Runtime: model.RuntimeCodex}); err == nil {
		t.Fatal("a certificate spent more than its join attempts")
	}
	// A different colleague can still request against the same live invitation.
	second := Digest("second-peer")
	if _, status, err := e.RequestLANJoin(LANJoinRequest{InviteID: invite.ID, RequestID: "second-request", Key: second, Runtime: model.RuntimeClaude}); err != nil || status != "pending" {
		t.Fatalf("second key after retries: %q %v", status, err)
	}
	if _, err := e.AcceptLANJoin("stuck-request-7", stuck); err != nil {
		t.Fatal(err)
	}
	if kind := e.Runtimes()[model.ActorSlot2]; kind != model.RuntimeCodex {
		t.Fatalf("admitted runtime = %s", kind)
	}
	if err := e.RevokeLANMember(); err != nil {
		t.Fatal(err)
	}
	if kind := e.Runtimes()[model.ActorSlot2]; kind != model.RuntimeAwaitingPeer {
		t.Fatalf("revoked slot kept a runtime: %s", kind)
	}
	// The successor selects its own runtime from a fresh invitation.
	next, err := e.CreateLANInvite()
	if err != nil {
		t.Fatal(err)
	}
	if _, status, err := e.RequestLANJoin(LANJoinRequest{InviteID: next.ID, RequestID: "successor-request", Key: second, Runtime: model.RuntimeGrok}); err != nil || status != "pending" {
		t.Fatalf("successor request: %q %v", status, err)
	}
	if _, err := e.AcceptLANJoin("successor-request", second); err != nil {
		t.Fatalf("successor admission: %v", err)
	}
	if kind := e.Runtimes()[model.ActorSlot2]; kind != model.RuntimeGrok {
		t.Fatalf("successor runtime = %s", kind)
	}
	// The superseded member's own request reads the explicit retirement answer.
	if _, status, err := e.LANJoinStatus("stuck-request-7", stuck); err != nil || status != "revoked" {
		t.Fatalf("superseded member status = %q %v", status, err)
	}
}
