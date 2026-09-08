package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCollaborationHasOnlyTwoCreationModes(t *testing.T) {
	d, err := (Collaboration{}).ForCreation()
	if err != nil || d.Mode != CollaborationDefault || d.Instructions != DefaultCollaborationInstructions || d.Version != CollaborationVersion {
		t.Fatalf("default=%+v err=%v", d, err)
	}
	if d.Responsibility(ActorClaude) != "lead" || d.Responsibility(ActorCodex) != "executor" {
		t.Fatal("wrong default responsibilities")
	}
	custom, err := (Collaboration{Mode: CollaborationCustom, Instructions: "Agent 2 proposes; Agent 1 challenges.\n保留用户原文。"}).ForCreation()
	if err != nil || custom.Responsibility(ActorClaude) != "participant" || strings.Contains(custom.Instructions, DefaultCollaborationInstructions) {
		t.Fatalf("custom=%+v err=%v", custom, err)
	}
	for _, c := range []Collaboration{
		{Mode: "peer"}, {Mode: "driver"}, {Mode: "reviewer"}, {Mode: "plan"}, {Mode: "yolo"},
		{Mode: CollaborationCustom}, {Mode: CollaborationCustom, Instructions: " \n\t"},
		{Mode: CollaborationCustom, Instructions: "a\x00b"}, {Mode: CollaborationCustom, Instructions: string([]byte{0xff})},
		{Mode: CollaborationCustom, Instructions: strings.Repeat("字", MaxCollaborationInstructionsBytes/3+1)},
		{Mode: CollaborationDefault, Instructions: "silently override fixed instructions"}, {Version: -1}, {Version: CollaborationVersion + 1},
	} {
		if _, err := c.ForCreation(); err == nil {
			t.Fatalf("invalid mode accepted: %+v", c)
		}
	}
	copy := CloneCollaboration(&custom)
	copy.Instructions = "changed"
	if custom.Instructions == copy.Instructions {
		t.Fatal("clone aliases immutable instructions")
	}
}

func TestDefaultCollaborationIsAdaptiveAndCompact(t *testing.T) {
	for _, fragment := range []string{
		"least necessary coordination", "simple, low-risk tasks", "addressed Agent",
		"executes, verifies, and answers directly, without delegation or peer review",
		"Agent 1 (Lead)", "Agent 2 (Executor)", "complexity, uncertainty, or risk",
		"not to complete a fixed sequence", "not tool permissions", "Follow newer human instructions",
	} {
		if !strings.Contains(DefaultCollaborationInstructions, fragment) {
			t.Fatalf("default collaboration missing %q", fragment)
		}
	}
	if len(DefaultCollaborationInstructions) > 550 {
		t.Fatalf("default collaboration grew to %d bytes; budget is 550", len(DefaultCollaborationInstructions))
	}
}

func TestCollaborationVersionsRoundTripWithoutRewriting(t *testing.T) {
	// This is the policy actually persisted by version 1, not the current default.
	const persistedV1 = "Agent 1 is Lead: own planning, technical decisions, and final review. Delegate implementation and routine verification to Agent 2; inspect the evidence and request concrete corrections when needed. Agent 2 is Executor: implement, test, and report evidence, risks, and unresolved issues; challenge or supplement the Lead's plan when warranted. Prefer completing useful work over repeated debate or acknowledgements. Scale planning and review to the task; do not create ceremonial turns. These responsibilities do not grant or restrict tools. Follow newer human instructions."
	for _, version := range []int{1, CollaborationVersion} {
		for _, mode := range []string{CollaborationDefault, CollaborationCustom} {
			t.Run(fmt.Sprintf("v%d/%s", version, mode), func(t *testing.T) {
				want := Collaboration{Version: version, Mode: mode, Instructions: DefaultCollaborationInstructions}
				if version == 1 {
					want.Instructions = persistedV1
				}
				if mode == CollaborationCustom {
					want.Instructions = "Always request peer review before completion.\n保留用户原文。"
				}
				data, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				var got Collaboration
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				if err := got.Validate(); err != nil {
					t.Fatalf("stored policy rejected: %v", err)
				}
				normalized, err := got.ForCreation()
				if err != nil || normalized != want || got != want {
					t.Fatalf("stored policy changed: got=%+v normalized=%+v err=%v", got, normalized, err)
				}
				if mode == CollaborationDefault {
					filled, err := (Collaboration{Version: version, Mode: mode}).ForCreation()
					if err != nil || filled != want {
						t.Fatalf("wrong versioned default: %+v err=%v", filled, err)
					}
				}
			})
		}
	}
}

func TestCollaborationRejectsVersionPolicyMismatch(t *testing.T) {
	for _, c := range []Collaboration{
		{Version: 1, Mode: CollaborationDefault, Instructions: DefaultCollaborationInstructions},
		{Version: CollaborationVersion, Mode: CollaborationDefault, Instructions: defaultCollaborationInstructionsV1},
		{Version: 0, Mode: CollaborationDefault, Instructions: defaultCollaborationInstructionsV1},
		{Version: -1, Mode: CollaborationCustom, Instructions: "custom"},
		{Version: CollaborationVersion + 1, Mode: CollaborationCustom, Instructions: "custom"},
	} {
		if err := c.Validate(); err == nil {
			t.Fatalf("invalid stored policy accepted: %+v", c)
		}
		if _, err := c.ForCreation(); err == nil {
			t.Fatalf("invalid creation policy accepted: %+v", c)
		}
	}
}
