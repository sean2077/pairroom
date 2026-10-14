package relay

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/review"
)

func deliveryPlanningMessage(id string) Message {
	at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	return Message{ID: id, From: model.ActorSlot1, To: model.ActorSlot2, Text: "original text", State: "delivering",
		FromRuntime: model.RuntimeClaude, FromGeneration: 7, TargetGeneration: 3, Source: "send", Author: "host_owner",
		CreatedAt: at, UpdatedAt: at, ClaimedAt: at, Receipt: "private-receipt",
		Attachments: []model.Attachment{{ID: "image-original", SHA256: "original-hash"}},
		Quote:       &model.AgentQuote{FromHandle: "@agent2", Text: "original quote"},
		Review:      &review.Anchor{Schema: 1, Workspace: "original-workspace", Head: "original-head"}}
}

func TestDeliveryPlanExpiryOrderBoundaryAndPurity(t *testing.T) {
	z, a := deliveryPlanningMessage("z"), deliveryPlanningMessage("a")
	recent := deliveryPlanningMessage("recent")
	recent.ClaimedAt = recent.ClaimedAt.Add(time.Second)
	queued := deliveryPlanningMessage("queued")
	queued.State = "queued"
	unknown := deliveryPlanningMessage("unknown")
	unknown.State = "unknown"
	input := []Message{z, recent, a, queued, unknown}
	before, _ := json.Marshal(input)
	now := a.ClaimedAt.Add(DeliveryLease)
	facts := planExpiryFacts(input, now, DeliveryLease, false)
	if len(facts) != 2 || facts[0].ID != "a" || facts[1].ID != "z" {
		t.Fatalf("expiry order/boundary: %+v", facts)
	}
	for _, fact := range facts {
		if fact.State != "unknown" || fact.Receipt != "private-receipt" || fact.Message.Receipt != "private-receipt" || !fact.UpdatedAt.Equal(now) || !fact.CreatedAt.Equal(a.CreatedAt) || !fact.ClaimedAt.Equal(a.ClaimedAt) {
			t.Fatalf("expiry changed original identity/receipt/time: %+v", fact)
		}
	}
	if got := planExpiryFacts(input, now.Add(-time.Nanosecond), DeliveryLease, false); len(got) != 0 {
		t.Fatal("lease expired before its inclusive boundary")
	}
	all := planExpiryFacts(input, now.Add(-time.Nanosecond), DeliveryLease, true)
	if len(all) != 3 || all[0].ID != "a" || all[1].ID != "recent" || all[2].ID != "z" {
		t.Fatalf("forced expiry touched terminal/queued work: %+v", all)
	}
	facts[0].Attachments[0].ID = "changed"
	facts[0].Quote.Text = "changed"
	facts[0].Review.Workspace = "changed"
	after, _ := json.Marshal(input)
	if string(before) != string(after) || input[2].Receipt != "private-receipt" || input[0].ID != "z" {
		t.Fatal("expiry plan mutated input order or nested metadata")
	}
}

func TestDeliveryPlanAcknowledgementDecisions(t *testing.T) {
	for _, test := range []struct {
		state   string
		pending bool
		want    acknowledgementDecision
		wantErr bool
	}{
		{"delivering", false, acknowledgementHandoff, false},
		{"delivering", true, acknowledgementHandoff, false},
		{"unknown", false, acknowledgementHandoff, false},
		{"unknown", true, acknowledgementNoChange, true},
		{"handed_off", false, acknowledgementNoChange, false},
		{"handed_off", true, acknowledgementNoChange, false},
		{"queued", false, acknowledgementNoChange, true},
		{"cancelled", false, acknowledgementNoChange, true},
	} {
		message := deliveryPlanningMessage("message")
		message.State = test.state
		got, err := decideAcknowledgement(message, true, model.ActorSlot2, 3, "private-receipt", test.pending)
		if got != test.want || (err != nil) != test.wantErr {
			t.Fatalf("state=%s pending=%v: %v %v", test.state, test.pending, got, err)
		}
	}
	message := deliveryPlanningMessage("message")
	message.State = "handed_off"
	for _, test := range []struct {
		exists     bool
		slot       model.ActorID
		generation uint64
		receipt    string
	}{
		{false, model.ActorSlot2, 3, "private-receipt"},
		{true, model.ActorSlot1, 3, "private-receipt"},
		{true, model.ActorSlot2, 4, "private-receipt"},
		{true, model.ActorSlot2, 3, ""},
		{true, model.ActorSlot2, 3, "forged"},
	} {
		if _, err := decideAcknowledgement(message, test.exists, test.slot, test.generation, test.receipt, true); !errors.Is(err, ErrAuth) {
			t.Fatalf("settled receipt bypassed identity validation: %v", err)
		}
	}
	message.State = "unknown"
	if _, err := decideAcknowledgement(message, true, model.ActorSlot2, 3, "forged", true); !errors.Is(err, ErrAuth) {
		t.Fatalf("pending retry preceded receipt proof: %v", err)
	}
}

