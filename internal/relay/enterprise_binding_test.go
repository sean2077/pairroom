package relay

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func enterpriseBindingEvent(t *testing.T, seq uint64, slot model.ActorID, bindID, agentID string, generation uint64, runtime model.RuntimeKind, active bool) model.Event {
	t.Helper()
	at := time.Date(2026, 10, 13, 0, 0, int(seq), 123456789, time.UTC)
	binding := model.EnterpriseBinding{Version: 1, RoomID: "room-1", Slot: slot, BindID: bindID, Generation: generation, AgentID: agentID, Runtime: runtime, Active: active, ParkEnabled: true}
	if !active {
		binding.RetiredAt, binding.RetiredReason = &at, "explicit_unbind"
	}
	event, err := model.NewEnterpriseBindingUpdate(binding, at)
	if err != nil {
		t.Fatal(err)
	}
	event.Seq = seq
	return event
}

func TestEnterpriseBindingPreservesExplicitParkPolicyWithoutGrant(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		event := enterpriseBindingEvent(t, 2, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeClaude, true)
		binding, err := model.DecodeEnterpriseBindingUpdate(event)
		if err != nil {
			t.Fatal(err)
		}
		binding.ParkEnabled = enabled
		event, err = model.NewEnterpriseBindingUpdate(binding, event.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}
		event.Seq = 2
		port := &fakePort{snapshot: snapshotWithFacts(t, event)}
		engine := enterpriseTestEngine(t, port)
		if engine.bindings[model.ActorSlot1].ParkEnabled != enabled || engine.Snapshot().Bindings[model.ActorSlot1].ParkEnabled != enabled {
			t.Fatalf("park policy %v was replaced by a default", enabled)
		}
		if err := engine.rebuildEnterpriseLocked(context.Background(), whiteBoxAuth("u-owner")); err != nil {
			t.Fatal(err)
		}
		if engine.bindings[model.ActorSlot1].ParkEnabled != enabled {
			t.Fatal("rebuild changed the frozen park policy")
		}
		if _, err := engine.Claim(context.Background(), Auth{Slot: model.ActorSlot1, BindID: "bind-1", Generation: 1, Runtime: model.RuntimeClaude}, true); !errors.Is(err, ErrEnterpriseReadOnly) {
			t.Fatalf("park policy invented delivery authority: %v", err)
		}
		if err := engine.ReserveWake("message", model.ActorSlot1); !errors.Is(err, ErrEnterpriseReadOnly) {
			t.Fatalf("park policy invented wake authority: %v", err)
		}
		if port.commitCalls() != 0 {
			t.Fatal("park policy replay performed a mutation")
		}
	}
}

func TestEnterpriseBindingReplayUsesAgentIdentityWithoutGrant(t *testing.T) {
	first := enterpriseBindingEvent(t, 2, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeClaude, true)
	second := enterpriseBindingEvent(t, 3, model.ActorSlot2, "bind-2", "agent-2", 1, model.RuntimeClaude, true)
	port := &fakePort{snapshot: snapshotWithFacts(t, first, second)}
	engine := enterpriseTestEngine(t, port)
	for _, slot := range model.SlotActors() {
		binding := engine.bindings[slot]
		if !binding.Active || binding.Generation != 1 || binding.Runtime != model.RuntimeClaude || binding.enterprise == nil || binding.enterprise.agentID == "" {
			t.Fatalf("binding %s: %+v", slot, binding)
		}
		if binding.SessionID != "" || binding.CredentialHash != "" || binding.RemoteKey != "" || binding.TranscriptPath != "" {
			t.Fatalf("enterprise replay invented native identity: %+v", binding)
		}
		// A replayed historical identity never satisfies Local credential auth.
		if _, err := engine.Inspect(Auth{Slot: slot, BindID: binding.BindID, Generation: 1, Runtime: model.RuntimeClaude}); !errors.Is(err, ErrAuth) {
			t.Fatalf("replayed binding acted as an auth grant: %v", err)
		}
	}
	if port.commitCalls() != 0 {
		t.Fatal("binding replay performed a SQL mutation")
	}
}

