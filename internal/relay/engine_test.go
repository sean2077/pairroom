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
	e, err := Open(Config{RoomID: "room", Store: log, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorClaude: model.RuntimeClaude, model.ActorCodex: model.RuntimeCodex}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	auth := map[model.ActorID]Auth{}
	for _, slot := range model.SlotActors() {
		secret := "secret-" + string(slot)
		nonce := "nonce-" + string(slot)
		b, err := e.Bind(slot, BindRequest{BindID: "bind-" + string(slot), CredentialHash: Digest(secret), NonceHash: Digest(nonce)})
		if err != nil {
			t.Fatal(err)
		}
		a := Auth{Slot: slot, BindID: b.BindID, Generation: b.Generation, SessionID: "session-" + string(slot), Secret: secret}
		if _, err := e.Associate(a, nonce, a.SessionID, "/optional/transcript"); err != nil {
			t.Fatal(err)
		}
		auth[slot] = a
	}
	return e, auth, dir
}
func TestNativeRoutingAndPublicationIdentity(t *testing.T) {
	e, a, _ := testEngine(t)
	sender := a[model.ActorClaude]
	cases := []struct {
		text string
		to   model.ActorID
	}{
		{"@codex complete full reply\nline two", model.ActorCodex},
		{"@USER human only", model.ActorUser}, {"@USER and @CoDeX: peer wins", model.ActorCodex},
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
	if err != nil || first.To != model.ActorCodex {
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
	first, _ := e.Send(a[model.ActorClaude], SendRequest{ID: "one", Text: "first"})
	second, _ := e.Send(a[model.ActorClaude], SendRequest{ID: "two", Text: "second"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Claim(ctx, a[model.ActorCodex], false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait claimed: %v", err)
	}
	if e.Snapshot().Messages[0].State != "queued" {
		t.Fatal("cancellation consumed queued work")
	}
	claim, err := e.Claim(context.Background(), a[model.ActorCodex], true)
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
	if _, err := e.Claim(ctx, a[model.ActorCodex], false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second claim overtook first: %v", err)
	}
	if err := e.Ack(a[model.ActorClaude], claim.ID, claim.Receipt); err == nil {
		t.Fatal("cross-slot acknowledgement accepted")
	}
	if err := e.Ack(a[model.ActorCodex], claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	if err := e.Ack(a[model.ActorCodex], claim.ID, claim.Receipt); err != nil {
		t.Fatal("ack lost response retry not idempotent")
	}
	if e.Snapshot().Messages[0].State != "handed_off" {
		t.Fatal("stdout receipt not terminal")
	}
	claim, err = e.Claim(context.Background(), a[model.ActorCodex], false)
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
	if err := e.Ack(a[model.ActorCodex], claim.ID, claim.Receipt); err == nil {
		t.Fatal("late ack turned unknown into certainty")
	}
	if e.Snapshot().Messages[1].State != "unknown" {
		t.Fatal("lease loss not unknown")
	}
	retry, err := e.Retry(second.ID)
	if err != nil || retry.ID == second.ID || retry.RetryOf != second.ID {
		t.Fatalf("explicit retry: %+v %v", retry, err)
	}
	if _, err := e.Retry(second.ID); err == nil {
		t.Fatal("duplicate pending retries")
	}
	if err := e.Cancel(retry.ID); err != nil {
		t.Fatal(err)
	}
}
func TestNativeParkDisableTimeoutAndWake(t *testing.T) {
	e, a, _ := testEngine(t)
	receiver := a[model.ActorCodex]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := e.Claim(ctx, receiver, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	before := e.Snapshot().Sequence
	if err := e.Park(receiver.Slot, false); err != nil {
		t.Fatal(err)
	}
	message, _ := e.Send(a[model.ActorClaude], SendRequest{ID: "wake", Text: "explicit send wakes identical inbox"})
	if claim, err := e.Claim(context.Background(), receiver, true); err != nil || claim != nil {
		t.Fatal("disabled park consumed message")
	}
	if e.Snapshot().Messages[0].State != "queued" {
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
	result := make(chan *Claim, 1)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	go func() { c, _ := e.Claim(ctx2, receiver, true); result <- c }()
	_, err = e.Send(a[model.ActorClaude], SendRequest{ID: "after-park", Text: "a new delivery"})
	if err != nil {
		t.Fatal(err)
	}
	if c := <-result; c == nil {
		t.Fatal("park did not wake")
	}
	if before != 4 {
		t.Fatalf("timed-out wait persisted fake activity/delivery: seq=%d", before)
	}
}
func TestNativeBindingAssociationRevocationAndNonce(t *testing.T) {
	e, a, _ := testEngine(t)
	slot := model.ActorClaude
	old := a[slot]
	if _, err := e.Associate(old, "nonce-claude", old.SessionID, ""); !errors.Is(err, ErrNonce) {
		t.Fatal("nonce replay accepted")
	}
	if _, err := e.Bind(slot, BindRequest{BindID: "new-bind", CredentialHash: Digest("new-secret"), NonceHash: Digest("new-nonce")}); !errors.Is(err, ErrOccupied) {
		t.Fatal("occupied slot not protected")
	}
	same, err := e.Bind(slot, BindRequest{BindID: old.BindID, CredentialHash: Digest(old.Secret), NonceHash: Digest("consumed"), SessionID: old.SessionID})
	if err != nil || same.Generation != old.Generation {
		t.Fatal("same session did not recover idempotently")
	}
	_, _ = e.Send(a[model.ActorCodex], SendRequest{ID: "old-target", Text: "cannot cross generations"})
	replacement, err := e.Bind(slot, BindRequest{BindID: "new-bind", CredentialHash: Digest("new-secret"), NonceHash: Digest("new-nonce"), Replace: true})
	if err != nil || replacement.Generation != old.Generation+1 {
		t.Fatal("replace generation failed")
	}
	if _, err := e.Report(old, 1, "@codex stale"); !errors.Is(err, ErrAuth) {
		t.Fatal("old generation remained authorized")
	}
	if e.Snapshot().Messages[0].State != "cancelled" {
		t.Fatal("old-generation queued work survived replacement")
	}
	next := Auth{Slot: slot, BindID: replacement.BindID, Generation: replacement.Generation, Secret: "new-secret", SessionID: "new-session"}
	if _, err := e.Claim(context.Background(), next, true); !errors.Is(err, ErrAuth) {
		t.Fatal("unassociated session could claim")
	}
	if _, err := e.Associate(next, "missing", next.SessionID, ""); !errors.Is(err, ErrNonce) {
		t.Fatal("nonmatching nonce associated")
	}
	if _, err := e.Associate(next, "new-nonce", "another-session", ""); err == nil {
		t.Fatal("payload and presented official identity mismatch accepted")
	}
	if _, err := e.Associate(next, "new-nonce", next.SessionID, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.UnbindAs(old); err == nil {
		t.Fatal("old credentials revoked replacement")
	}
	if err := e.UnbindAs(next); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Bind(slot, BindRequest{BindID: next.BindID, CredentialHash: Digest(next.Secret), NonceHash: Digest("nonce")}); err == nil {
		t.Fatal("revoked ID reused")
	}
}
func TestNativeRestartPersistsQueueReceiptsGapsAndUnknown(t *testing.T) {
	e, a, dir := testEngine(t)
	published, err := e.Report(a[model.ActorClaude], 3, "@codex observed gap")
	if err != nil || published.GapFrom != 1 || published.GapTo != 2 {
		t.Fatalf("gap %+v %v", published, err)
	}
	_, _ = e.Send(a[model.ActorClaude], SendRequest{ID: "queued-restart", Text: "safe queued"})
	claim, err := e.Claim(context.Background(), a[model.ActorCodex], false)
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
	if _, accepted, err := fresh.Publication(a[model.ActorClaude], 3); err != nil || !accepted {
		t.Fatal("publication receipt lost")
	}
	again, err := fresh.Report(a[model.ActorClaude], 3, "@codex same receipt")
	if err != nil || again.Message.ID != claim.ID {
		t.Fatal("restart duplicated accepted publication")
	}
	got, err := fresh.Claim(context.Background(), a[model.ActorCodex], false)
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
			_, err := e.Send(a[model.ActorClaude], SendRequest{ID: fmt.Sprintf("c-%d", i), Text: "same body valid across sends"})
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
		c, err := e.Claim(context.Background(), a[model.ActorCodex], false)
		if err != nil || c.ID != m.ID {
			t.Fatal("durable order changed")
		}
		if err := e.Ack(a[model.ActorCodex], c.ID, c.Receipt); err != nil {
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
	if _, err := e.Report(a[model.ActorClaude], 1, "@codex unsafe publication"); err == nil {
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
	e.cfg.Runtimes[model.ActorClaude] = model.RuntimeCodex
	p, err := e.Report(a[model.ActorClaude], 1, "@codex1 exact duplicate runtime peer")
	if err != nil || p.Message == nil || p.Message.To != model.ActorCodex {
		t.Fatal("stable duplicate suffix routing failed")
	}
	p, err = e.Report(a[model.ActorClaude], 2, "@codex legacy bare name should not route")
	if err != nil || p.Message != nil {
		t.Fatal("bare duplicate runtime routed")
	}
	peer, err := e.Peer(a[model.ActorClaude])
	if err != nil || peer.SessionID != a[model.ActorCodex].SessionID || peer.TranscriptPath == "" {
		t.Fatal("peer metadata missing")
	}
	claim, err := e.Claim(context.Background(), a[model.ActorCodex], false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(claim.Envelope, peer.SessionID) || strings.Contains(claim.Envelope, "/optional/transcript") {
		t.Fatal("session metadata polluted envelope")
	}
}
