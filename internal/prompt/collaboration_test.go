package prompt

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
)

func TestVersionedDefaultCollaborationFitsEveryRuntimePair(t *testing.T) {
	runtimes := []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok}
	for _, version := range []int{1, model.CollaborationVersion} {
		spec, err := (model.Collaboration{Version: version}).ForCreation()
		if err != nil {
			t.Fatal(err)
		}
		for _, actor := range model.SlotActors() {
			for _, self := range runtimes {
				for _, peer := range runtimes {
					t.Run(fmt.Sprintf("v%d/%s/%s/%s", version, actor, self, peer), func(t *testing.T) {
						got := BootstrapPromptWithRuntime(actor, self, peer) + "\n" + protocol.CollaborationInstructions(actor, &spec)
						if strings.Count(got, spec.Instructions) != 1 {
							t.Fatal("stored policy must be projected exactly once")
						}
						if len(got) > MaxBootstrapBytes {
							t.Fatalf("bootstrap plus collaboration = %d bytes, budget = %d", len(got), MaxBootstrapBytes)
						}
						if !strings.Contains(got, "Your responsibility: "+spec.Responsibility(actor)) {
							t.Fatal("lost stable slot responsibility")
						}
					})
				}
			}
		}
	}
}

func TestCustomCollaborationDoesNotInheritAdaptiveDefaults(t *testing.T) {
	const policy = "Always request peer review before completion.\n保留用户原文。"
	for _, version := range []int{1, model.CollaborationVersion} {
		spec := model.Collaboration{Version: version, Mode: model.CollaborationCustom, Instructions: policy}
		for _, actor := range model.SlotActors() {
			got := protocol.CollaborationInstructions(actor, &spec)
			if !strings.Contains(got, policy) || !strings.Contains(got, "Your responsibility: participant") {
				t.Fatalf("custom policy changed: %s", got)
			}
			for _, unwanted := range []string{model.DefaultCollaborationInstructions, "Lead", "Executor", "without delegation or peer review"} {
				if strings.Contains(got, unwanted) {
					t.Fatalf("custom policy inherited %q", unwanted)
				}
			}
		}
	}
}
