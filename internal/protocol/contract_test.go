package protocol

import (
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestProtocolVersionMatchesMentionRelayContract(t *testing.T) {
	if Version != "pairroom-protocol/v5" {
		t.Fatalf("protocol version = %q, want pairroom-protocol/v5", Version)
	}
}

func TestResolveFiltersRoleAndContainsMentionRules(t *testing.T) {
	contract, err := Resolve(Selection{Actor: model.ActorCodex, Role: model.RoleReviewer})
	if err != nil {
		t.Fatal(err)
	}
	got := contract.Text()
	for _, fragment := range []string{
		Version, "actor: codex", "role: reviewer", "[authority.human]",
		"[delivery.single-turn]", "[delivery.peer]", "[delivery.stop]",
		"[delivery.human]", "[role.reviewer]", "exact peer_handle", "@user overrides",
	} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("contract missing %q:\n%s", fragment, got)
		}
	}
	for _, fragment := range []string{
		"routing_mode", "HANDOFF", "PAIRROOM:", "workflow", "@peer", "@human",
		"[role.driver]", "[role.peer]",
	} {
		if strings.Contains(got, fragment) {
			t.Fatalf("filtered contract unexpectedly contains %q:\n%s", fragment, got)
		}
	}
}

func TestResolveWithoutFiltersIsCompleteAndDeterministic(t *testing.T) {
	first, err := Resolve(Selection{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Resolve(Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Text() != second.Text() {
		t.Fatal("contract output must be deterministic")
	}
	for _, fragment := range []string{"[role.driver]", "[role.reviewer]", "[role.peer]", "[convergence.intentional]"} {
		if !strings.Contains(first.Text(), fragment) {
			t.Fatalf("complete contract missing %q", fragment)
		}
	}
}

func TestResolveRejectsInvalidSelection(t *testing.T) {
	for _, selection := range []Selection{
		{Actor: model.ActorID("other")},
		{Role: model.ParticipantRole("observer")},
	} {
		if _, err := Resolve(selection); err == nil {
			t.Fatalf("Resolve(%+v) succeeded", selection)
		}
	}
}

func TestBootstrapUsesDynamicDuplicateHandles(t *testing.T) {
	got := Bootstrap(model.ActorClaude, model.RuntimeCodex, model.RuntimeCodex)
	for _, fragment := range []string{"Codex 0 (@codex0)", "Codex 1 (@codex1)", "Include @codex1", "without @codex1"} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("bootstrap missing %q:\n%s", fragment, got)
		}
	}
}
