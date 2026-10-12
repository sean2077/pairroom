package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
	"github.com/sean2077/pairroom/internal/store"
)

func dynamicEngine(t *testing.T, shared model.ActorID) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(Config{RoomID: "dynamic-room", Store: log, DynamicBindings: true, SharedSlot: shared,
		// Creation defaults must never manufacture actual binding identities.
		Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e, dir
}

func dynamicRequest(id string, kind model.RuntimeKind) BindRequest {
	return BindRequest{BindID: id, Runtime: kind, SessionID: "session-" + id, CredentialHash: Digest("secret-" + id)}
}

func dynamicBind(t *testing.T, e *Engine, slot model.ActorID, req BindRequest) Auth {
	t.Helper()
	b, err := e.Bind(slot, req)
	if err != nil {
		t.Fatal(err)
	}
	return Auth{Slot: b.Slot, Runtime: b.Runtime, BindID: b.BindID, Generation: b.Generation, SessionID: b.SessionID, Secret: "secret-" + req.BindID}
}

func TestDynamicNativeAutobindIsAtomicAndRetriesBeforeVacancy(t *testing.T) {
	e, _ := dynamicEngine(t, "")
	initial := e.Snapshot()
	if !initial.DynamicBindings || initial.Protocol != protocol.NativeDynamicVersion || initial.TargetGenerations[model.ActorSlot1] != 1 || initial.TargetGenerations[model.ActorSlot2] != 1 {
		t.Fatalf("empty Room context = %+v", initial)
	}
	for _, kind := range e.Runtimes() {
		if kind != model.RuntimeAwaitingBinding {
			t.Fatalf("creation default became an actual Runtime: %s", kind)
		}
	}
	first := dynamicRequest("first", model.RuntimeCodex)
	a := dynamicBind(t, e, "", first)
	if a.Slot != model.ActorSlot1 || a.Runtime != model.RuntimeCodex || a.Generation != 1 {
		t.Fatalf("first binding = %+v", a)
	}
	if b, err := e.Bind("", first); err != nil || b.Slot != model.ActorSlot1 || b.Generation != 1 || e.Snapshot().Bindings[model.ActorSlot2].Active {
		t.Fatalf("retry occupied the second slot: %+v, %v", b, err)
	}
	duplicate := dynamicRequest("duplicate-session", model.RuntimeCodex)
	duplicate.SessionID = a.SessionID
	if _, err := e.Bind("", duplicate); err == nil || e.Snapshot().Bindings[model.ActorSlot2].Active {
		t.Fatal("same Runtime/session occupied both slots")
	}
	b := dynamicBind(t, e, "", dynamicRequest("second", model.RuntimeCodex))
	if b.Slot != model.ActorSlot2 || b.Runtime != model.RuntimeCodex {
		t.Fatal("distinct same-Runtime session could not join")
	}
	if _, err := e.Bind("", dynamicRequest("third", model.RuntimeGrok)); !errors.Is(err, ErrOccupied) {
		t.Fatalf("full Room auto bind = %v", err)
	}
	if recovered, err := e.Bind("", first); err != nil || recovered.BindID != first.BindID || recovered.Generation != 1 {
		t.Fatalf("full Room could not recover original BindID: %+v, %v", recovered, err)
	}
	forged := first
	forged.Runtime = model.RuntimeClaude
	if _, err := e.Bind("", forged); !errors.Is(err, ErrAuth) {
		t.Fatal("BindID retry changed its Runtime")
	}
}

func TestDynamicNativeConcurrentAutobindsAndLANVacancy(t *testing.T) {
	e, _ := dynamicEngine(t, "")
	start := make(chan struct{})
	type result struct {
		binding Binding
		err     error
	}
	results := make(chan result, 3)
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b, err := e.Bind("", dynamicRequest(fmt.Sprintf("concurrent-%d", i), model.RuntimeClaude))
			results <- result{b, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	occupied := map[model.ActorID]bool{}
	refused := 0
	for r := range results {
		if r.err != nil {
			if !errors.Is(r.err, ErrOccupied) {
				t.Fatal(r.err)
			}
			refused++
			continue
		}
		if !r.binding.Slot.ValidParticipant() || occupied[r.binding.Slot] || r.binding.Generation != 1 {
			t.Fatalf("concurrent claim reused a slot: %+v", r.binding)
		}
		occupied[r.binding.Slot] = true
	}
	if len(occupied) != 2 || refused != 1 || e.Sequence() != 2 {
		t.Fatalf("autobind results slots=%v refused=%d sequence=%d", occupied, refused, e.Sequence())
	}

	lan, _ := dynamicEngine(t, model.ActorSlot1)
	if a := dynamicBind(t, lan, "", dynamicRequest("lan-local", model.RuntimeGemini)); a.Slot != model.ActorSlot2 {
		t.Fatal("auto bind consumed the reserved LAN peer slot")
	}
	if _, err := lan.Bind("", dynamicRequest("lan-extra", model.RuntimeClaude)); !errors.Is(err, ErrOccupied) {
		t.Fatal("second local auto bind consumed LAN vacancy")
	}
	if got := lan.Runtimes()[model.ActorSlot1]; got != model.RuntimeAwaitingPeer {
		t.Fatalf("LAN vacancy metadata = %s", got)
	}
}

func TestDynamicNativeBootstrapRejectsReplacedSelfIdentity(t *testing.T) {
	e, _ := dynamicEngine(t, "")
	first, err := e.Bind("", dynamicRequest("original", model.RuntimeClaude))
	if err != nil {
		t.Fatal(err)
	}
	if text, generation, err := e.BootstrapContextForBinding(first); err != nil || generation != 1 || !strings.Contains(text, "peer: @agent2") {
		t.Fatalf("original binding bootstrap = %q, %d, %v", text, generation, err)
	}
	req := dynamicRequest("replacement", model.RuntimeGrok)
	req.Replace = true
	next, err := e.Bind(first.Slot, req)
	if err != nil {
		t.Fatal(err)
	}
	if text, generation, err := e.BootstrapContextForBinding(first); !errors.Is(err, ErrAuth) || text != "" || generation != 0 {
		t.Fatalf("old binding received its successor's context: %q, %d, %v", text, generation, err)
	}
	for _, mutation := range []func(*Binding){
		func(b *Binding) { b.Runtime = model.RuntimeClaude },
		func(b *Binding) { b.SessionID = first.SessionID },
		func(b *Binding) { b.RemoteKey = Digest("foreign-member") },
		func(b *Binding) { b.Active = false },
	} {
		wrong := next
		mutation(&wrong)
		if _, _, err := e.BootstrapContextForBinding(wrong); !errors.Is(err, ErrAuth) {
			t.Fatal("bootstrap accepted a mismatched self identity")
		}
	}
	if text, generation, err := e.BootstrapContextForBinding(next); err != nil || generation != 1 || !strings.Contains(text, "Grok clips hook text") {
		t.Fatalf("current binding bootstrap = %q, %d, %v", text, generation, err)
	}
	if err := e.Unbind(next.Slot); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.BootstrapContextForBinding(next); !errors.Is(err, ErrAuth) {
		t.Fatal("revoked binding still received a ready bootstrap")
	}
}

func TestDynamicNativeReplacementPinsPublicationsAndHistoricalAuthor(t *testing.T) {
	e, dir := dynamicEngine(t, "")
	a := dynamicBind(t, e, model.ActorSlot1, dynamicRequest("a", model.RuntimeClaude))
	b := dynamicBind(t, e, model.ActorSlot2, dynamicRequest("b", model.RuntimeCodex))
	accepted, err := e.Report(a, 1, "original @agent2 task", 1)
	if err != nil || accepted.Message == nil || accepted.Message.TargetGeneration != 1 || accepted.Message.FromRuntime != model.RuntimeClaude || accepted.Message.FromGeneration != 1 {
		t.Fatalf("accepted report = %+v, %v", accepted, err)
	}
	priorSend, err := e.Send(a, SendRequest{ID: "accepted-send", Text: "original explicit task", ExpectedPeerGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	oldReply, err := e.Send(b, SendRequest{ID: "old-author", Text: "evidence from Codex", ExpectedPeerGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	replacement := dynamicRequest("replacement", model.RuntimeGrok)
	replacement.Replace = true
	next := dynamicBind(t, e, model.ActorSlot2, replacement)
	if next.Generation != 2 || next.Runtime != model.RuntimeGrok || e.Runtimes()[model.ActorSlot2] != model.RuntimeGrok {
		t.Fatalf("replacement = %+v", next)
	}
	for _, wrong := range []Auth{b, {Slot: next.Slot, Runtime: model.RuntimeCodex, BindID: next.BindID, Generation: next.Generation, SessionID: next.SessionID, Secret: next.Secret}, {Slot: next.Slot, BindID: next.BindID, Generation: next.Generation, SessionID: next.SessionID, Secret: next.Secret}} {
		if _, err := e.Send(wrong, SendRequest{ID: "stale-auth", Text: "forged", ExpectedPeerGeneration: 1}); !errors.Is(err, ErrAuth) {
			t.Fatalf("stale or wrong-Runtime credentials authenticated: %v", err)
		}
	}
	if again, err := e.Report(a, 1, "retry after replacement", 1); err != nil || again.Message == nil || again.Message.ID != accepted.Message.ID || again.Message.TargetGeneration != 1 {
		t.Fatalf("accepted report was retargeted: %+v, %v", again, err)
	}
	if again, err := e.Send(a, SendRequest{ID: "accepted-send", Text: priorSend.Text, ExpectedPeerGeneration: 1}); err != nil || again.ID != priorSend.ID || again.TargetGeneration != 1 || again.State != "cancelled" {
		t.Fatalf("accepted send retry was retargeted: %+v, %v", again, err)
	}
	if _, err := e.Send(a, SendRequest{ID: "accepted-send", Text: priorSend.Text, ExpectedPeerGeneration: 2}); !errors.Is(err, ErrSendPayloadConflict) {
		t.Fatalf("accepted send ID allowed changed target generation: %v", err)
	}
	before := e.Sequence()
	late, err := e.Report(a, 2, "private obsolete reply @agent2", 1)
	if err != nil || late.Outcome != PeerChangedCode || late.Message != nil || late.ExpectedPeerGeneration != 1 || e.Sequence() != before+1 {
		t.Fatalf("stale report did not settle without forwarding: %+v, %v", late, err)
	}
	if repeated, err := e.Report(a, 2, "cannot reuse this report for @agent2", 2); err != nil || repeated.Outcome != PeerChangedCode || repeated.ExpectedPeerGeneration != 1 || repeated.Message != nil || e.Sequence() != before+1 {
		t.Fatalf("terminal stale report retargeted on retry: %+v, %v", repeated, err)
	}
	if _, err := e.Send(a, SendRequest{ID: "late-send", Text: "old peer", ExpectedPeerGeneration: 1}); !errors.Is(err, ErrPeerChanged) || e.Sequence() != before+1 {
		t.Fatalf("late send created an effect: %v", err)
	}
	if _, err := e.Send(a, SendRequest{ID: "no-context", Text: "no target provenance"}); !errors.Is(err, ErrPeerChanged) {
		t.Fatalf("dynamic peer send allowed missing context: %v", err)
	}
	if p, err := e.Report(a, 3, "@agent2 without originating context"); err != nil || p.Outcome != PeerChangedCode || p.Message != nil {
		t.Fatalf("missing context report was not terminal: %+v, %v", p, err)
	}
	if p, err := e.Report(a, 4, "@user decision stays human", 1); err != nil || p.Message == nil || p.Message.To != model.ActorUser || p.ExpectedPeerGeneration != 1 {
		t.Fatalf("stale peer context blocked human routing: %+v, %v", p, err)
	}
	if p, err := e.Report(a, 5, "private unaddressed response", 1); err != nil || p.Message != nil || p.Outcome != "" || p.ExpectedPeerGeneration != 1 {
		t.Fatalf("non-routed receipt lost original context: %+v, %v", p, err)
	}
	if _, err := e.Send(a, SendRequest{ID: "new-peer", Text: "fresh work for Grok", ExpectedPeerGeneration: 2}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	claim, err := e.Claim(ctx, a, false)
	if err != nil || claim == nil || claim.ID != oldReply.ID || claim.PeerGeneration != 2 || !strings.Contains(claim.Envelope, "from: @agent2 (codex, generation 1)") || !strings.Contains(claim.Envelope, "peer: @agent2, generation 2") {
		t.Fatalf("delivery lost historical source/current peer distinction: %+v, %v", claim, err)
	}
	if err := e.Ack(a, claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	events, err := e.cfg.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Data), "private obsolete reply") {
			t.Fatal("rejected Stop body entered shared history")
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(Config{RoomID: "dynamic-room", Store: log, DynamicBindings: true})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	snapshot := restarted.Snapshot()
	if snapshot.Bindings[model.ActorSlot2].Runtime != model.RuntimeGrok || snapshot.Bindings[model.ActorSlot2].Generation != 2 || restarted.Runtimes()[model.ActorSlot1] != model.RuntimeClaude {
		t.Fatalf("replay lost actual runtimes: %+v", snapshot.Bindings)
	}
	for _, message := range snapshot.Messages {
		if message.ID == oldReply.ID && (message.FromRuntime != model.RuntimeCodex || message.FromGeneration != 1) {
			t.Fatal("replacement retroactively renamed old message author")
		}
	}
	if p, ok, err := restarted.Publication(a, 2); err != nil || !ok || p.Outcome != PeerChangedCode || p.Message != nil {
		t.Fatalf("terminal publication did not survive replay: %+v, %v", p, err)
	}
}

func TestDynamicNativePrejoinQueueReservesFirstGeneration(t *testing.T) {
	for _, shared := range []model.ActorID{"", model.ActorSlot2} {
		t.Run(string(shared), func(t *testing.T) {
			e, _ := dynamicEngine(t, shared)
			a := dynamicBind(t, e, "", dynamicRequest("creator", model.RuntimeCodex))
			bootstrap, generation := e.BootstrapContext(a.Slot)
			if generation != 1 || !strings.Contains(bootstrap, "peer: @agent2") || !strings.Contains(bootstrap, "generation 1") {
				t.Fatalf("vacancy context = %q, %d", bootstrap, generation)
			}
			queued, err := e.Send(a, SendRequest{ID: "prejoin", Text: "prepared work", ExpectedPeerGeneration: 1})
			if err != nil || queued.TargetGeneration != 1 {
				t.Fatalf("prejoin send = %+v, %v", queued, err)
			}
			var b Auth
			if shared == "" {
				b = dynamicBind(t, e, "", dynamicRequest("first-peer", model.RuntimeGemini))
			} else {
				b = admitLAN(t, e, Digest("first-peer"))
			}
			if e.PeerGeneration(a.Slot) != 1 || b.Generation != 1 {
				t.Fatal("initial admission changed the reserved generation")
			}
			late, err := e.Report(a, 1, "work composed before first peer joined @agent2", 1)
			if err != nil || late.Message == nil || late.Message.TargetGeneration != 1 || late.Outcome != "" {
				t.Fatalf("initial admission invalidated originating context: %+v, %v", late, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if shared == "" {
				claim, err := e.Claim(ctx, b, false)
				if err != nil || claim == nil || claim.ID != queued.ID || claim.PeerGeneration != 1 {
					t.Fatalf("prejoin message not delivered: %+v, %v", claim, err)
				}
			} else {
				head, err := e.PrepareHead(ctx, b, false)
				if err != nil || head == nil {
					t.Fatalf("prepare LAN head: %+v, %v", head, err)
				}
				claim, err := e.ClaimPrepared(ctx, b, head.Message.ID, head.Digest, false)
				if err != nil || claim == nil || claim.ID != queued.ID || claim.PeerGeneration != 1 || claim.Message.FromRuntime != model.RuntimeCodex {
					t.Fatalf("prejoin LAN message not delivered: %+v, %v", claim, err)
				}
			}
		})
	}
}

func TestDynamicNativeLANCanonicalizesRuntimeBeforeDurableAdmission(t *testing.T) {
	e, _ := dynamicEngine(t, model.ActorSlot2)
	dynamicBind(t, e, "", dynamicRequest("local", model.RuntimeClaude))
	invite, err := e.CreateLANInvite()
	if err != nil {
		t.Fatal(err)
	}
	j := LANJoinRequest{RequestID: "alias-join", InviteID: invite.ID, Key: Digest("alias-key"), Runtime: " CODEX "}
	accepted, status, err := e.RequestLANJoin(j)
	if err != nil || status != "pending" || accepted.Runtime != model.RuntimeCodex {
		t.Fatalf("canonical request = %+v, %s, %v", accepted, status, err)
	}
	j.Runtime = model.RuntimeCodex
	if _, status, err := e.RequestLANJoin(j); err != nil || status != "pending" {
		t.Fatalf("canonical retry = %s, %v", status, err)
	}
	b, err := e.AcceptLANJoin(j.RequestID, j.Key)
	if err != nil || b.Runtime != model.RuntimeCodex || b.Generation != 1 {
		t.Fatalf("canonical membership = %+v, %v", b, err)
	}
	if _, err := e.LANAuth(j.Key); err != nil {
		t.Fatalf("admitting a valid alias poisoned the writer: %v", err)
	}
}

func TestDynamicNativeReplacementDoesNotRetargetUnknownRetry(t *testing.T) {
	e, _ := dynamicEngine(t, "")
	a := dynamicBind(t, e, "", dynamicRequest("a", model.RuntimeClaude))
	b := dynamicBind(t, e, "", dynamicRequest("b", model.RuntimeCodex))
	message, err := e.Send(a, SendRequest{ID: "uncertain", Text: "possibly accepted work", ExpectedPeerGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	claim, err := e.Claim(ctx, b, false)
	if err != nil || claim == nil {
		t.Fatalf("claim = %+v, %v", claim, err)
	}
	req := dynamicRequest("successor", model.RuntimeClaude)
	req.Replace = true
	dynamicBind(t, e, model.ActorSlot2, req)
	if _, err := e.Retry(message.ID); !errors.Is(err, ErrPeerChanged) {
		t.Fatalf("Retry silently assigned uncertain old work to a successor: %v", err)
	}
	if err := e.Ack(b, claim.ID, claim.Receipt); !errors.Is(err, ErrAuth) {
		t.Fatal("revoked collector acknowledged old work")
	}
}

func TestDynamicNativeUnbindErasesCredentialsAndReservesSuccessorAcrossRestart(t *testing.T) {
	for _, self := range []bool{false, true} {
		t.Run(fmt.Sprintf("self=%t", self), func(t *testing.T) {
			e, dir := dynamicEngine(t, "")
			a := dynamicBind(t, e, "", dynamicRequest("departing", model.RuntimeCodex))
			b := dynamicBind(t, e, "", dynamicRequest("continuing", model.RuntimeClaude))
			old, err := e.Send(b, SendRequest{ID: "old-recipient", Text: "work for departing session", ExpectedPeerGeneration: 1})
			if err != nil {
				t.Fatal(err)
			}
			if self {
				err = e.UnbindAs(a)
			} else {
				err = e.Unbind(a.Slot)
			}
			if err != nil {
				t.Fatal(err)
			}
			fact := e.bindings[a.Slot]
			if fact.Active || fact.Runtime != model.RuntimeCodex || fact.Generation != 1 || fact.SessionID != "" || fact.CredentialHash != "" || fact.TranscriptPath != "" {
				t.Fatalf("unbound identity = %+v", fact.Binding)
			}
			if e.Runtimes()[a.Slot] != model.RuntimeAwaitingBinding || e.PeerGeneration(b.Slot) != 2 || e.messages[old.ID].State != "cancelled" {
				t.Fatal("unbind retained an execution runtime or old target inbox")
			}
			if _, err := e.Inspect(a); !errors.Is(err, ErrAuth) {
				t.Fatal("deactivated credentials still authenticate")
			}
			if _, err := e.Send(b, SendRequest{ID: "old-context", Text: "late old context", ExpectedPeerGeneration: 1}); !errors.Is(err, ErrPeerChanged) {
				t.Fatal("deactivation treated old peer context as a wildcard")
			}
			queued, err := e.Send(b, SendRequest{ID: "successor-work", Text: "new work for next binding", ExpectedPeerGeneration: 2})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			log, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := Open(Config{RoomID: "dynamic-room", Store: log, DynamicBindings: true})
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if _, err := restarted.Inspect(a); !errors.Is(err, ErrAuth) || restarted.Runtimes()[a.Slot] != model.RuntimeAwaitingBinding || restarted.PeerGeneration(b.Slot) != 2 {
				t.Fatal("restart restored a deactivated identity")
			}
			if _, err := restarted.Bind("", dynamicRequest("departing", model.RuntimeCodex)); err == nil {
				t.Fatal("revoked BindID was reused")
			}
			next := dynamicBind(t, restarted, "", dynamicRequest("next", model.RuntimeGrok))
			if next.Slot != a.Slot || next.Generation != 2 || next.Runtime != model.RuntimeGrok {
				t.Fatalf("successor = %+v", next)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			claim, err := restarted.Claim(ctx, next, false)
			if err != nil || claim == nil || claim.ID != queued.ID || claim.PeerGeneration != b.Generation {
				t.Fatalf("successor lost intentionally queued work: %+v, %v", claim, err)
			}
		})
	}
}

func TestDynamicNativeReplayRejectsRuntimeMutationWithinGeneration(t *testing.T) {
	e, dir := dynamicEngine(t, "")
	a := dynamicBind(t, e, "", dynamicRequest("a", model.RuntimeClaude))
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	fact := bindingFact{Binding: Binding{Slot: a.Slot, BindID: a.BindID, Runtime: model.RuntimeCodex, Generation: a.Generation, SessionID: a.SessionID, Active: true}, CredentialHash: Digest(a.Secret)}
	event, err := model.NewEvent("dynamic-room", EventBinding, a.Slot, fact)
	if err != nil || log.Append(&event) != nil {
		t.Fatal("could not create malformed replay fixture")
	}
	if _, err := Open(Config{RoomID: "dynamic-room", Store: log, DynamicBindings: true}); err == nil {
		t.Fatal("replay allowed runtime mutation in the same generation")
	}
	_ = log.Close()
}

func TestDynamicNativeWakeProbeReplaysLANAndUsesActualBindingRuntime(t *testing.T) {
	e, _ := dynamicEngine(t, model.ActorSlot2)
	a := dynamicBind(t, e, "", dynamicRequest("original", model.RuntimeClaude))
	admitLAN(t, e, Digest("remote-peer"))
	req := dynamicRequest("replacement", model.RuntimeGrok)
	req.Replace = true
	next := dynamicBind(t, e, a.Slot, req)
	message, err := e.SendUser(SendRequest{ID: "waiting", To: next.Slot, Text: "work for current session"})
	if err != nil {
		t.Fatal(err)
	}
	if candidate, ok := e.WakeCandidate(message.ID); !ok || candidate.Runtime != model.RuntimeGrok || candidate.Generation != 2 || candidate.SessionID != next.SessionID {
		t.Fatalf("wake candidate did not use actual binding: %+v, %v", candidate, ok)
	}
	if err := e.ReserveWake(message.ID, next.Slot); !errors.Is(err, ErrWakeIneligible) {
		t.Fatalf("replacement inherited its predecessor's wake capability: %v", err)
	}
	for _, bound := range []bool{true, false} {
		if !bound {
			if err := e.Unbind(next.Slot); err != nil {
				t.Fatal(err)
			}
		}
		events, err := e.cfg.Store.Load()
		if err != nil {
			t.Fatal(err)
		}
		sequence := e.Sequence()
		// Persisted creation selections contain no runtime. The read-only
		// probe must replay the LAN join and later local replacement facts.
		creation := map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeAwaitingBinding, model.ActorSlot2: model.RuntimeAwaitingPeer}
		if got, err := HasWakeWorkWithSharedSlot("dynamic-room", events, creation, model.ActorSlot2, true); err != nil || got != bound {
			t.Fatalf("wake probe with bound=%v returned %v, %v", bound, got, err)
		}
		if e.Sequence() != sequence || creation[model.ActorSlot1] != model.RuntimeAwaitingBinding || creation[model.ActorSlot2] != model.RuntimeAwaitingPeer {
			t.Fatal("startup probe mutated Room state or immutable selections")
		}
	}
}

func TestDynamicNativeIdentityDoesNotLeakPrivateMetadata(t *testing.T) {
	e, _ := dynamicEngine(t, model.ActorSlot2)
	a := dynamicBind(t, e, "", dynamicRequest("private-owner", model.RuntimeClaude))
	b := admitLAN(t, e, Digest("remote-member"))
	if _, err := e.Send(a, SendRequest{ID: "shared", Text: "intended content", ExpectedPeerGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	tail, err := e.AuthSnapshotTail(b)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(tail)
	if err != nil || !tail.DynamicBindings || tail.Protocol != protocol.NativeDynamicVersion || tail.TargetGenerations[model.ActorSlot1] != 1 {
		t.Fatalf("LAN dynamic snapshot = %+v, %v", tail, err)
	}
	for _, private := range []string{a.SessionID, a.Secret, Digest(a.Secret), "credential_hash", "transcript_path"} {
		if strings.Contains(string(data), private) {
			t.Fatalf("dynamic public context leaked %q", private)
		}
	}
}

func TestDynamicNativeReportAcceptsAtMostOneExpectedGeneration(t *testing.T) {
	e, _ := dynamicEngine(t, "")
	source := dynamicBind(t, e, "", dynamicRequest("report-arity-source", model.RuntimeClaude))
	peer := dynamicBind(t, e, "", dynamicRequest("report-arity-peer", model.RuntimeCodex))
	before := e.Sequence()
	if _, err := e.Report(source, 1, "ambiguous context @agent2", peer.Generation, peer.Generation); err == nil {
		t.Fatal("report accepted more than one expected peer generation")
	}
	if e.Sequence() != before || len(e.Snapshot().Messages) != 0 {
		t.Fatal("rejected report appended a fact or forwarded a message")
	}
	// The supported arities stay wire-compatible and the sequence is not consumed.
	accepted, err := e.Report(source, 1, "one context @agent2", peer.Generation)
	if err != nil || accepted.Message == nil || accepted.ExpectedPeerGeneration != peer.Generation || accepted.Message.TargetGeneration != peer.Generation || accepted.Outcome != "" {
		t.Fatalf("single-generation report = %+v, %v", accepted, err)
	}
	// An accepted report is recovered by its own receipt whatever arity the retry uses.
	recovered, err := e.Report(source, 1, "retry text @agent2", peer.Generation, peer.Generation)
	if err != nil || recovered.ReportSeq != 1 || recovered.Message == nil || recovered.Message.ID != accepted.Message.ID {
		t.Fatalf("accepted receipt recovery = %+v, %v", recovered, err)
	}
	if e.Sequence() != before+1 {
		t.Fatal("idempotent recovery appended another fact")
	}
}
