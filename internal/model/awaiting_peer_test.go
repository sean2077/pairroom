package model

import "testing"

func TestAwaitingPeerNeverInventsRuntimeOrMention(t *testing.T) {
	pending := AgentSelection{AwaitingPeer: true}
	if err := pending.Validate(ActorSlot2); err != nil {
		t.Fatal(err)
	}
	if got := pending.Normalized(ActorSlot2); got.Runtime != "" || got.Provider.Source != "" || !got.AwaitingPeer {
		t.Fatalf("normalization invented peer configuration: %+v", got)
	}
	if RuntimeAwaitingPeer.Valid() || RuntimeAwaitingPeer.DefaultCommand() != "" {
		t.Fatal("awaiting peer must never select an executable Runtime")
	}
	identities := ParticipantIdentities(map[ActorID]RuntimeKind{ActorSlot1: RuntimeCodex, ActorSlot2: RuntimeAwaitingPeer})
	if identities[ActorSlot1].MentionHandle != "@codex" || identities[ActorSlot2].MentionHandle != "" {
		t.Fatalf("pending peer invented a mention handle: %+v", identities)
	}
	for _, invalid := range []AgentSelection{
		{AwaitingPeer: true, Runtime: RuntimeCodex},
		{AwaitingPeer: true, Provider: NativeProviderRef()},
		{AwaitingPeer: true, Sandbox: "danger-full-access"},
	} {
		if err := invalid.Validate(ActorSlot2); err == nil {
			t.Fatalf("pending peer accepted premature configuration: %+v", invalid)
		}
	}
}