func TestEnterpriseBindingRejectsGenerationAndIdentityChanges(t *testing.T) {
	first := enterpriseBindingEvent(t, 2, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeClaude, true)
	retired := enterpriseBindingEvent(t, 3, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeClaude, false)
	changedRetirement := retired
	changedRetirement.Seq = 4
	var retiredPayload model.EnterpriseBinding
	if err := json.Unmarshal(retired.Data, &retiredPayload); err != nil {
		t.Fatal(err)
	}
	retiredPayload.RetiredReason = "different_reason"
	changedRetirement.Data, _ = json.Marshal(retiredPayload)
	cases := map[string][]model.Event{
		"first generation skipped":       {enterpriseBindingEvent(t, 2, model.ActorSlot1, "bind-1", "agent-1", 2, model.RuntimeClaude, true)},
		"first generation retired":       {enterpriseBindingEvent(t, 2, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeClaude, false)},
		"same generation new binding":    {first, enterpriseBindingEvent(t, 3, model.ActorSlot1, "bind-other", "agent-1", 1, model.RuntimeClaude, true)},
		"same generation new agent":      {first, enterpriseBindingEvent(t, 3, model.ActorSlot1, "bind-1", "agent-other", 1, model.RuntimeClaude, true)},
		"same generation new runtime":    {first, enterpriseBindingEvent(t, 3, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeCodex, true)},
		"generation jump":                {first, enterpriseBindingEvent(t, 3, model.ActorSlot1, "bind-new", "agent-2", 3, model.RuntimeCodex, true)},
		"new generation retired":         {first, enterpriseBindingEvent(t, 3, model.ActorSlot1, "bind-new", "agent-2", 2, model.RuntimeCodex, false)},
		"binding id reused":              {first, enterpriseBindingEvent(t, 3, model.ActorSlot1, "bind-1", "agent-1", 2, model.RuntimeClaude, true)},
		"agent in both slots":            {first, enterpriseBindingEvent(t, 3, model.ActorSlot2, "bind-2", "agent-1", 1, model.RuntimeClaude, true)},
		"retired generation resurrected": {first, retired, enterpriseBindingEvent(t, 4, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeClaude, true)},
		"retirement changed":             {first, retired, enterpriseBindingEvent(t, 4, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeClaude, false)},
		"retirement reason changed":      {first, retired, changedRetirement},
		"old id on different slot":       {first, retired, enterpriseBindingEvent(t, 4, model.ActorSlot2, "bind-1", "agent-2", 1, model.RuntimeClaude, true)},
		"generation regressed":           {first, enterpriseBindingEvent(t, 3, model.ActorSlot1, "bind-2", "agent-2", 2, model.RuntimeCodex, true), enterpriseBindingEvent(t, 4, model.ActorSlot1, "bind-3", "agent-3", 1, model.RuntimeClaude, true)},
	}
	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			port := &fakePort{snapshot: snapshotWithFacts(t, events...)}
			if _, err := OpenEnterprise(context.Background(), Config{RoomID: "room-1"}, port, whiteBoxAuth("u-owner")); err == nil {
				t.Fatal("invalid enterprise binding history opened")
			}
			if port.commitCalls() != 0 {
				t.Fatal("invalid history performed a mutation")
			}
		})
	}
}

func TestEnterpriseBindingInvalidRebuildPreservesOriginalProjection(t *testing.T) {
	first := enterpriseBindingEvent(t, 2, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeClaude, true)
	port := &fakePort{snapshot: snapshotWithFacts(t, first)}
	engine := enterpriseTestEngine(t, port)
	before := engine.Snapshot()
	changed := enterpriseBindingEvent(t, 3, model.ActorSlot1, "bind-1", "agent-other", 1, model.RuntimeClaude, true)
	port.snapshot = snapshotWithFacts(t, first, changed)
	if err := engine.rebuildEnterpriseLocked(context.Background(), whiteBoxAuth("u-owner")); err == nil {
		t.Fatal("changed same-generation identity rebuilt")
	}
	if !reflect.DeepEqual(before, engine.Snapshot()) || engine.bindings[model.ActorSlot1].enterprise.agentID != "agent-1" || port.commitCalls() != 0 {
		t.Fatal("invalid Binding rebuild changed the original projection or issued a mutation")
	}
}

