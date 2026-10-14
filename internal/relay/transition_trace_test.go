package relay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// Contract-derived transition traces.
//
// A second storage backend (the enterprise PostgreSQL persistence boundary)
// must reproduce the durable fact sequence these traces pin, not only the
// API-level results already asserted by engine_test.go, delivery_test.go and
// dynamic_test.go. Expectations come from docs/STORAGE.md and docs/PROTOCOL.md:
//
//   - "Publication acceptance and optional enqueue share one append."
//   - A delivery claim is persisted before the envelope leaves the engine.
//   - An acknowledgement appends its receipt fact and keeps the original
//     identity.
//   - Cancellation removes queued work only.
//   - A recovered delivering claim keeps its original identity and receipt.
//   - A stale-generation publication terminates with a body-free receipt and
//     enqueues nothing for the successor.
//
// Scope: these traces speak about durable facts and engine state. They do not
// prove stdout ordering; that boundary is covered by the real CLI tests
// TestDeliveryAcknowledgesOnlyAfterCompleteStdout and
// TestHookKeepsBlockDecisionWhenAcknowledgementIsUncertain in
// internal/relayclient, which drive an actual collector process.
//
// Normalized facts keep only contract-owned fields, so a future backend can be
// diffed against the same expectations instead of against engine internals.

type traceFact struct {
	Kind       string
	Actor      string
	ID         string
	State      string
	Outcome    string
	HasMessage bool
}

func traceFacts(t *testing.T, e *Engine) []traceFact {
	t.Helper()
	events, err := e.cfg.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	facts := make([]traceFact, 0, len(events))
	for _, ev := range events {
		fact := traceFact{Kind: ev.Kind, Actor: string(ev.Actor)}
		var payload struct {
			ID      string `json:"id"`
			State   string `json:"state"`
			Outcome string `json:"outcome"`
			Message *struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"message"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err == nil {
			fact.Outcome = payload.Outcome
			fact.ID = payload.ID
			fact.State = payload.State
			if payload.Message != nil {
				fact.ID = payload.Message.ID
				fact.State = payload.Message.State
			}
			fact.HasMessage = fact.ID != ""
		}
		facts = append(facts, fact)
	}
	return facts
}

// traceDelta returns the facts appended after the given baseline length and
// verifies that the existing prefix is unchanged, which is the append-only
// integrity a second backend must also preserve.
func traceDelta(t *testing.T, e *Engine, baseline []traceFact) []traceFact {
	t.Helper()
	now := traceFacts(t, e)
	if len(now) < len(baseline) {
		t.Fatalf("durable facts shrank: %d -> %d", len(baseline), len(now))
	}
	for i, fact := range baseline {
		if now[i] != fact {
			t.Fatalf("fact %d changed: %+v -> %+v", i, fact, now[i])
		}
	}
	return now[len(baseline):]
}

func TestTraceAcceptedDeliveryAppendsOneFactPerTransition(t *testing.T) {
	e, a, _ := testEngine(t)
	baseline := traceFacts(t, e)

	message, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "trace-send", Text: "trace body"})
	if err != nil {
		t.Fatal(err)
	}
	delta := traceDelta(t, e, baseline)
	if len(delta) != 1 || delta[0].Kind != EventMessage || delta[0].State != "queued" || delta[0].ID != message.ID {
		t.Fatalf("acceptance and enqueue did not share exactly one append: %+v", delta)
	}

	baseline = traceFacts(t, e)
	claim, err := e.Claim(context.Background(), a[model.ActorSlot2], false)
	if err != nil || claim == nil || claim.ID != message.ID {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	delta = traceDelta(t, e, baseline)
	if len(delta) != 1 || delta[0].Kind != EventMessage || delta[0].State != "delivering" || delta[0].ID != message.ID {
		t.Fatalf("claim was not exactly one durable fact before the envelope left: %+v", delta)
	}

	baseline = traceFacts(t, e)
	if err := e.Ack(a[model.ActorSlot2], claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	delta = traceDelta(t, e, baseline)
	if len(delta) != 1 || delta[0].Kind != EventMessage || delta[0].State != "handed_off" || delta[0].ID != message.ID {
		t.Fatalf("acknowledgement was not exactly one terminal append: %+v", delta)
	}

	baseline = traceFacts(t, e)
	queued, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "trace-cancel", Text: "cancel me"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Cancel(queued.ID); err != nil {
		t.Fatal(err)
	}
	delta = traceDelta(t, e, baseline)
	if len(delta) != 2 || delta[0].State != "queued" || delta[1].State != "cancelled" || delta[1].ID != queued.ID {
		t.Fatalf("cancellation did not append a single terminal fact: %+v", delta)
	}
}

func TestTraceRecoveredDeliveryKeepsOriginalIdentityAndReceipt(t *testing.T) {
	e, a, dir := testEngine(t)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e.cfg.Now = func() time.Time { return now }

	message, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "trace-unknown", Text: "uncertain work"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := e.Claim(context.Background(), a[model.ActorSlot2], false)
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	now = now.Add(e.cfg.Lease + time.Second)
	if err := e.Reap(); err != nil {
		t.Fatal(err)
	}
	if state := e.Snapshot().Messages[0].State; state != "unknown" {
		t.Fatalf("lease loss state = %q", state)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	restored := reopenEngine(t, dir)
	if state := restored.Snapshot().Messages[0].State; state != "unknown" {
		t.Fatalf("restart changed the uncertain state: %q", state)
	}
	baseline := traceFacts(t, restored)
	if err := restored.Ack(a[model.ActorSlot2], claim.ID, claim.Receipt); err != nil {
		t.Fatalf("original receipt rejected after restart: %v", err)
	}
	delta := traceDelta(t, restored, baseline)
	if len(delta) != 1 || delta[0].State != "handed_off" || delta[0].ID != message.ID {
		t.Fatalf("settlement allocated a new identity: %+v", delta)
	}
	if got := restored.Snapshot().Messages; len(got) != 1 || got[0].ID != message.ID {
		t.Fatalf("settlement changed the message set: %+v", got)
	}
}

func TestTraceStaleGenerationPublicationIsBodyFree(t *testing.T) {
	e, _ := dynamicEngine(t, "")
	sender := dynamicBind(t, e, model.ActorSlot1, dynamicRequest("trace-sender", model.RuntimeClaude))
	dynamicBind(t, e, model.ActorSlot2, dynamicRequest("trace-receiver", model.RuntimeCodex))
	baseline := traceFacts(t, e)

	const body = "@agent2 private stale reply text"
	publication, err := e.Report(sender, 1, body, 99)
	if err != nil {
		t.Fatal(err)
	}
	if publication.Outcome != PeerChangedCode || publication.Message != nil {
		t.Fatalf("stale peer generation did not terminate body-free: %+v", publication)
	}
	delta := traceDelta(t, e, baseline)
	if len(delta) != 1 || delta[0].Kind != EventPublication || delta[0].Outcome != PeerChangedCode || delta[0].HasMessage {
		t.Fatalf("stale publication facts: %+v", delta)
	}
	for _, fact := range traceFacts(t, e) {
		if fact.Kind == EventMessage {
			t.Fatalf("stale publication enqueued work for the successor: %+v", fact)
		}
	}
	events, err := e.cfg.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if strings.Contains(string(ev.Data), body) {
			t.Fatal("stale publication persisted its private reply body")
		}
	}
}
