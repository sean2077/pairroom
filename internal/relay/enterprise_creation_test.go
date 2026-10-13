package relay

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
)

func TestOpenEnterpriseCreationDerivesVacantNativeV9(t *testing.T) {
	port := &fakePort{snapshot: emptySnapshot(t)}
	engine := enterpriseTestEngine(t, port)
	snapshot := engine.Snapshot()
	if !snapshot.DynamicBindings || snapshot.Protocol != protocol.NativeDynamicVersion || snapshot.Sequence != 1 || len(snapshot.Messages) != 0 {
		t.Fatalf("creation projection: %+v", snapshot)
	}
	for _, slot := range model.SlotActors() {
		if engine.Runtimes()[slot] != model.RuntimeAwaitingBinding || snapshot.TargetGenerations[slot] != 1 {
			t.Fatalf("vacant slot %s runtime=%s generation=%d", slot, engine.Runtimes()[slot], snapshot.TargetGenerations[slot])
		}
		if binding := snapshot.Bindings[slot]; binding.Active || binding.Generation != 0 || binding.SessionID != "" || binding.BindID != "" || binding.Runtime != model.RuntimeAwaitingBinding {
			t.Fatalf("creation fabricated a Binding for %s: %+v", slot, binding)
		}
	}
	if len(engine.bindings) != 0 {
		t.Fatal("creation allocated Binding rows")
	}
	message, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner"), SendRequest{ID: "first", To: model.ActorSlot2, Text: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	if message.TargetGeneration != 1 || engine.Sequence() != 2 {
		t.Fatalf("first send lost vacant target generation: %+v", message)
	}
}

func TestOpenEnterpriseRejectsCreationAndTrustedMetadataCorruption(t *testing.T) {
	for name, mutate := range map[string]func(*EnterpriseRoomSnapshot){
		"missing creation": func(s *EnterpriseRoomSnapshot) { s.Events = nil; s.Seq = 0 },
		"message instead of creation": func(s *EnterpriseRoomSnapshot) {
			event := originalMessageFact(t, "relay-missing-creation", "missing", "x")
			event.Seq = 1
			s.Events[0] = event
		},
		"duplicate creation": func(s *EnterpriseRoomSnapshot) {
			second := s.Events[0]
			second.Seq = 2
			s.Events = append(s.Events, second)
			s.Seq = 2
		},
		"trusted deployment mismatch": func(s *EnterpriseRoomSnapshot) { s.DeploymentID = "other" },
		"trusted org mismatch":        func(s *EnterpriseRoomSnapshot) { s.OrgID = "other" },
		"trusted owner mismatch":      func(s *EnterpriseRoomSnapshot) { s.CreationUserID = "other" },
		"legacy creation": func(s *EnterpriseRoomSnapshot) {
			creation, err := model.DecodeEnterpriseRoomCreation(s.Events[0])
			if err != nil {
				t.Fatal(err)
			}
			s.Events[0].Data, err = json.Marshal(creation.RoomMeta)
			if err != nil {
				t.Fatal(err)
			}
		},
		"unsupported origin": func(s *EnterpriseRoomSnapshot) {
			creation, err := model.DecodeEnterpriseRoomCreation(s.Events[0])
			if err != nil {
				t.Fatal(err)
			}
			creation.Enterprise.Version = 2
			s.Events[0].Data, err = json.Marshal(creation)
			if err != nil {
				t.Fatal(err)
			}
		},
		"malformed creation": func(s *EnterpriseRoomSnapshot) { s.Events[0].Data = []byte(`{broken`) },
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := emptySnapshot(t)
			mutate(&snapshot)
			port := &fakePort{snapshot: snapshot}
			if _, err := OpenEnterprise(context.Background(), Config{RoomID: "room-1"}, port, whiteBoxAuth("u-owner")); err == nil {
				t.Fatal("invalid creation opened")
			}
			if port.commitCalls() != 0 {
				t.Fatal("invalid constructor performed a mutation")
			}
		})
	}
}

func TestOpenEnterpriseRefusesRuntimeAndLANOverrides(t *testing.T) {
	for name, cfg := range map[string]Config{
		"LAN slot":         {RoomID: "room-1", SharedSlot: model.ActorSlot2},
		"concrete Runtime": {RoomID: "room-1", Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeCodex}},
		"awaiting peer":    {RoomID: "room-1", Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot2: model.RuntimeAwaitingPeer}},
		"unknown slot":     {RoomID: "room-1", Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorUser: model.RuntimeAwaitingBinding}},
	} {
		t.Run(name, func(t *testing.T) {
			port := &fakePort{snapshot: emptySnapshot(t), snapFn: func(EnterpriseAuth) (EnterpriseRoomSnapshot, error) {
				t.Fatal("invalid configuration reached the port")
				return EnterpriseRoomSnapshot{}, nil
			}}
			if _, err := OpenEnterprise(context.Background(), cfg, port, whiteBoxAuth("u-owner")); err == nil {
				t.Fatal("configuration override accepted")
			}
		})
	}
}