func TestEnterpriseBindingRetirementAndFreshGeneration(t *testing.T) {
	first := enterpriseBindingEvent(t, 2, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeClaude, true)
	retired := enterpriseBindingEvent(t, 3, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeClaude, false)
	port := &fakePort{snapshot: snapshotWithFacts(t, first, retired)}
	engine := enterpriseTestEngine(t, port)
	if engine.bindings[model.ActorSlot1].Active || engine.Runtimes()[model.ActorSlot1] != model.RuntimeAwaitingBinding || engine.generation(model.ActorSlot1) != 2 {
		t.Fatal("retirement did not retain history and expose the next vacant generation")
	}
	// The same Agent may join after explicit retirement, with a fresh binding
	// identity and exactly the next generation. History is never relabeled.
	next := enterpriseBindingEvent(t, 4, model.ActorSlot1, "bind-2", "agent-1", 2, model.RuntimeCodex, true)
	port.snapshot = snapshotWithFacts(t, first, retired, next)
	if err := engine.rebuildEnterpriseLocked(context.Background(), whiteBoxAuth("u-owner")); err != nil {
		t.Fatal(err)
	}
	if b := engine.bindings[model.ActorSlot1]; !b.Active || b.BindID != "bind-2" || b.Generation != 2 || b.Runtime != model.RuntimeCodex || b.enterprise.agentID != "agent-1" {
		t.Fatalf("fresh generation not replayed: %+v", b)
	}
	if !engine.seenBinds["bind-1"] || port.commitCalls() != 0 {
		t.Fatal("rebuild lost old identity or wrote state")
	}
}

