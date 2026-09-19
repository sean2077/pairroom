package protocol

import (
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestNativeBootstrapAvoidsRoutineDiagnosticTurns(t *testing.T) {
	for _, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
		text := NativeBootstrap(model.ActorSlot1, kind, kind)
		if !strings.Contains(text, "Use relay status on uncertainty, not after every reply.") || strings.Contains(text, "Check relay status next time") {
			t.Fatal("bootstrap mandates an unnecessary status round trip")
		}
	}
}
