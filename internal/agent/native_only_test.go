package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestGeminiEmbeddedNeverFallsBackToClaude(t *testing.T) {
	for _, factory := range []Factory{FactoryFor(model.RuntimeGemini), SlotFactory(false, model.RuntimeGemini)} {
		adapter := factory(Config{Actor: model.ActorSlot2, Runtime: model.RuntimeGemini, Command: "must-not-launch", Env: map[string]string{"API_KEY": "secret-not-an-error"}}, func(model.RuntimeEvent) { t.Error("Native-only factory emitted a vendor event") })
		err := adapter.Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Native rooms only") || strings.Contains(err.Error(), "secret-not-an-error") {
			t.Fatalf("unsafe fallback: %v", err)
		}
		if adapter.Actor() != model.ActorSlot2 || adapter.State() != model.StateError || adapter.SessionID() != "" {
			t.Fatal("fabricated native session")
		}
		if err := adapter.StartTurn(context.Background(), model.AgentInput{}); err == nil {
			t.Fatal("accepted unsupported turn")
		}
		if adapter.Steer(context.Background(), model.AgentInput{}).State != SteerUnavailable {
			t.Fatal("accepted unsupported steering")
		}
		if err := adapter.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