func TestDeliveryPlanAcknowledgementPreservesReceiptAndCopiesMetadata(t *testing.T) {
	original := deliveryPlanningMessage("message")
	at := original.UpdatedAt.Add(time.Minute)
	fact := planAcknowledgementFact(original, at)
	if fact.State != "handed_off" || fact.ID != original.ID || fact.Receipt != original.Receipt || fact.Message.Receipt != original.Receipt || fact.TargetGeneration != 3 || !fact.CreatedAt.Equal(original.CreatedAt) || !fact.ClaimedAt.Equal(original.ClaimedAt) || !fact.UpdatedAt.Equal(at) {
		t.Fatalf("handoff fact: %+v", fact)
	}
	fact.Attachments[0].ID, fact.Quote.Text, fact.Review.Workspace = "changed", "changed", "changed"
	if original.State != "delivering" || original.Attachments[0].ID != "image-original" || original.Quote.Text != "original quote" || original.Review.Workspace != "original-workspace" {
		t.Fatal("handoff plan changed input")
	}
}

func TestDeliveryPlanRetryDecisionsAndProvenance(t *testing.T) {
	original := deliveryPlanningMessage("original")
	original.State = "unknown"
	if err := decideRetry(original, true, true, 3, false); err != nil {
		t.Fatal(err)
	}
	if err := decideRetry(original, false, true, 3, false); err == nil {
		t.Fatal("missing original admitted")
	}
	notUnknown := original
	notUnknown.State = "delivering"
	if err := decideRetry(notUnknown, true, true, 3, false); err == nil {
		t.Fatal("known in-flight original admitted")
	}
	if err := decideRetry(original, true, true, 4, true); !errors.Is(err, ErrPeerChanged) {
		t.Fatalf("stale peer did not take precedence: %v", err)
	}
	if err := decideRetry(original, true, true, 3, true); err == nil {
		t.Fatal("pending retry admitted")
	}
	if err := decideRetry(original, true, false, 4, false); err != nil {
		t.Fatalf("legacy routing gained a generation guard: %v", err)
	}
	at := original.CreatedAt.Add(time.Hour)
	for _, dynamic := range []bool{false, true} {
		fact := planRetryFact(original, dynamic, 4, "new-stable-id", at)
		if fact.ID != "new-stable-id" || fact.State != "queued" || fact.Source != "retry" || fact.RetryOf != "original" || fact.Text != "original text" || fact.Author != "host_owner" || fact.Receipt != "" || fact.Message.Receipt != "" || !fact.ClaimedAt.IsZero() || !fact.CreatedAt.Equal(at) || !fact.UpdatedAt.Equal(at) {
			t.Fatalf("retry candidate: %+v", fact)
		}
		if dynamic {
			if fact.FromRuntime != model.RuntimeClaude || fact.FromGeneration != 7 || fact.TargetGeneration != 3 {
				t.Fatal("dynamic retry retagged the original source or target")
			}
		} else if fact.FromRuntime != "" || fact.FromGeneration != 0 || fact.TargetGeneration != 4 {
			t.Fatal("legacy retry routing changed")
		}
		fact.Attachments[0].ID, fact.Quote.Text, fact.Review.Workspace = "changed", "changed", "changed"
		if original.Attachments[0].ID != "image-original" || original.Quote.Text != "original quote" || original.Review.Workspace != "original-workspace" || original.Receipt != "private-receipt" {
			t.Fatal("retry candidate aliases original metadata")
		}
	}
	for _, state := range []string{"queued", "delivering", "unknown", "handed_off", "cancelled"} {
		candidate := Message{RetryOf: original.ID, State: state}
		want := state == "queued" || state == "delivering"
		if isPendingRetry(candidate, original.ID) != want || isPendingRetry(candidate, "other") {
			t.Fatalf("pending retry state %s", state)
		}
	}
}

