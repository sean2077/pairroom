package protocol

import (
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestNativeBootstrapBudgetAndBoundaries(t *testing.T) {
	for _, slot := range model.SlotActors() {
		for _, a := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
			for _, b := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
				text := NativeBootstrap(slot, a, b)
				for _, version := range []int{1, model.CollaborationVersion} {
					collaboration, err := (model.Collaboration{Version: version}).ForCreation()
					if err != nil {
						t.Fatal(err)
					}
					full := text + "\n\n" + CollaborationInstructions(slot, &collaboration)
					if len(full) > 1800 {
						t.Fatalf("native bootstrap + collaboration v%d %s/%s/%s = %d bytes", version, slot, a, b, len(full))
					}
				}
				for _, fragment := range []string{NativeVersion, "advisory", "relay send", "never semantically deduplicated", "Never read or print relay credentials"} {
					if !strings.Contains(text, fragment) {
						t.Fatalf("missing %q", fragment)
					}
				}
			}
		}
	}
}

func TestGrokNativeBootstrapExplainsForegroundDelivery(t *testing.T) {
	text := NativeBootstrap(model.ActorSlot1, model.RuntimeGrok, model.RuntimeGrok)
	for _, wanted := range []string{"@grok0", "@grok1", "clips hook text", "relay wait", "relay send/exchange"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("missing Grok contract %q", wanted)
		}
	}
	if strings.Contains(text, "Stop relays your full visible reply.") {
		t.Fatal("Grok bootstrap promised unclipped hook output")
	}
}
