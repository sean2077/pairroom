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