func TestEnterpriseBindingReplacementSharesInboxProjectionOnce(t *testing.T) {
	bound := enterpriseBindingEvent(t, 2, model.ActorSlot2, "bind-1", "agent-1", 1, model.RuntimeClaude, true)
	queued := originalMessageFact(t, "queued-1", "queued", "queued body")
	queued.Seq = 3
	delivering := originalMessageFact(t, "delivery-1", "delivery", "delivering body")
	delivering.Seq = 4
	var payload messageFact
	if err := json.Unmarshal(delivering.Data, &payload); err != nil {
		t.Fatal(err)
	}
	payload.State, payload.Receipt, payload.ClaimedAt = "delivering", "original-receipt", delivering.CreatedAt
	delivering.Data, _ = json.Marshal(payload)
	port := &fakePort{snapshot: snapshotWithFacts(t, bound, queued, delivering)}
	appended, attention := 0, 0
	engine, err := OpenEnterprise(context.Background(), Config{RoomID: "room-1", OnAppend: func(model.Event) { appended++ }, OnAttention: func(Attention) { attention++ }}, port, whiteBoxAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	replacement := enterpriseBindingEvent(t, 5, model.ActorSlot2, "bind-2", "agent-2", 2, model.RuntimeCodex, true)
	if err := engine.apply(replacement); err != nil {
		t.Fatal(err)
	}
	if engine.messages["queued-1"].State != "cancelled" || engine.messages["delivery-1"].State != "unknown" || engine.messages["delivery-1"].Receipt != "original-receipt" {
		t.Fatalf("replacement changed wrong delivery state: %+v", engine.messages)
	}
	if len(engine.derived) != 1 || engine.derived[0].Key != "delivery-1" || engine.derived[0].Kind != AttentionDeliveryUncertain || len(engine.messages) != 2 || engine.Sequence() != 5 {
		t.Fatalf("replacement produced duplicate facts/attention: derived=%+v messages=%d seq=%d", engine.derived, len(engine.messages), engine.Sequence())
	}
	engine.derived = nil
	repeated := replacement
	repeated.Seq = 6
	repeated.ID = "evt-binding-observed-again"
	if err := engine.apply(repeated); err != nil {
		t.Fatal(err)
	}
	if len(engine.derived) != 0 {
		t.Fatal("identical binding observation repeated derived uncertainty")
	}
	// The constructor and rebuild use this same application logic without
	// emitting callbacks or a second message update fact.
	port.snapshot = snapshotWithFacts(t, bound, queued, delivering, replacement, repeated)
	if err := engine.rebuildEnterpriseLocked(context.Background(), whiteBoxAuth("u-owner")); err != nil {
		t.Fatal(err)
	}
	if engine.messages["queued-1"].State != "cancelled" || engine.messages["delivery-1"].State != "unknown" || len(engine.derived) != 0 || appended != 0 || attention != 0 || port.commitCalls() != 0 {
		t.Fatal("read-only replay changed transition semantics or emitted effects")
	}
}

type enterpriseBindingHistoryStore struct {
	events  []model.Event
	appends int
}

func (s *enterpriseBindingHistoryStore) Append(*model.Event) error {
	s.appends++
	return errors.New("unexpected append")
}
func (s *enterpriseBindingHistoryStore) Load() ([]model.Event, error) { return s.events, nil }
func (*enterpriseBindingHistoryStore) Close() error                   { return nil }

func TestBindingFactCodecsCannotCrossAuthorityModes(t *testing.T) {
	enterprise := enterpriseBindingEvent(t, 2, model.ActorSlot1, "bind-1", "agent-1", 1, model.RuntimeClaude, true)
	for _, shared := range []model.ActorID{"", model.ActorSlot2} {
		storage := &enterpriseBindingHistoryStore{events: []model.Event{enterprise}}
		if _, err := Open(Config{RoomID: "room-1", Store: storage, DynamicBindings: true, SharedSlot: shared}); err == nil {
			t.Fatal("Local/LAN accepted enterprise binding codec")
		}
		if storage.appends != 0 {
			t.Fatal("rejected foreign binding caused a reap append")
		}
	}
	legacy, err := model.NewEvent("room-1", EventBinding, model.ActorSlot1, bindingFact{Binding: Binding{Slot: model.ActorSlot1, BindID: "local-bind", Runtime: model.RuntimeClaude, Generation: 1, Active: true, SessionID: "private-session"}, CredentialHash: Digest("private-secret")})
	if err != nil {
		t.Fatal(err)
	}
	legacy.Seq = 2
	for _, kind := range []string{EventBinding, EventLANInvite, EventLANJoin, EventLANMember} {
		t.Run(kind, func(t *testing.T) {
			foreign := legacy
			foreign.Kind = kind
			port := &fakePort{snapshot: snapshotWithFacts(t, foreign)}
			if _, err := OpenEnterprise(context.Background(), Config{RoomID: "room-1"}, port, whiteBoxAuth("u-owner")); err == nil {
				t.Fatal("enterprise accepted Local/LAN authority history")
			}
			if port.commitCalls() != 0 {
				t.Fatal("foreign authority replay wrote state")
			}
		})
	}
}

func TestEnterpriseMembershipHistoryIsStrictAndDoesNotAuthorizeBindings(t *testing.T) {
	fact, err := model.NewEnterpriseMembershipChange("room-1", 2, model.EnterpriseMembershipIntent{TargetUserID: "u-member", Action: model.EnterpriseMembershipAdmit, Role: model.EnterpriseRoomMember}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	port := &fakePort{snapshot: snapshotWithFacts(t, fact)}
	engine := enterpriseTestEngine(t, port)
	if len(engine.bindings) != 0 || engine.Sequence() != 2 || !reflect.DeepEqual(engine.Runtimes(), map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeAwaitingBinding, model.ActorSlot2: model.RuntimeAwaitingBinding}) || port.commitCalls() != 0 {
		t.Fatal("membership history invented a binding or changed runtimes")
	}
	for name, data := range map[string]string{
		"future version": `{"version":999,"target_user_id":"u-member","action":"admit","role":"member"}`,
		"wrong type":     `{"version":1,"target_user_id":true,"action":"admit","role":"member"}`,
		"unknown field":  `{"version":1,"target_user_id":"u-member","action":"admit","role":"member","admin":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			bad := fact
			bad.Data = []byte(data)
			port.snapshot = snapshotWithFacts(t, bad)
			if _, err := OpenEnterprise(context.Background(), Config{RoomID: "room-1"}, port, whiteBoxAuth("u-owner")); err == nil {
				t.Fatal("invalid membership history opened")
			}
			if len(engine.bindings) != 0 || engine.Runtimes()[model.ActorSlot1] != model.RuntimeAwaitingBinding || port.commitCalls() != 0 {
				t.Fatal("invalid history changed authority projection")
			}
		})
	}
}