func TestLocalAcknowledgementStillReapsBeforeRefusalAndKeepsClockBoundaries(t *testing.T) {
	engine, auth, _ := testEngine(t)
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	calls := 0
	engine.cfg.Now = func() time.Time { calls++; return now }
	message, err := engine.Send(auth[model.ActorSlot1], SendRequest{ID: "clock-message", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := engine.Claim(context.Background(), auth[model.ActorSlot2], false)
	if err != nil {
		t.Fatal(err)
	}
	baseline := traceFacts(t, engine)
	now = now.Add(engine.cfg.Lease)
	calls = 0
	if err := engine.Ack(auth[model.ActorSlot2], message.ID, "forged"); !errors.Is(err, ErrAuth) {
		t.Fatalf("invalid ACK: %v", err)
	}
	delta := traceDelta(t, engine, baseline)
	if len(delta) != 1 || delta[0].State != "unknown" || delta[0].ID != message.ID || delta[0].Actor != string(model.ActorSystem) || calls != 3 {
		t.Fatalf("reap-before-refusal changed: delta=%+v clock calls=%d", delta, calls)
	}
	if engine.messages[message.ID].Receipt != claim.Receipt {
		t.Fatal("expiry discarded original private receipt")
	}
	calls = 0
	if err := engine.Ack(auth[model.ActorSlot2], message.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("handoff clock observations=%d, want auth/reap/materialize/append", calls)
	}
	baseline = traceFacts(t, engine)
	calls = 0
	if err := engine.Ack(auth[model.ActorSlot2], message.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	if delta := traceDelta(t, engine, baseline); len(delta) != 0 || calls != 2 {
		t.Fatalf("no-op ACK allocated a fact/time: delta=%+v calls=%d", delta, calls)
	}
	calls = 0
	if _, err := engine.Retry(message.ID); err == nil {
		t.Fatal("handed-off message was retried")
	}
	if calls != 1 || len(traceDelta(t, engine, baseline)) != 0 {
		t.Fatalf("rejected Retry allocated a fact/time: calls=%d", calls)
	}
}

type failSecondExpiryAppend struct {
	Persistence
	calls int
}

func (s *failSecondExpiryAppend) Append(event *model.Event) error {
	s.calls++
	if s.calls == 2 {
		return errInjectedAppend
	}
	return s.Persistence.Append(event)
}

func TestLocalExpiryPlanRetainsSequentialAppendFailureBoundary(t *testing.T) {
	engine, auth, _ := testEngine(t)
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	engine.cfg.Now = func() time.Time { return now }
	var ids []string
	for _, from := range model.SlotActors() {
		message, err := engine.Send(auth[from], SendRequest{ID: "expiry-" + string(from), Text: "body"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Claim(context.Background(), auth[model.OtherParticipant(from)], false); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, message.ID)
	}
	sort.Strings(ids)
	baseline := traceFacts(t, engine)
	injected := &failSecondExpiryAppend{Persistence: engine.cfg.Store}
	engine.cfg.Store = injected
	now = now.Add(engine.cfg.Lease)
	if err := engine.Reap(); !errors.Is(err, errInjectedAppend) {
		t.Fatalf("second append fault: %v", err)
	}
	delta := traceDelta(t, engine, baseline)
	if len(delta) != 1 || delta[0].ID != ids[0] || delta[0].State != "unknown" || engine.messages[ids[1]].State != "delivering" || engine.Fatal() == nil {
		t.Fatalf("planner changed Local partial-append boundary: delta=%+v first=%s second=%s fatal=%v", delta, engine.messages[ids[0]].State, engine.messages[ids[1]].State, engine.Fatal())
	}
}

func TestLocalActiveBindingStillRequiresCredentialHash(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		for _, hash := range []string{"missing", "", "bad-hash"} {
			payload := map[string]any{"slot": "slot1", "bind_id": "bind-1", "generation": 1, "active": true, "runtime": "claude", "session_id": "official-session", "credential_hash": hash}
			if hash == "missing" {
				delete(payload, "credential_hash")
			}
			event, err := model.NewEvent("room-1", EventBinding, model.ActorSlot1, payload)
			if err != nil {
				t.Fatal(err)
			}
			event.Seq = 1
			storage := &enterpriseBindingHistoryStore{events: []model.Event{event}}
			if _, err := Open(Config{RoomID: "room-1", Store: storage, DynamicBindings: dynamic, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}}); err == nil {
				t.Fatalf("Local active binding accepted hash=%q dynamic=%v", hash, dynamic)
			}
			if storage.appends != 0 {
				t.Fatal("invalid Local identity triggered a recovery append")
			}
		}
	}
}

// Pure planning has no random source: the same supplied ID/time and input
// produce the exact same candidate, including preserved provenance.
func TestDeliveryPlanRetryIsDeterministicForSuppliedIdentity(t *testing.T) {
	message := deliveryPlanningMessage("original")
	message.State = "unknown"
	at := message.CreatedAt.Add(time.Minute)
	first, second := planRetryFact(message, true, 3, "stable-id", at), planRetryFact(message, true, 3, "stable-id", at)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("retry plan generated implicit identity or time")
	}
}
