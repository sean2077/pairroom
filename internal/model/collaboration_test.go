package model

import (
	"strings"
	"testing"
)

func TestCollaborationHasOnlyTwoCreationModes(t *testing.T) {
	d, err := (Collaboration{}).ForCreation()
	if err != nil || d.Mode != CollaborationDefault || d.Instructions != DefaultCollaborationInstructions || d.Version != 1 {
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
		{Mode: CollaborationDefault, Instructions: "silently override fixed instructions"}, {Version: 2},
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
