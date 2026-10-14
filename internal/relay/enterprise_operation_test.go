package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func operationSnapshot(t *testing.T, messages ...Message) EnterpriseRoomSnapshot {
	t.Helper()
	facts := []model.Event{
		enterpriseBindingEvent(t, 2, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeClaude, true),
		enterpriseBindingEvent(t, 3, model.ActorSlot2, "bind-2", "agent-2", 1, model.RuntimeClaude, true),
	}
	for _, message := range messages {
		data, err := json.Marshal(messageFact{Message: message, Receipt: message.Receipt})
		if err != nil {
			t.Fatal(err)
		}
		facts = append(facts, model.Event{ID: "event-" + message.ID, RoomID: "room-1", Seq: uint64(len(facts) + 2), Kind: EventMessage, Actor: message.From, CreatedAt: message.UpdatedAt, Data: data})
	}
	return snapshotWithFacts(t, facts...)
}

func operationCaller(slot model.ActorID) EnterpriseOperationCaller {
	caller := EnterpriseOperationCaller{Actor: slot, UserID: "u-owner", ClientID: "operation-1"}
	if slot.ValidParticipant() {
		caller.BindingID, caller.Generation = "bind-"+strings.TrimPrefix(string(slot), "slot"), 1
	}
	return caller
}

func operationMessage(id, state string) Message {
	at := time.Date(2026, 10, 13, 1, 0, 0, 0, time.UTC)
	message := Message{ID: id, From: model.ActorSlot1, To: model.ActorSlot2, FromRuntime: model.RuntimeClaude, FromGeneration: 1, TargetGeneration: 1, Text: "original full text", State: state, CreatedAt: at, UpdatedAt: at, Source: "send", Author: "original-author"}
	if state == "delivering" || state == "unknown" || state == "handed_off" {
		message.Receipt, message.ClaimedAt = "receipt-"+id, at
	}
	return message
}

