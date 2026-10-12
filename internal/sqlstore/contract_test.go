package sqlstore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/store"
)

// The kernel's pinned constants must equal the relay contracts they mirror;
// they are pinned here so a relay change fails this test instead of silently
// diverging.
func TestHumanSendConstantsMatchRelayContracts(t *testing.T) {
	if humanSendKind != relay.EventMessage {
		t.Fatalf("humanSendKind %q != relay.EventMessage %q", humanSendKind, relay.EventMessage)
	}
	if humanSendTextBytes != relay.MaxBodyBytes {
		t.Fatalf("humanSendTextBytes %d != relay.MaxBodyBytes %d", humanSendTextBytes, relay.MaxBodyBytes)
	}
	if humanSendMaxTokenBytes != 256 {
		t.Fatalf("humanSendMaxTokenBytes %d != 256", humanSendMaxTokenBytes)
	}
}

// decodeProducerFact runs the real Local producer (relay engine over a JSONL
// store) and decodes its persisted fact with the kernel's pinned codec.
func decodeProducerFact(t *testing.T) (model.Event, humanSendPayload) {
	t.Helper()
	dir := t.TempDir()
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := relay.Open(relay.Config{
		RoomID:   "room-1",
		Store:    log,
		Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.SendUser(relay.SendRequest{ID: "bridge-1", Text: "bridge body", To: model.ActorSlot2}); err != nil {
		t.Fatal(err)
	}
	event, err := log.ReadEvent(1)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := decodeHumanSendPayload(event.Data)
	if err != nil {
		t.Fatalf("kernel codec rejects the real producer fact: %v", err)
	}
	return event, payload
}

// The kernel codec decodes the real producer fact without losing or inventing
// fields.
func TestKernelCodecDecodesTheLocalProducerFact(t *testing.T) {
	event, payload := decodeProducerFact(t)
	if event.Kind != humanSendKind || event.Actor != model.ActorUser || event.RoomID != "room-1" {
		t.Fatalf("producer event: %+v", event)
	}
	if payload.ID == "" || payload.From != model.ActorUser || string(payload.To) != "slot2" || payload.Text != "bridge body" {
		t.Fatalf("payload identity/body: %+v", payload)
	}
	if payload.State != "queued" || payload.Source != "send" || payload.TargetGeneration != 1 {
		t.Fatalf("payload state/source/generation: %+v", payload)
	}
	if payload.ClientKey != "user/bridge-1" {
		t.Fatalf("Local client key form changed: %q", payload.ClientKey)
	}
	if payload.Review != nil || payload.Quote != nil || len(payload.Attachments) != 0 {
		t.Fatalf("unexpected optional content: %+v", payload)
	}
	if payload.CreatedAt.IsZero() || !payload.UpdatedAt.Equal(payload.CreatedAt) {
		t.Fatalf("payload times: %+v", payload)
	}
}

// The producer-derived fact, re-scoped to the enterprise principal-scoped
// client key, commits through the kernel; an unknown field injected into the
// same bytes is refused with zero writes.
func TestHumanSendCommitsProducerDerivedFact(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	event, payload := decodeProducerFact(t)
	payload.ClientKey = humanSendClientKey("u-owner", "bridge-1")
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	event.Data = data
	cmd := HumanSendCommand{
		Auth:     AuthContext{Deployment: "dep-test", UserID: "u-owner"},
		RoomID:   "room-1",
		ClientID: "bridge-1",
		Intent:   HumanSendIntent{To: "slot2", Text: "bridge body"},
		Fact:     event,
	}
	res, err := f.store.CommitHumanSend(ctx, cmd)
	if err != nil || res.Outcome != OutcomeCommitted || res.Fact.ID != event.ID {
		t.Fatalf("producer-derived commit: %+v %v", res, err)
	}

	var injected map[string]any
	if err := json.Unmarshal(data, &injected); err != nil {
		t.Fatal(err)
	}
	injected["mystery"] = true
	injectedData, err := json.Marshal(injected)
	if err != nil {
		t.Fatal(err)
	}
	forged := cmd
	forged.ClientID = "bridge-2"
	forged.Fact.ID = "evt-bridge-2"
	forged.Fact.Data = injectedData
	res, err = f.store.CommitHumanSend(ctx, forged)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassInvalidFact {
		t.Fatalf("injected field: %+v", res)
	}
	if events, _, _, _, _ := f.roomCounts(t); events != 1 {
		t.Fatalf("refusal wrote state: events=%d", events)
	}
}
