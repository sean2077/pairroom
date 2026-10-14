package relay

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func publicEnterpriseProjectionFixture(t *testing.T) EnterpriseRoomSnapshot {
	t.Helper()
	first := enterpriseBindingEvent(t, 2, model.ActorSlot1, "binding-one", "agent-one", 1, model.RuntimeClaude, true)
	second := enterpriseBindingEvent(t, 3, model.ActorSlot2, "binding-two", "agent-two", 1, model.RuntimeClaude, true)
	policy, err := model.DecodeEnterpriseBindingUpdate(second)
	if err != nil {
		t.Fatal(err)
	}
	policy.ParkEnabled = false
	second, err = model.NewEnterpriseBindingUpdate(policy, second.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	second.Seq = 3
	message := originalMessageFact(t, "private-claim-message", "send-original", "visible body")
	message.Seq = 4
	var payload messageFact
	if err := json.Unmarshal(message.Data, &payload); err != nil {
		t.Fatal(err)
	}
	payload.State, payload.Receipt, payload.ClaimedAt = "delivering", "private-original-claim-receipt", payload.CreatedAt
	payload.Quote = &model.AgentQuote{FromHandle: "@agent1", Text: "visible quote"}
	message.Data, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return snapshotWithFacts(t, first, second, message)
}

func TestProjectEnterpriseSnapshotPublicJSONNeverDisclosesPrivateReceipt(t *testing.T) {
	input := publicEnterpriseProjectionFixture(t)
	inputBefore, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(inputBefore, []byte("private-original-claim-receipt")) {
		t.Fatal("fixture did not contain a private claim receipt")
	}
	projected, err := ProjectEnterpriseSnapshot(input)
	if err != nil {
		t.Fatal(err)
	}
	if projected.RoomID != "room-1" || projected.Sequence != 4 || !projected.DynamicBindings || projected.Protocol != "pairroom-protocol/v9" || len(projected.Messages) != 1 {
		t.Fatalf("public projection: %+v", projected)
	}
	message := projected.Messages[0]
	if message.Text != "visible body" || message.Quote == nil || message.Quote.Text != "visible quote" || message.State != "delivering" || message.Receipt != "" {
		t.Fatalf("projection leaked a receipt, changed content or reaped delivery: %+v", message)
	}
	if !projected.Bindings[model.ActorSlot1].ParkEnabled || projected.Bindings[model.ActorSlot2].ParkEnabled {
		t.Fatal("projection inferred park policy")
	}
	for _, slot := range model.SlotActors() {
		binding := projected.Bindings[slot]
		if binding.Runtime != model.RuntimeClaude || binding.Generation != 1 || !binding.Active || binding.SessionID != "" || binding.TranscriptPath != "" || binding.RemoteKey != "" {
			t.Fatalf("same-Runtime Binding gained local identity: %+v", binding)
		}
	}
	publicJSON, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private-original-claim-receipt", `"receipt"`, `"credential_hash"`, `"session_id"`, `"transcript_path"`, `"remote_key"`, `"token_hash"`, `"csrf_hash"`} {
		if bytes.Contains(publicJSON, []byte(forbidden)) {
			t.Fatalf("public JSON contains private protocol material %q: %s", forbidden, publicJSON)
		}
	}
	// Results own their mutable metadata. Mutating a view cannot mutate the
	// trusted SQL snapshot or any subsequent view built from it.
	projected.Messages[0].Quote.Text = "mutated"
	delete(projected.Bindings, model.ActorSlot1)
	inputAfter, _ := json.Marshal(input)
	if !bytes.Equal(inputBefore, inputAfter) {
		t.Fatal("pure projection changed the SQL snapshot")
	}
	again, err := ProjectEnterpriseSnapshot(input)
	if err != nil || again.Messages[0].Quote.Text != "visible quote" || len(again.Bindings) != 2 {
		t.Fatal("returned view aliases another projection or its input")
	}
}

func TestProjectEnterpriseSnapshotReusesOpenEnterpriseProjection(t *testing.T) {
	input := publicEnterpriseProjectionFixture(t)
	projected, err := ProjectEnterpriseSnapshot(input)
	if err != nil {
		t.Fatal(err)
	}
	port := &fakePort{snapshot: input}
	engine := enterpriseTestEngine(t, port)
	if !reflect.DeepEqual(projected, engine.Snapshot()) {
		t.Fatal("standalone public view differs from the existing Engine projection")
	}
	if port.commitCalls() != 0 {
		t.Fatal("constructor projection issued a mutation")
	}
}

func TestProjectEnterpriseSnapshotRejectsInvalidOrForeignAuthorityHistory(t *testing.T) {
	tests := map[string]func(*EnterpriseRoomSnapshot){
		"missing creation":        func(s *EnterpriseRoomSnapshot) { s.Events = nil; s.Seq = 0 },
		"sequence gap":            func(s *EnterpriseRoomSnapshot) { s.Events[3].Seq = 9 },
		"foreign Room":            func(s *EnterpriseRoomSnapshot) { s.Events[3].RoomID = "other-room" },
		"wrong deployment":        func(s *EnterpriseRoomSnapshot) { s.DeploymentID = "other-deployment" },
		"unsupported native kind": func(s *EnterpriseRoomSnapshot) { s.Events[3].Kind = "native.unrecognized" },
		"local Binding":           func(s *EnterpriseRoomSnapshot) { s.Events[1].Kind = EventBinding },
		"LAN Binding":             func(s *EnterpriseRoomSnapshot) { s.Events[1].Kind = EventLANMember },
		"native session smuggling": func(s *EnterpriseRoomSnapshot) {
			s.Events[1].Data = []byte(strings.TrimSuffix(string(s.Events[1].Data), "}") + `,"session_id":"private-native-session"}`)
		},
		"credential smuggling": func(s *EnterpriseRoomSnapshot) {
			s.Events[1].Data = []byte(strings.TrimSuffix(string(s.Events[1].Data), "}") + `,"credential_hash":"private-credential"}`)
		},
		"unsupported binding version": func(s *EnterpriseRoomSnapshot) {
			s.Events[1].Data = []byte(strings.Replace(string(s.Events[1].Data), `"version":1`, `"version":999`, 1))
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			input := publicEnterpriseProjectionFixture(t)
			mutate(&input)
			projected, err := ProjectEnterpriseSnapshot(input)
			if err == nil || !reflect.DeepEqual(projected, Snapshot{}) {
				t.Fatalf("invalid history disclosed a partial projection: %+v %v", projected, err)
			}
		})
	}
}