func runOperation(t *testing.T, snapshot EnterpriseRoomSnapshot, caller EnterpriseOperationCaller, intent EnterpriseOperationIntent, at time.Time) EnterpriseOperationPlan {
	t.Helper()
	prepare, err := PrepareEnterpriseOperation(snapshot, caller, intent, EnterpriseOperationIDs{MessageID: "new-message", Receipt: "new-receipt"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := prepare(at)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func operationMessageFact(t *testing.T, event model.Event) messageFact {
	t.Helper()
	var fact messageFact
	if event.Kind != EventMessage {
		t.Fatalf("expected message fact: %+v", event)
	}
	if err := json.Unmarshal(event.Data, &fact); err != nil {
		t.Fatal(err)
	}
	return fact
}

func appendOperationPlan(snapshot EnterpriseRoomSnapshot, plan EnterpriseOperationPlan) EnterpriseRoomSnapshot {
	copy := snapshot
	copy.Events = append([]model.Event(nil), snapshot.Events...)
	for _, fact := range plan.Facts {
		fact.Seq = uint64(len(copy.Events) + 1)
		fact.ID = fmt.Sprintf("assigned-%d", fact.Seq)
		copy.Events = append(copy.Events, fact)
	}
	copy.Seq, copy.Revision = uint64(len(copy.Events)), snapshot.Revision+1
	return copy
}

func TestEnterpriseOperationFrozenInputAndRepeatedPreparation(t *testing.T) {
	snapshot := operationSnapshot(t)
	before, _ := json.Marshal(snapshot)
	intent := EnterpriseOperationIntent{Kind: EnterpriseOperationSend, Text: "peer body", ExpectedPeerGeneration: 1}
	prepare, err := PrepareEnterpriseOperation(snapshot, operationCaller(model.ActorSlot1), intent, EnterpriseOperationIDs{MessageID: "fixed-message"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	first, err := prepare(at)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(snapshot)
	if string(before) != string(after) {
		t.Fatal("preparation changed supplied snapshot")
	}
	if len(first.Facts) != 1 || first.Facts[0].ID != "" || first.Facts[0].Seq != 0 || first.Facts[0].CreatedAt != at || first.Result.Message.ID != "fixed-message" || first.Result.Message.FromRuntime != model.RuntimeClaude || first.Result.Message.TargetGeneration != 1 {
		t.Fatalf("unfrozen plan: %+v", first)
	}
	// Caller mutation after capture, and mutation of a returned result, cannot
	// alter the captured history or a later independent plan.
	snapshot.Events[1].Data[0] = '!'
	first.Facts[0].Data[0] = '!'
	first.Result.Message.Text = "mutated result"
	second, err := prepare(at)
	if err != nil || second.Result.Message.Text != "peer body" || !json.Valid(second.Facts[0].Data) {
		t.Fatalf("plan aliased mutable inputs/results: %+v %v", second, err)
	}
	if _, err := prepare(time.Time{}); err == nil {
		t.Fatal("accepted absent locked timestamp")
	}
}

func TestEnterpriseOperationReportBodyPrivacyGapAndPeerContext(t *testing.T) {
	at := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, text string
		peer       uint64
		to         model.ActorID
		outcome    string
	}{
		{"unaddressed", "private unaddressed reply", 1, "", ""},
		{"peer changed", "@agent2 private obsolete reply", 2, "", PeerChangedCode},
		{"peer wins", "@user and @agent2 full reply", 1, model.ActorSlot2, ""},
		{"human", "@user full reply", 0, model.ActorUser, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := runOperation(t, operationSnapshot(t), operationCaller(model.ActorSlot1), EnterpriseOperationIntent{Kind: EnterpriseOperationReport, ReportSeq: 3, Text: tc.text, ExpectedPeerGeneration: tc.peer}, at)
			publication := plan.Result.Publication
			if len(plan.Facts) != 1 || plan.Facts[0].Kind != EventPublicationGap || publication.GapFrom != 1 || publication.GapTo != 2 || publication.Outcome != tc.outcome {
				t.Fatalf("wrong publication receipt: %+v", plan)
			}
			if tc.to == "" {
				if publication.Message != nil || strings.Contains(string(plan.Facts[0].Data), tc.text) {
					t.Fatal("private unrouted text persisted")
				}
				return
			}
			if publication.Message == nil || publication.Message.To != tc.to || publication.Message.Text != tc.text || publication.Message.FromRuntime != model.RuntimeClaude || publication.Message.FromGeneration != 1 {
				t.Fatalf("publication lost stable source/body: %+v", publication)
			}
			if _, err := ProjectEnterpriseSnapshot(appendOperationPlan(operationSnapshot(t), plan)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEnterpriseOperationClaimExpiryAndAckAreOrderedCandidates(t *testing.T) {
	old, next := operationMessage("old", "delivering"), operationMessage("next", "queued")
	snapshot := operationSnapshot(t, old, next)
	at := old.ClaimedAt.Add(DeliveryLease)
	plan := runOperation(t, snapshot, operationCaller(model.ActorSlot2), EnterpriseOperationIntent{Kind: EnterpriseOperationClaim}, at)
	if len(plan.Facts) != 2 || operationMessageFact(t, plan.Facts[0]).State != "unknown" || operationMessageFact(t, plan.Facts[1]).State != "delivering" || operationMessageFact(t, plan.Facts[1]).Receipt != "new-receipt" || plan.Result.Claim.ID != "next" || plan.Result.Claim.PeerGeneration != 1 {
		t.Fatalf("claim did not expire then claim: %+v", plan)
	}
	projected, err := ProjectEnterpriseSnapshot(appendOperationPlan(snapshot, plan))
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(projected)
	if strings.Contains(string(public), "new-receipt") || strings.Contains(string(public), "receipt-old") {
		t.Fatal("public projection disclosed claim receipt")
	}
	// The original receipt still settles an expired delivery when there is no
	// explicit retry. Both expiry and handoff are one SQL candidate batch.
	ack := runOperation(t, operationSnapshot(t, old), operationCaller(model.ActorSlot2), EnterpriseOperationIntent{Kind: EnterpriseOperationAck, MessageID: "old", Receipt: "receipt-old"}, at)
	if len(ack.Facts) != 2 || operationMessageFact(t, ack.Facts[0]).State != "unknown" || operationMessageFact(t, ack.Facts[1]).State != "handed_off" || !ack.Result.HandedOff {
		t.Fatalf("late receipt was not retained: %+v", ack)
	}
	prepare, err := PrepareEnterpriseOperation(snapshot, operationCaller(model.ActorSlot2), EnterpriseOperationIntent{Kind: EnterpriseOperationAck, MessageID: "old", Receipt: "incorrect"}, EnterpriseOperationIDs{})
	if err != nil {
		t.Fatal(err)
	}
	bad, err := prepare(at)
	if !errors.Is(err, ErrAuth) || len(bad.Facts) != 0 {
		t.Fatalf("invalid acknowledgement exposed a partial batch: %+v %v", bad, err)
	}
}

func TestEnterpriseOperationZeroFactOutcomesAndStrictResultShape(t *testing.T) {
	at := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	settled, cancelled := operationMessage("settled", "handed_off"), operationMessage("cancelled", "cancelled")
	for _, tc := range []struct {
		name     string
		snapshot EnterpriseRoomSnapshot
		caller   EnterpriseOperationCaller
		intent   EnterpriseOperationIntent
	}{
		{"empty claim", operationSnapshot(t), operationCaller(model.ActorSlot2), EnterpriseOperationIntent{Kind: EnterpriseOperationClaim}},
		{"settled acknowledgement", operationSnapshot(t, settled), operationCaller(model.ActorSlot2), EnterpriseOperationIntent{Kind: EnterpriseOperationAck, MessageID: "settled", Receipt: "receipt-settled"}},
		{"already cancelled", operationSnapshot(t, cancelled), operationCaller(model.ActorUser), EnterpriseOperationIntent{Kind: EnterpriseOperationCancel, MessageID: "cancelled"}},
		{"unchanged park", operationSnapshot(t), operationCaller(model.ActorSlot1), EnterpriseOperationIntent{Kind: EnterpriseOperationPark, ParkEnabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := runOperation(t, tc.snapshot, tc.caller, tc.intent, at)
			if len(plan.Facts) != 0 {
				t.Fatalf("no-op invented Room facts: %+v", plan.Facts)
			}
			if err := ValidateEnterpriseOperationResult(plan.Result); err != nil {
				t.Fatal(err)
			}
			if plan.Result.Kind != EnterpriseOperationClaim {
				plan.Result.Empty = true
			} else {
				plan.Result.HandedOff = true
			}
			if err := ValidateEnterpriseOperationResult(plan.Result); err == nil {
				t.Fatal("accepted unrelated result fields")
			}
		})
	}
	park := runOperation(t, operationSnapshot(t), operationCaller(model.ActorSlot1), EnterpriseOperationIntent{Kind: EnterpriseOperationPark, ParkEnabled: false}, at)
	if len(park.Facts) != 1 || park.Result.Binding.ParkEnabled || park.Result.Binding.AgentID != "agent-1" || park.Result.Binding.Runtime != model.RuntimeClaude {
		t.Fatalf("park replaced actual binding identity: %+v", park)
	}
	disabled := appendOperationPlan(operationSnapshot(t, operationMessage("head", "queued")), park)
	if claim := runOperation(t, disabled, operationCaller(model.ActorSlot1), EnterpriseOperationIntent{Kind: EnterpriseOperationClaim, ParkEnabled: true}, at); !claim.Result.Empty || len(claim.Facts) != 0 {
		t.Fatal("disabled park invented delivery")
	}
}

func TestEnterpriseOperationRetryPreservesAuthorAndBlocksLateAck(t *testing.T) {
	old := operationMessage("old", "unknown")
	at := old.UpdatedAt.Add(time.Minute)
	snapshot := operationSnapshot(t, old)
	retry := runOperation(t, snapshot, operationCaller(model.ActorUser), EnterpriseOperationIntent{Kind: EnterpriseOperationRetry, MessageID: old.ID}, at)
	message := retry.Result.Message
	if len(retry.Facts) != 1 || message.ID != "new-message" || message.Author != old.Author || message.From != old.From || message.FromRuntime != old.FromRuntime || message.FromGeneration != old.FromGeneration || message.TargetGeneration != old.TargetGeneration || message.RetryOf != old.ID || message.Receipt != "" || !message.ClaimedAt.IsZero() {
		t.Fatalf("retry relabelled provenance or inherited claim: %+v", message)
	}
	snapshot = appendOperationPlan(snapshot, retry)
	prepare, err := PrepareEnterpriseOperation(snapshot, operationCaller(model.ActorSlot2), EnterpriseOperationIntent{Kind: EnterpriseOperationAck, MessageID: old.ID, Receipt: old.Receipt}, EnterpriseOperationIDs{})
	if err != nil {
		t.Fatal(err)
	}
	if plan, err := prepare(at); err == nil || len(plan.Facts) != 0 {
		t.Fatalf("late original acknowledgement settled pending Retry: %+v %v", plan, err)
	}
	// Replacing the source does not relabel retry history; replacing the
	// target does prohibit routing old work to its successor generation.
	for _, slot := range model.SlotActors() {
		t.Run(string(slot), func(t *testing.T) {
			s := operationSnapshot(t, old)
			retire := enterpriseBindingEvent(t, s.Seq+1, slot, "bind-"+strings.TrimPrefix(string(slot), "slot"), "agent-"+strings.TrimPrefix(string(slot), "slot"), 1, model.RuntimeClaude, false)
			replace := enterpriseBindingEvent(t, s.Seq+2, slot, "new-bind", "new-agent", 2, model.RuntimeCodex, true)
			s.Events = append(s.Events, retire, replace)
			s.Seq += 2
			s.Revision += 2
			prepare, err := PrepareEnterpriseOperation(s, operationCaller(model.ActorUser), EnterpriseOperationIntent{Kind: EnterpriseOperationRetry, MessageID: old.ID}, EnterpriseOperationIDs{MessageID: "retry-after-replacement"})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := prepare(at)
			if slot == model.ActorSlot2 {
				if !errors.Is(err, ErrPeerChanged) || len(plan.Facts) != 0 {
					t.Fatalf("retagged old target: %+v %v", plan, err)
				}
				return
			}
			if err != nil || plan.Result.Message.FromRuntime != model.RuntimeClaude || plan.Result.Message.FromGeneration != 1 {
				t.Fatalf("source history was relabelled: %+v %v", plan, err)
			}
			if _, err := ProjectEnterpriseSnapshot(appendOperationPlan(s, plan)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEnterpriseOperationRejectsForeignCallerAndMalformedShapes(t *testing.T) {
	valid := []EnterpriseOperationIntent{
		{Kind: EnterpriseOperationSend, Text: "hello"}, {Kind: EnterpriseOperationReport, ReportSeq: 1}, {Kind: EnterpriseOperationClaim},
		{Kind: EnterpriseOperationAck, MessageID: "message", Receipt: "receipt"}, {Kind: EnterpriseOperationRetry, MessageID: "message"},
		{Kind: EnterpriseOperationCancel, MessageID: "message"}, {Kind: EnterpriseOperationPark},
	}
	for _, intent := range valid {
		if err := ValidateEnterpriseOperationIntent(intent); err != nil {
			t.Fatal(err)
		}
		bad := intent
		if intent.Kind == EnterpriseOperationAck {
			bad.Text = "unrelated"
		} else {
			bad.Receipt = "unrelated"
		}
		if err := ValidateEnterpriseOperationIntent(bad); err == nil {
			t.Fatalf("accepted unrelated intent: %+v", bad)
		}
	}
	for _, bad := range []EnterpriseOperationIntent{{Kind: "raw-event"}, {Kind: EnterpriseOperationReport}, {Kind: EnterpriseOperationSend, Text: " "}, {Kind: EnterpriseOperationAck, MessageID: "bad\xff", Receipt: "receipt"}, {Kind: EnterpriseOperationRetry, MessageID: "bad\tcontrol"}} {
		if err := ValidateEnterpriseOperationIntent(bad); err == nil {
			t.Fatalf("accepted malformed intent: %+v", bad)
		}
	}
	caller := operationCaller(model.ActorSlot1)
	caller.BindingID = "foreign-binding"
	prepare, err := PrepareEnterpriseOperation(operationSnapshot(t), caller, valid[0], EnterpriseOperationIDs{MessageID: "id"})
	if err != nil {
		t.Fatal(err)
	} // Mutable scope checks happen only in Prepare.
	if _, err := prepare(time.Now().UTC()); !errors.Is(err, ErrAuth) {
		t.Fatalf("foreign binding accepted: %v", err)
	}
	if _, err := PrepareEnterpriseOperation(operationSnapshot(t), operationCaller(model.ActorUser), EnterpriseOperationIntent{Kind: EnterpriseOperationClaim}, EnterpriseOperationIDs{}); !errors.Is(err, ErrAuth) {
		t.Fatalf("human invented collector authority: %v", err)
	}
}

func TestEnterpriseOwnershipHistoryDescribesWithoutGrant(t *testing.T) {
	at := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	fact, err := model.NewEnterpriseOwnershipChange("room-1", 2, model.EnterpriseOwnershipChange{Version: 1, FromUserID: "u-owner", ToUserID: "u-next", Governance: true}, at)
	if err != nil {
		t.Fatal(err)
	}
	engine := enterpriseTestEngine(t, &fakePort{snapshot: snapshotWithFacts(t, fact)})
	if len(engine.bindings) != 0 || len(engine.messages) != 0 || !reflect.DeepEqual(engine.Runtimes(), map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeAwaitingBinding, model.ActorSlot2: model.RuntimeAwaitingBinding}) {
		t.Fatal("ownership history manufactured Relay authority")
	}
	for _, data := range []string{
		`{"version":2,"from_user_id":"u-owner","to_user_id":"u-next","governance":true}`,
		`{"version":1,"from_user_id":"u-owner","to_user_id":"u-next"}`,
		`{"version":1,"from_user_id":"u-owner","to_user_id":"u-next","governance":"true"}`,
		`{"version":1,"from_user_id":"u-owner","to_user_id":"u-next","governance":true,"admin":true}`,
	} {
		bad := fact
		bad.Data = []byte(data)
		if _, err := ProjectEnterpriseSnapshot(snapshotWithFacts(t, bad)); err == nil {
			t.Fatalf("accepted malformed ownership history: %s", data)
		}
	}
	local := emptyEngine(Config{RoomID: "room-1"})
	if err := local.apply(fact); err == nil || local.sequence != 0 || len(local.bindings) != 0 {
		t.Fatal("local replay accepted enterprise authority history")
	}
}

func TestEnterpriseOperationRetirementUsesOneFactAndSharedDeliveryProjection(t *testing.T) {
	queued, delivering := operationMessage("queued", "queued"), operationMessage("delivering", "delivering")
	snapshot := operationSnapshot(t, queued, delivering)
	before, _ := json.Marshal(snapshot)
	prepare, err := PrepareEnterpriseBindingRetirement(snapshot, "bind-2", "device.revoked", "retire-fixed-id")
	if err != nil {
		t.Fatal(err)
	}
	at := delivering.UpdatedAt.Add(time.Second)
	fact, err := prepare(at)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := model.DecodeEnterpriseBindingUpdate(fact)
	if err != nil || fact.ID != "retire-fixed-id" || fact.Seq != 0 || identity.Active || identity.AgentID != "agent-2" || identity.Runtime != model.RuntimeClaude || identity.Generation != 1 || !identity.ParkEnabled || identity.RetiredReason != "device.revoked" || identity.RetiredAt == nil || !identity.RetiredAt.Equal(at) {
		t.Fatalf("retirement did not freeze actual binding: %+v %v", identity, err)
	}
	after, _ := json.Marshal(snapshot)
	if string(before) != string(after) {
		t.Fatal("retirement mutated its source snapshot")
	}
	result, err := ProjectEnterpriseSnapshot(appendOperationPlan(snapshot, EnterpriseOperationPlan{Facts: []model.Event{fact}}))
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, message := range result.Messages {
		states[message.ID] = message.State
	}
	if states["queued"] != "cancelled" || states["delivering"] != "unknown" || result.Bindings[model.ActorSlot2].Active || result.Sequence != snapshot.Seq+1 {
		t.Fatalf("single retirement failed shared delivery effects: %+v", result)
	}
	retiredSnapshot := appendOperationPlan(snapshot, EnterpriseOperationPlan{Facts: []model.Event{fact}})
	duplicate := fact
	duplicate.ID = "retirement-duplicate"
	replayed, err := ProjectEnterpriseSnapshot(appendOperationPlan(retiredSnapshot, EnterpriseOperationPlan{Facts: []model.Event{duplicate}}))
	if err != nil || !reflect.DeepEqual(result.Messages, replayed.Messages) {
		t.Fatalf("same retirement applied delivery effects twice: %+v %v", replayed.Messages, err)
	}
	for _, id := range []string{"missing", "bind-2"} {
		prepare, err := PrepareEnterpriseBindingRetirement(retiredSnapshot, id, "user.revoked", "other-event")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prepare(at); err == nil {
			t.Fatalf("retirement accepted absent/inactive binding %q", id)
		}
	}
	prepare, err = PrepareEnterpriseBindingRetirement(snapshot, "bind-2", "", "other-event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(at); err == nil {
		t.Fatal("retirement silently defaulted its reason")
	}
}

func TestEnterpriseOperationRejectsMalformedAcceptedResult(t *testing.T) {
	plan := runOperation(t, operationSnapshot(t), operationCaller(model.ActorSlot1), EnterpriseOperationIntent{Kind: EnterpriseOperationSend, Text: "body", ExpectedPeerGeneration: 1}, time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC))
	for _, mutate := range []func(*Message){
		func(m *Message) { m.Receipt = "private" }, func(m *Message) { m.State = "delivering" },
		func(m *Message) { m.Source = "stop" }, func(m *Message) { m.FromGeneration = 0 },
		func(m *Message) { m.To = model.ActorSlot1 }, func(m *Message) { m.TargetGeneration = 0 },
		func(m *Message) { m.ClaimedAt = m.CreatedAt }, func(m *Message) { m.ID = "invalid\xff" },
	} {
		copy := *plan.Result.Message
		mutate(&copy)
		result := plan.Result
		result.Message = &copy
		if err := ValidateEnterpriseOperationResult(result); err == nil {
			t.Fatalf("accepted corrupted original result: %+v", result)
		}
	}
}

func TestEnterpriseGrantSelectorsDoNotFallBackToHumanAuthority(t *testing.T) {
	for _, auth := range []EnterpriseAuth{
		{Deployment: "dep-test", UserID: "u-owner", DeviceGrantID: "device-grant"},
		{Deployment: "dep-test", UserID: "u-owner", BindingGrantID: "binding-grant"},
	} {
		port := &fakePort{snapshot: emptySnapshot(t)}
		port.commitFn = func(command EnterpriseSendCommand) (EnterpriseSendResult, error) {
			if command.Auth != auth {
				t.Fatalf("grant selector silently became human authority: %+v", command.Auth)
			}
			return EnterpriseSendResult{}, &EnterpriseCommandError{Outcome: EnterpriseOutcomeRefused, Class: EnterpriseClassAuthorization}
		}
		engine := enterpriseTestEngine(t, port)
		_, err := engine.ExecuteHumanSend(context.Background(), auth, SendRequest{ID: "not-a-human-send", Text: "body", To: model.ActorSlot2})
		if err == nil || port.commitCalls() != 1 || engine.Sequence() != 1 || len(engine.Snapshot().Messages) != 0 {
			t.Fatalf("selector refusal did not preserve empty projection: %v", err)
		}
	}
}
