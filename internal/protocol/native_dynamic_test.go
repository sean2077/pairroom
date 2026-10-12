package protocol

import (
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestDynamicNativeBootstrapHasStableHandlesAndExplicitPeerGeneration(t *testing.T) {
	for _, slot := range model.SlotActors() {
		for _, self := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok, model.RuntimeGemini} {
			for _, peer := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok, model.RuntimeGemini, model.RuntimeAwaitingBinding, model.RuntimeAwaitingPeer} {
				text := NativeDynamicBootstrap(slot, self, peer, 2)
				for _, version := range []int{1, model.CollaborationVersion} {
					collaboration, err := (model.Collaboration{Version: version}).ForCreation()
					if err != nil {
						t.Fatal(err)
					}
					full := text + "\n\n" + CollaborationInstructions(slot, &collaboration) + "\n" + SharedRoomBootstrapNotice
					if len(full) > 1800 {
						t.Fatalf("dynamic bootstrap budget %s/%s/%s = %d", slot, self, peer, len(full))
					}
				}
				for _, required := range []string{"@agent1", "@agent2", "generation 2", NativeDynamicVersion, "replaced peer blocks stale publication", "relay send", "@user", "Never read or print relay credentials"} {
					if !strings.Contains(text, required) {
						t.Fatalf("bootstrap lacks %q", required)
					}
				}
				for _, retired := range []string{"@claude", "@codex", "@grok", "@gemini", NativeVersion, "--legacy-native"} {
					if strings.Contains(text, retired) {
						t.Fatalf("dynamic bootstrap retained old routing %q", retired)
					}
				}
			}
		}
	}
	if NativeDynamicBootstrap(model.ActorUser, model.RuntimeCodex, model.RuntimeClaude, 1) != "" || NativeDynamicBootstrap(model.ActorSlot1, model.RuntimeCodex, model.RuntimeClaude, 0) != "" {
		t.Fatal("bootstrap accepted an invalid actor or wildcard peer generation")
	}
	if NativeDynamicBootstrap(model.ActorSlot1, model.RuntimeAwaitingBinding, model.RuntimeClaude, 1) != "" || NativeDynamicBootstrap(model.ActorSlot1, model.RuntimeCodex, "future-runtime", 1) != "" {
		t.Fatal("bootstrap manufactured an identity for an unbound or unknown runtime")
	}
	legacy, err := ResolveNative(Selection{Actor: model.ActorSlot1})
	if err != nil || legacy.Version != NativeVersion {
		t.Fatal("existing Native protocol was upgraded implicitly")
	}
	current, err := ResolveNativeDynamic(Selection{Actor: model.ActorSlot1})
	if err != nil || current.Version != NativeDynamicVersion {
		t.Fatal("dynamic Native protocol was not selected")
	}
}
