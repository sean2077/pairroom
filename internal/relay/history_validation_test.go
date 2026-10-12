package relay

import (
	"encoding/json"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestValidateNativeHistoryDoesNotChangeLegacyLANSelections(t *testing.T) {
	e, _, _ := lanEngine(t, nil)
	admitLANRuntime(t, e, model.RuntimeCodex)
	events, err := e.cfg.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	initial := map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeAwaitingPeer}
	before := e.Sequence()
	if err := ValidateNativeHistory("room", events, initial, model.ActorSlot2, false); err != nil {
		t.Fatal(err)
	}
	if initial[model.ActorSlot2] != model.RuntimeAwaitingPeer || e.Sequence() != before {
		t.Fatal("read-only validation rewrote a selection or appended a fact")
	}
}

func TestValidateNativeHistoryRejectsInvalidDynamicBindingTransitions(t *testing.T) {
	for _, name := range []string{"session mutation", "credential mutation", "runtime mutation", "generation skip", "revoked reactivation", "reused bind ID"} {
		t.Run(name, func(t *testing.T) {
			e, _ := dynamicEngine(t, "")
			a := dynamicBind(t, e, "", dynamicRequest("original", model.RuntimeClaude))
			fact := e.bindings[a.Slot]
			switch name {
			case "session mutation":
				fact.SessionID = "different-session"
			case "credential mutation":
				fact.CredentialHash = Digest("different-secret")
			case "runtime mutation":
				fact.Runtime = model.RuntimeCodex
			case "generation skip":
				fact.BindID, fact.Generation = "new-bind", 3
			case "revoked reactivation":
				if err := e.UnbindAs(a); err != nil {
					t.Fatal(err)
				}
			case "reused bind ID":
				fact.Slot, fact.SessionID = model.ActorSlot2, "independent-session"
			}
			events, err := e.cfg.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateNativeHistory("dynamic-room", events, nil, "", true); err != nil {
				t.Fatalf("valid prefix rejected: %v", err)
			}
			invalid, err := model.NewEvent("dynamic-room", EventBinding, fact.Slot, fact)
			if err != nil {
				t.Fatal(err)
			}
			invalid.Seq = e.Sequence() + 1
			events = append(events, invalid)
			if err := ValidateNativeHistory("dynamic-room", events, nil, "", true); err == nil {
				t.Fatalf("read-only replay accepted %s", name)
			}
			if e.Sequence() != invalid.Seq-1 {
				t.Fatal("rejected history changed live state")
			}
		})
	}
}

func TestValidateNativeHistoryRejectsDynamicMessageSourceChanges(t *testing.T) {
	e, _ := dynamicEngine(t, "")
	a := dynamicBind(t, e, "", dynamicRequest("author", model.RuntimeClaude))
	if _, err := e.Send(a, SendRequest{ID: "message", Text: "original author", ExpectedPeerGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	events, err := e.cfg.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateNativeHistory("dynamic-room", events, nil, "", true); err != nil {
		t.Fatal(err)
	}
	last := &events[len(events)-1]
	var fact messageFact
	if err := json.Unmarshal(last.Data, &fact); err != nil {
		t.Fatal(err)
	}
	fact.FromRuntime = model.RuntimeCodex
	last.Data, err = json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateNativeHistory("dynamic-room", events, nil, "", true); err == nil {
		t.Fatal("read-only replay accepted a source Runtime different from its binding")
	}
}
