package model

import "testing"

func TestGeminiSelectionValidation(t *testing.T) {
	for _, alias := range []string{"gemini", "Gemini", "gemini-cli", "gemini_cli", "geminicli"} {
		kind := ParseRuntimeKind(alias)
		if kind != RuntimeGemini || !kind.Valid() || kind.DefaultCommand() != "gemini" || kind.DisplayName() != "Gemini CLI" {
			t.Fatal(alias, kind)
		}
	}
	for _, mode := range []string{"", "default", "auto_edit", "plan", "yolo"} {
		for _, sandbox := range []string{"", "on", "off"} {
			if err := (AgentSelection{Runtime: RuntimeGemini, PermissionMode: mode, Sandbox: sandbox}).Validate(ActorSlot1); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, selection := range []AgentSelection{
		{Runtime: RuntimeGemini, Effort: "high"},
		{Runtime: RuntimeGemini, ApprovalPolicy: "never"},
		{Runtime: RuntimeGemini, PermissionMode: "bypassPermissions"},
		{Runtime: RuntimeGemini, Sandbox: "danger-full-access"},
		{Runtime: RuntimeGemini, Provider: ProviderRef{Source: ProviderCCSwitch, AppType: "gemini", ProfileID: "other"}},
	} {
		if err := selection.Validate(ActorSlot1); err == nil {
			t.Fatalf("silently accepted unsupported Gemini override: %+v", selection)
		}
	}
}
