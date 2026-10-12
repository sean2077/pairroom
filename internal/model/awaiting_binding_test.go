package model

import (
	"strings"
	"testing"
)

func TestAwaitingBindingNeverInheritsExecutionDefaults(t *testing.T) {
	selection := AgentSelection{AwaitingBinding: true}
	for _, slot := range SlotActors() {
		if err := selection.Validate(slot); err != nil || selection.Normalized(slot) != selection {
			t.Fatalf("empty dynamic selection acquired defaults: %+v, %v", selection.Normalized(slot), err)
		}
	}
	for _, invalid := range []AgentSelection{
		{AwaitingBinding: true, AwaitingPeer: true},
		{AwaitingBinding: true, Runtime: RuntimeCodex},
		{AwaitingBinding: true, Provider: NativeProviderRef()},
		{AwaitingBinding: true, Model: "model"},
		{AwaitingBinding: true, Instructions: "instructions"},
	} {
		if invalid.Validate(ActorSlot1) == nil {
			t.Fatalf("vacant Native slot accepted execution settings: %+v", invalid)
		}
	}
	if RuntimeAwaitingBinding.Valid() || RuntimeAwaitingBinding.DefaultCommand() != "" || RuntimeAwaitingBinding.ProviderAgentType() != "" || RuntimeAwaitingBinding.NativeWakePolicy().Transport.Supported() {
		t.Fatal("awaiting binding acquired an executable Runtime capability")
	}
	if RuntimeAwaitingBinding.DisplayName() != "Awaiting binding" {
		t.Fatal("vacant local slot is not distinguishable from a pending remote peer")
	}
}

func TestDynamicNativeNamesKeepStableParticipantHandle(t *testing.T) {
	for _, kind := range []RuntimeKind{RuntimeClaude, RuntimeCodex, RuntimeGrok, RuntimeGemini} {
		for _, peer := range []RuntimeKind{kind, RuntimeAwaitingBinding, RuntimeAwaitingPeer} {
			name := NativeSessionName("room-example", "Shared task", ActorSlot1, kind, peer, true)
			if !strings.Contains(name, "@agent1") || strings.Contains(name, "@"+string(kind)) {
				t.Fatalf("dynamic native name = %q", name)
			}
		}
	}
	if NativeSessionName("room-example", "Shared task", ActorSlot1, "", RuntimeAwaitingBinding, true) != "" {
		t.Fatal("dynamic session title guessed an unknown caller's Runtime")
	}
}
