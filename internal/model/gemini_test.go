package model

import "testing"

func TestGeminiRuntimeIdentityAndNativeSelection(t *testing.T) {
	for _, spelling := range []RuntimeKind{"gemini", "Gemini-CLI", "gemini_cli", "geminicli"} {
		kind := spelling.Canonical()
		if kind != RuntimeGemini || !kind.Valid() || kind.DisplayName() != "Gemini CLI" || kind.DefaultCommand() != "gemini" {
			t.Fatalf("bad Gemini identity: %s", kind)
		}
	}
	selection := AgentSelection{Runtime: RuntimeGemini}.Normalized(ActorSlot1)
	if err := selection.Validate(ActorSlot1); err != nil {
		t.Fatal(err)
	}
	for _, unsupported := range []AgentSelection{
		{Runtime: RuntimeGemini, PermissionMode: "yolo"},
		{Runtime: RuntimeGemini, Sandbox: "off"},
		{Runtime: RuntimeGemini, ApprovalPolicy: "never"},
		{Runtime: RuntimeGemini, Provider: ProviderRef{Source: ProviderCCSwitch, AppType: "gemini", ProfileID: "secret-ref"}},
	} {
		if unsupported.Validate(ActorSlot1) == nil {
			t.Fatal("accepted unsupported owned Gemini configuration")
		}
	}
	ids := ParticipantIdentities(map[ActorID]RuntimeKind{ActorSlot1: RuntimeGemini, ActorSlot2: RuntimeCodex})
	if ids[ActorSlot1].MentionHandle != "@gemini" || ids[ActorSlot2].MentionHandle != "@codex" {
		t.Fatal("cross-runtime identity lost")
	}
	ids = ParticipantIdentities(map[ActorID]RuntimeKind{ActorSlot1: RuntimeGemini, ActorSlot2: RuntimeGemini})
	if ids[ActorSlot1].MentionHandle != "@gemini0" || ids[ActorSlot2].MentionHandle != "@gemini1" {
		t.Fatal("duplicate runtimes share a handle")
	}
}