func TestEnterpriseRebuildRejectsChangedCreationWithoutAdopting(t *testing.T) {
	for _, changeBothControlAndFact := range []bool{false, true} {
		t.Run(map[bool]string{false: "fact-only", true: "fact-and-control"}[changeBothControlAndFact], func(t *testing.T) {
			original := originalMessageFact(t, "relay-kept-origin", "kept", "keep")
			port := &fakePort{snapshot: snapshotWithFacts(t, original)}
			engine := enterpriseTestEngine(t, port)
			bad := snapshotWithFacts(t, original)
			creation, err := model.DecodeEnterpriseRoomCreation(bad.Events[0])
			if err != nil {
				t.Fatal(err)
			}
			creation.Enterprise.OrgID = "other"
			bad.Events[0].Data, err = json.Marshal(creation)
			if err != nil {
				t.Fatal(err)
			}
			if changeBothControlAndFact {
				bad.OrgID = "other"
			}
			port.snapFn = func(EnterpriseAuth) (EnterpriseRoomSnapshot, error) { return bad, nil }
			port.commitFn = func(EnterpriseSendCommand) (EnterpriseSendResult, error) {
				return EnterpriseSendResult{}, &EnterpriseCommandError{Outcome: EnterpriseOutcomeRefused, Class: EnterpriseClassRevisionConflict, Err: errors.New("changed")}
			}
			if _, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner"), SendRequest{ID: "new", To: model.ActorSlot2, Text: "new"}); err == nil {
				t.Fatal("changed origin rebuilt")
			}
			if engine.Fatal() == nil || engine.Sequence() != 2 || !engine.cfg.DynamicBindings || len(engine.Snapshot().Messages) != 1 || engine.Snapshot().Messages[0].ID != "relay-kept-origin" {
				t.Fatal("failed creation validation changed the durable projection")
			}
			if port.commitCalls() != 1 {
				t.Fatal("invalid rebuild retried a mutation")
			}
		})
	}
}

func TestEnterpriseRebuildPinsOriginalCreationFact(t *testing.T) {
	for name, mutate := range map[string]func(*model.Event, *model.EnterpriseRoomCreation){
		"event identity": func(event *model.Event, _ *model.EnterpriseRoomCreation) { event.ID = "evt-other-birth" },
		"name":           func(_ *model.Event, creation *model.EnterpriseRoomCreation) { creation.Name = "Other Room" },
		"collaboration": func(_ *model.Event, creation *model.EnterpriseRoomCreation) {
			creation.Collaboration = &model.Collaboration{Version: 2, Mode: "custom", Instructions: "Different frozen rule."}
		},
		"creation time": func(event *model.Event, creation *model.EnterpriseRoomCreation) {
			event.CreatedAt = event.CreatedAt.Add(1)
			creation.CreatedAt = event.CreatedAt
		},
	} {
		t.Run(name, func(t *testing.T) {
			original := originalMessageFact(t, "relay-kept-birth", "kept", "keep")
			port := &fakePort{snapshot: snapshotWithFacts(t, original)}
			engine := enterpriseTestEngine(t, port)
			bad := snapshotWithFacts(t, original)
			creation, err := model.DecodeEnterpriseRoomCreation(bad.Events[0])
			if err != nil {
				t.Fatal(err)
			}
			mutate(&bad.Events[0], &creation)
			bad.Events[0].Data, err = json.Marshal(creation)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := model.DecodeEnterpriseRoomCreation(bad.Events[0]); err != nil {
				t.Fatalf("counterexample must be a valid but different creation: %v", err)
			}
			port.snapFn = func(EnterpriseAuth) (EnterpriseRoomSnapshot, error) { return bad, nil }
			port.commitFn = func(EnterpriseSendCommand) (EnterpriseSendResult, error) {
				return EnterpriseSendResult{}, &EnterpriseCommandError{Outcome: EnterpriseOutcomeRefused, Class: EnterpriseClassRevisionConflict, Err: errors.New("changed")}
			}
			if _, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner"), SendRequest{ID: "new", To: model.ActorSlot2, Text: "new"}); err == nil {
				t.Fatal("different creation replaced the original birth fact")
			}
			if engine.Fatal() == nil || engine.Sequence() != 2 || len(engine.Snapshot().Messages) != 1 || engine.Snapshot().Messages[0].ID != "relay-kept-birth" {
				t.Fatal("failed creation pin changed the durable projection")
			}
			if port.commitCalls() != 1 {
				t.Fatal("different creation allowed a mutation retry")
			}
		})
	}
}
