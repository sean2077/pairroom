package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func testEngine(t *testing.T) (*Engine, map[model.ActorID]Auth, string) {
	t.Helper()
	dir := t.TempDir()
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(Config{RoomID: "room", Store: log, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	auth := map[model.ActorID]Auth{}
	for _, slot := range model.SlotActors() {
		secret := "secret-" + string(slot)
		session := "session-" + string(slot)
		// bind associates immediately from the harness-provided official session id.
		b, err := e.Bind(slot, BindRequest{BindID: "bind-" + string(slot), CredentialHash: Digest(secret), SessionID: session})
		if err != nil {
			t.Fatal(err)
		}
		a := Auth{Slot: slot, BindID: b.BindID, Generation: b.Generation, SessionID: session, Secret: secret}
		// The Stop hook records the transcript path the environment does not carry.
		if _, err := e.ConfirmSession(a, session, "/optional/transcript"); err != nil {
			t.Fatal(err)
		}
		auth[slot] = a
	}
	return e, auth, dir
}
func TestNativeRoutingAndPublicationIdentity(t *testing.T) {
	e, a, _ := testEngine(t)
	sender := a[model.ActorSlot1]
	cases := []struct {
		text string
		to   model.ActorID
	}{
		{"@codex complete full reply\nline two", model.ActorSlot2},
		{"@USER human only", model.ActorUser}, {"@USER and @CoDeX: peer wins", model.ActorSlot2},
		{"finished, no relay", ""}, {"@claude self mention", ""}, {"```\n@codex\n```", ""}, {"https://example.com/@codex", ""}, {"inline `@codex` ignored", ""}, {"@codex-not-exact", ""},
	}
	for i, tc := range cases {
		p, err := e.Report(sender, uint64(i+1), tc.text)
		if err != nil {
			t.Fatal(err)
		}
		if tc.to == "" {
			if p.Message != nil {
				t.Fatalf("unexpected route for %q: %+v", tc.text, p)
			}
		} else if p.Message == nil || p.Message.To != tc.to || p.Message.Text != tc.text {
			t.Fatalf("lost routing/body: %+v", p)
		}
		again, err := e.Report(sender, uint64(i+1), "a changed retry body must not enqueue again")
		if err != nil {
			t.Fatal(err)
		}
		if (p.Message == nil) != (again.Message == nil) || p.Message != nil && p.Message.ID != again.Message.ID {
			t.Fatal("report idempotency failed")
		}
	}
	first, err := e.Send(sender, SendRequest{ID: "explicit-1", Text: "@user and @claude are body only"})
	if err != nil || first.To != model.ActorSlot2 {
		t.Fatalf("explicit default: %+v %v", first, err)
	}
	same, err := e.Send(sender, SendRequest{ID: "explicit-1", Text: first.Text})
	if err != nil || same.ID != first.ID {
		t.Fatal("send not idempotent")
	}
	different, err := e.Send(sender, SendRequest{ID: "explicit-2", Text: first.Text})
	if err != nil || different.ID == first.ID {
		t.Fatal("illegitimate content deduplication")
	}
	human, err := e.Send(sender, SendRequest{ID: "explicit-user", Text: "@codex ignored", To: model.ActorUser})
	if err != nil || human.To != model.ActorUser {
		t.Fatal("explicit user route failed")
	}
	auto, err := e.Report(sender, 10, "@codex same-turn complete visible response")
	if err != nil || auto.Message == nil || auto.Message.ID == different.ID {
		t.Fatal("dual publication silently merged")
	}
	logs, err := e.cfg.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range logs {
		if strings.Contains(string(ev.Data), "finished, no relay") {
			t.Fatal("non-routed private reply mirrored")
		}
	}
	// Public projections never contain long-lived credentials, hashes or claims.
	snapshot, _ := json.Marshal(e.Snapshot())
	for _, s := range []string{sender.Secret, Digest(sender.Secret), "credential_hash", "nonce_hash", "receipt"} {
		if strings.Contains(string(snapshot), s) {
			t.Fatalf("snapshot contains secret field %s", s)
		}
	}
}
func TestNativeFIFOClaimAckUnknownAndExplicitRetry(t *testing.T) {
	e, a, _ := testEngine(t)
	first, _ := e.Send(a[model.ActorSlot1], SendRequest{ID: "one", Text: "first"})
	second, _ := e.Send(a[model.ActorSlot1], SendRequest{ID: "two", Text: "second"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Claim(ctx, a[model.ActorSlot2], false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait claimed: %v", err)
	}
	if e.Snapshot().Messages[0].State != "queued" {
		t.Fatal("cancellation consumed queued work")
	}
	claim, err := e.Claim(context.Background(), a[model.ActorSlot2], true)
	if err != nil || claim.ID != first.ID || !strings.Contains(claim.Envelope, "first") {
		t.Fatalf("FIFO: %+v %v", claim, err)
	}
	events, _ := e.cfg.Store.Load()
	last := events[len(events)-1]
	if last.Kind != EventMessage || !strings.Contains(string(last.Data), `"state":"delivering"`) {
		t.Fatal("envelope escaped before durable claim")
	}
	if err := e.Cancel(first.ID); err == nil {
		t.Fatal("Cancel consumed already claimed work")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := e.Claim(ctx, a[model.ActorSlot2], false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second claim overtook first: %v", err)
	}
	if err := e.Ack(a[model.ActorSlot1], claim.ID, claim.Receipt); err == nil {
		t.Fatal("cross-slot acknowledgement accepted")
	}
	if err := e.Ack(a[model.ActorSlot2], claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	if err := e.Ack(a[model.ActorSlot2], claim.ID, claim.Receipt); err != nil {
		t.Fatal("ack lost response retry not idempotent")
	}
	if e.Snapshot().Messages[0].State != "handed_off" {
		t.Fatal("stdout receipt not terminal")
	}
	claim, err = e.Claim(context.Background(), a[model.ActorSlot2], false)
	if err != nil || claim.ID != second.ID {
		t.Fatal("second FIFO claim wrong")
	}
	e.mu.Lock()
	m := e.messages[second.ID]
	m.ClaimedAt = e.cfg.Now().Add(-2 * DeliveryLease)
	e.messages[m.ID] = m
	e.mu.Unlock()
	if err := e.Reap(); err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().Messages[1].State != "unknown" {
		t.Fatal("lease loss not unknown")
	}
	if err := e.Ack(a[model.ActorSlot2], claim.ID, "another-receipt"); err == nil {
		t.Fatal("late ack with a foreign receipt was accepted")
	}
	retry, err := e.Retry(second.ID)
	if err != nil || retry.ID == second.ID || retry.RetryOf != second.ID {
		t.Fatalf("explicit retry: %+v %v", retry, err)
	}
	if err := e.Ack(a[model.ActorSlot2], claim.ID, claim.Receipt); err == nil {
		t.Fatal("late ack accepted while an explicit Retry is pending")
	}
	if _, err := e.Retry(second.ID); err == nil {
		t.Fatal("duplicate pending retries")
	}
	if err := e.Cancel(retry.ID); err != nil {
		t.Fatal(err)
	}
	// With the Retry cancelled, the original claimer's receipt-matched late
	// acknowledgement is provable and settles the unknown delivery instead of
	// forcing a duplicate explicit re-send.
	if err := e.Ack(a[model.ActorSlot2], claim.ID, claim.Receipt); err != nil {
		t.Fatalf("proven late acknowledgement rejected: %v", err)
	}
	if e.Snapshot().Messages[1].State != "handed_off" {
		t.Fatal("proven late ack did not settle the unknown delivery")
	}
}
func TestNativeParkDisableTimeoutAndWake(t *testing.T) {
	e, a, _ := testEngine(t)
	receiver := a[model.ActorSlot2]
	// With no outstanding peer message, a Stop park returns at once instead of
	// holding the native harness for its whole window.
	start := time.Now()
	if claim, err := e.Claim(context.Background(), receiver, true); err != nil || claim != nil || time.Since(start) > time.Second {
		t.Fatalf("idle park held the harness: %v %v", claim, err)
	}
	if _, err := e.Send(receiver, SendRequest{ID: "question", Text: "a question for the peer"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := e.Claim(ctx, receiver, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	before := e.Snapshot().Sequence
	if err := e.Park(receiver.Slot, false); err != nil {
		t.Fatal(err)
	}
	message, _ := e.Send(a[model.ActorSlot1], SendRequest{ID: "wake", Text: "explicit send wakes identical inbox"})
	if claim, err := e.Claim(context.Background(), receiver, true); err != nil || claim != nil {
		t.Fatal("disabled park consumed message")
	}
	if e.Snapshot().Messages[1].State != "queued" {
		t.Fatal("park disable lost queued message")
	}
	claim, err := e.Claim(context.Background(), receiver, false)
	if err != nil || claim.ID != message.ID {
		t.Fatal("foreground fallback did not work")
	}
	_ = e.Ack(receiver, claim.ID, claim.Receipt)
	if err := e.Park(receiver.Slot, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(receiver, SendRequest{ID: "follow-up", Text: "another question"}); err != nil {
		t.Fatal(err)
	}
	result := make(chan *Claim, 1)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	go func() { c, _ := e.Claim(ctx2, receiver, true); result <- c }()
	_, err = e.Send(a[model.ActorSlot1], SendRequest{ID: "after-park", Text: "a new delivery"})
	if err != nil {
		t.Fatal(err)
	}
	if c := <-result; c == nil {
		t.Fatal("park did not wake")
	}
	if before != 5 {
		t.Fatalf("timed-out wait persisted fake activity/delivery: seq=%d", before)
	}
}

// A peer reply settles the expectation: the next Stop does not park again
// until this slot addresses its peer, and a fresh request re-arms it.
func TestNativeParkWaitsOnlyForAnOutstandingPeerReply(t *testing.T) {
	e, a, _ := testEngine(t)
	asker, peer := a[model.ActorSlot1], a[model.ActorSlot2]
	idle := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		claim, err := e.Claim(ctx, asker, true)
		return claim == nil && err == nil
	}
	if !idle() {
		t.Fatal("park waited without an outstanding request")
	}
	if _, err := e.Send(asker, SendRequest{ID: "q1", Text: "question"}); err != nil {
		t.Fatal(err)
	}
	if idle() {
		t.Fatal("park did not wait for an outstanding reply")
	}
	if _, err := e.Send(peer, SendRequest{ID: "a1", Text: "answer"}); err != nil {
		t.Fatal(err)
	}
	claim, err := e.Claim(context.Background(), asker, true)
	if err != nil || claim == nil {
		t.Fatalf("queued reply not collected by park: %v", err)
	}
	if err := e.Ack(asker, claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	if !idle() {
		t.Fatal("park kept waiting after the reply arrived")
	}
	// Neither human input nor an @user escalation arms a park: the human is
	// usually in front of the harness and must not wait behind a Stop hook.
	if _, err := e.SendUser(SendRequest{ID: "human", Text: "human steer", To: model.ActorSlot2}); err != nil {
		t.Fatal(err)
	}
	if !idle() {
		t.Fatal("human input to the peer armed this slot's park")
	}
	if _, err := e.Send(asker, SendRequest{ID: "ask-human", Text: "which option?", To: model.ActorUser}); err != nil {
		t.Fatal(err)
	}
	if !idle() {
		t.Fatal("an @user escalation held the harness")
	}
	// Human input to this slot does not settle an outstanding peer request.
	if _, err := e.Send(asker, SendRequest{ID: "q-pending", Text: "peer question"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SendUser(SendRequest{ID: "human-steer", Text: "also consider X", To: model.ActorSlot1}); err != nil {
		t.Fatal(err)
	}
	if claim, err = e.Claim(context.Background(), asker, true); err != nil || claim == nil {
		t.Fatalf("human input not collected by park: %v", err)
	}
	if err := e.Ack(asker, claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	if idle() {
		t.Fatal("human input cleared the outstanding peer request")
	}
	if _, err := e.Send(peer, SendRequest{ID: "a-pending", Text: "peer answer"}); err != nil {
		t.Fatal(err)
	}
	if claim, err = e.Claim(context.Background(), asker, true); err != nil || claim == nil {
		t.Fatalf("peer answer not collected by park: %v", err)
	}
	if err := e.Ack(asker, claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	cancelled, err := e.Send(asker, SendRequest{ID: "q2", Text: "second question"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Cancel(cancelled.ID); err != nil {
		t.Fatal(err)
	}
	if !idle() {
		t.Fatal("cancelled request still armed park")
	}
	if _, err := e.Send(asker, SendRequest{ID: "q3", Text: "third question"}); err != nil {
		t.Fatal(err)
	}
	e.cfg.Now = func() time.Time { return time.Now().UTC().Add(ReplyParkWindow + time.Minute) }
	if !idle() {
		t.Fatal("stale request kept arming park")
	}
	if ready, err := e.WaitForPending(context.Background(), asker); ready || err != nil {
		t.Fatalf("readiness probe waited without an expected reply: %v %v", ready, err)
	}
}

func TestNativeDrainAcknowledgementRetainsAuthentication(t *testing.T) {
	e, auth, _ := testEngine(t)
	receiver := auth[model.ActorSlot2]
	if _, err := e.Send(auth[model.ActorSlot1], SendRequest{ID: "drain", Text: "work"}); err != nil {
		t.Fatal(err)
	}
	claim, err := e.Claim(context.Background(), receiver, false)
	if err != nil {
		t.Fatal(err)
	}
	e.SetDraining(true)
	wrongGeneration := receiver
	wrongGeneration.Generation++
	for _, a := range []Auth{auth[model.ActorSlot1], wrongGeneration} {
		if err := e.Ack(a, claim.ID, claim.Receipt); !errors.Is(err, ErrAuth) {
			t.Fatalf("invalid draining ack authorized: %v", err)
		}
	}
	if err := e.Ack(receiver, claim.ID, "wrong-receipt"); !errors.Is(err, ErrAuth) {
		t.Fatal(err)
	}
	if _, err := e.Claim(context.Background(), receiver, false); !errors.Is(err, ErrClosed) {
		t.Fatalf("drain allowed claim: %v", err)
	}
	if err := e.Ack(receiver, claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	if err := e.Ack(receiver, claim.ID, claim.Receipt); err != nil {
		t.Fatalf("ack retry not idempotent: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Ack(receiver, claim.ID, claim.Receipt); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed engine accepted ack: %v", err)
	}
}
func TestNativeBindingAssociationAndRevocation(t *testing.T) {
	e, a, _ := testEngine(t)
	slot := model.ActorSlot1
	old := a[slot]
	// A different official session cannot take an occupied slot without replace.
	if _, err := e.Bind(slot, BindRequest{BindID: "new-bind", CredentialHash: Digest("new-secret"), SessionID: "intruder-session"}); !errors.Is(err, ErrOccupied) {
		t.Fatal("occupied slot not protected")
	}
	// bind requires the official session id captured from the harness environment.
	if _, err := e.Bind(slot, BindRequest{BindID: "new-bind", CredentialHash: Digest("new-secret"), Replace: true}); err == nil {
		t.Fatal("bind without a session id was accepted")
	}
	// The same session recovers idempotently without consuming a generation.
	same, err := e.Bind(slot, BindRequest{BindID: old.BindID, CredentialHash: Digest(old.Secret), SessionID: old.SessionID})
	if err != nil || same.Generation != old.Generation {
		t.Fatal("same session did not recover idempotently")
	}
	_, _ = e.Send(a[model.ActorSlot2], SendRequest{ID: "old-target", Text: "cannot cross generations"})
	replacement, err := e.Bind(slot, BindRequest{BindID: "new-bind", CredentialHash: Digest("new-secret"), SessionID: "new-session", Replace: true})
	if err != nil || replacement.Generation != old.Generation+1 {
		t.Fatal("replace generation failed")
	}
	if _, err := e.Report(old, 1, "@codex stale"); !errors.Is(err, ErrAuth) {
		t.Fatal("old generation remained authorized")
	}
	if e.Snapshot().Messages[0].State != "cancelled" {
		t.Fatal("old-generation queued work survived replacement")
	}
	// The replacement is associated at bind, so it authenticates and claims at once.
	next := Auth{Slot: slot, BindID: replacement.BindID, Generation: replacement.Generation, Secret: "new-secret", SessionID: "new-session"}
	if _, err := e.ConfirmSession(next, "another-session", ""); err == nil {
		t.Fatal("mismatched hook session was confirmed")
	}
	if _, err := e.ConfirmSession(next, next.SessionID, "/transcript"); err != nil {
		t.Fatal(err)
	}
	if err := e.UnbindAs(old); err == nil {
		t.Fatal("old credentials revoked replacement")
	}
	if err := e.UnbindAs(next); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Bind(slot, BindRequest{BindID: next.BindID, CredentialHash: Digest(next.Secret), SessionID: "new-session"}); err == nil {
		t.Fatal("revoked ID reused")
	}
}
func TestNativeRestartPersistsQueueReceiptsGapsAndUnknown(t *testing.T) {
	e, a, dir := testEngine(t)
	published, err := e.Report(a[model.ActorSlot1], 3, "@codex observed gap")
	if err != nil || published.GapFrom != 1 || published.GapTo != 2 {
		t.Fatalf("gap %+v %v", published, err)
	}
	_, _ = e.Send(a[model.ActorSlot1], SendRequest{ID: "queued-restart", Text: "safe queued"})
	claim, err := e.Claim(context.Background(), a[model.ActorSlot2], false)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate process loss: close only the writer, not the Engine shutdown path.
	if err := e.cfg.Store.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := store.OpenExisting(dir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(Config{RoomID: "room", Store: log, Runtimes: e.cfg.Runtimes})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	snapshot := fresh.Snapshot()
	if snapshot.Messages[0].State != "unknown" || snapshot.Messages[1].State != "queued" {
		t.Fatalf("recovery states %+v", snapshot.Messages)
	}
	if _, accepted, err := fresh.Publication(a[model.ActorSlot1], 3); err != nil || !accepted {
		t.Fatal("publication receipt lost")
	}
	again, err := fresh.Report(a[model.ActorSlot1], 3, "@codex same receipt")
	if err != nil || again.Message.ID != claim.ID {
		t.Fatal("restart duplicated accepted publication")
	}
	got, err := fresh.Claim(context.Background(), a[model.ActorSlot2], false)
	if err != nil || got.ID != snapshot.Messages[1].ID {
		t.Fatal("unknown was automatically replayed")
	}
}
func TestNativeConcurrentSendPreservesDurableFIFO(t *testing.T) {
	e, a, _ := testEngine(t)
	const count = 32
	var wg sync.WaitGroup
	errCh := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := e.Send(a[model.ActorSlot1], SendRequest{ID: fmt.Sprintf("c-%d", i), Text: "same body valid across sends"})
			if err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	snapshot := e.Snapshot()
	if len(snapshot.Messages) != count {
		t.Fatal("concurrent sends lost or deduplicated")
	}
	for _, m := range snapshot.Messages {
		c, err := e.Claim(context.Background(), a[model.ActorSlot2], false)
		if err != nil || c.ID != m.ID {
			t.Fatal("durable order changed")
		}
		if err := e.Ack(a[model.ActorSlot2], c.ID, c.Receipt); err != nil {
			t.Fatal(err)
		}
	}
}
func TestNativeAppendFailureNeverPublishesOrHandsOut(t *testing.T) {
	e, a, dir := testEngine(t)
	before := e.Snapshot().Sequence
	if err := e.cfg.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Report(a[model.ActorSlot1], 1, "@codex unsafe publication"); err == nil {
		t.Fatal("closed writer accepted publication")
	}
	if e.Snapshot().Sequence != before || len(e.Snapshot().Messages) != 0 {
		t.Fatal("unpersisted publication projected as fact")
	}
	data, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "unsafe publication") {
		t.Fatal("failure wrote fake fact")
	}
}
func TestNativeDuplicateRuntimeHandlesAndPeerMetadata(t *testing.T) {
	e, a, _ := testEngine(t)
	e.cfg.Runtimes[model.ActorSlot1] = model.RuntimeCodex
	p, err := e.Report(a[model.ActorSlot1], 1, "@codex1 exact duplicate runtime peer")
	if err != nil || p.Message == nil || p.Message.To != model.ActorSlot2 {
		t.Fatal("stable duplicate suffix routing failed")
	}
	p, err = e.Report(a[model.ActorSlot1], 2, "@codex legacy bare name should not route")
	if err != nil || p.Message != nil {
		t.Fatal("bare duplicate runtime routed")
	}
	peer, err := e.Peer(a[model.ActorSlot1])
	if err != nil || peer.SessionID != a[model.ActorSlot2].SessionID || peer.TranscriptPath == "" {
		t.Fatal("peer metadata missing")
	}
	claim, err := e.Claim(context.Background(), a[model.ActorSlot2], false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(claim.Envelope, peer.SessionID) || strings.Contains(claim.Envelope, "/optional/transcript") {
		t.Fatal("session metadata polluted envelope")
	}
}
