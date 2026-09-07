package model

import "testing"

func TestAgentSelectionAcceptsYoloPermissionAliases(t *testing.T) {
	claude := AgentSelection{Runtime: RuntimeClaude, Provider: NativeProviderRef(), PermissionMode: "yolo", OrdinaryReviewerPolicy: ReviewerEnforced}
	if err := claude.Validate(ActorClaude); err != nil {
		t.Fatalf("Claude yolo should be valid: %v", err)
	}
	grok := AgentSelection{Runtime: RuntimeGrok, Provider: NativeProviderRef(), PermissionMode: "yolo", OrdinaryReviewerPolicy: ReviewerEnforced}
	if err := grok.Validate(ActorClaude); err != nil {
		t.Fatalf("Grok yolo should be valid: %v", err)
	}
	codex := AgentSelection{Runtime: RuntimeCodex, Provider: NativeProviderRef(), ApprovalPolicy: "yolo", OrdinaryReviewerPolicy: ReviewerEnforced}
	if err := codex.Validate(ActorCodex); err != nil {
		t.Fatalf("Codex yolo approval should be valid: %v", err)
	}
}

func TestProviderNormalizationPreservesIncompleteReferences(t *testing.T) {
	for _, provider := range []ProviderRef{
		{ProfileID: "selected-profile"},
		{AppType: "codex"},
		{AppType: "codex", ProfileID: "selected-profile"},
	} {
		t.Run(provider.AppType+"/"+provider.ProfileID, func(t *testing.T) {
			selection := AgentSelection{Runtime: RuntimeCodex, Provider: provider}
			normalized := selection.Normalized(ActorCodex)
			if normalized.Provider.AppType != provider.AppType || normalized.Provider.ProfileID != provider.ProfileID {
				t.Errorf("normalization discarded explicit provider identity: %+v -> %+v", provider, normalized.Provider)
			}
			if err := selection.Validate(ActorCodex); err == nil {
				t.Fatal("incomplete profile reference silently fell back to native configuration")
			}
		})
	}
	for _, provider := range []ProviderRef{{}, NativeProviderRef(), {Source: ProviderCCSwitch, AppType: "codex", ProfileID: "selected-profile"}} {
		if err := (AgentSelection{Runtime: RuntimeCodex, Provider: provider}).Validate(ActorCodex); err != nil {
			t.Fatalf("valid provider %+v: %v", provider, err)
		}
	}
}
