package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestGrokReservesFinalStopBeforeVendorContinuationCutoff(t *testing.T) {
	f := grokNativeHTTP(t, model.RuntimeCodex)
	a := bindGrok(t, f, model.ActorSlot1)
	if _, err := f.native.engine.SendUser(relay.SendRequest{ID: "pending", To: a.Slot, Text: "pending full input"}); err != nil {
		t.Fatal(err)
	}
	if err := f.native.engine.Park(a.Slot, true); err != nil {
		t.Fatal(err)
	}
	// Grok does not call Stop again after eight block decisions. Our eighth
	// Stop must publish its complete reply and allow, not ask for that round.
	for i := 0; i < relay.MaxBlocks; i++ {
		text := fmt.Sprintf("@codex complete result %d", i)
		out, err := grokHook(t, f, a, text, map[string]any{"stopHookActive": i > 0})
		if err != nil {
			t.Fatal(err)
		}
		var decision struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(out, &decision); err != nil {
			t.Fatal(err)
		}
		if i < relay.MaxBlocks-1 {
			if decision.Decision != "block" || !strings.Contains(decision.Reason, "pairroom relay wait") || strings.Contains(decision.Reason, "--room") || strings.Contains(decision.Reason, "--slot") || strings.Contains(decision.Reason, "pending full input") {
				t.Fatalf("invalid bounded readiness: %s", out)
			}
		} else if decision.Decision != "" {
			t.Fatal("requested a continuation whose final reply the vendor would not report")
		}
	}
	messages := f.native.engine.Snapshot().Messages
	if len(messages) != relay.MaxBlocks+1 || messages[0].State != "queued" || messages[len(messages)-1].Text != "@codex complete result 7" {
		t.Fatalf("final reply lost or readiness claimed an envelope: %+v", messages)
	}
	// The next real user turn gets a fresh cap, without changing bindings.
	out, err := grokHook(t, f, a, "new turn", map[string]any{"stopHookActive": false})
	if err != nil || !strings.Contains(string(out), `"decision":"block"`) {
		t.Fatalf("new turn did not reset continuation cap: %s %v", out, err)
	}
}
