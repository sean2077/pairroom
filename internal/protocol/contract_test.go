package protocol

import (
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestProtocolVersionMatchesTurnRelayContract(t *testing.T) {
	if Version != "pairroom-protocol/v4" {
		t.Fatalf("protocol version = %q, want pairroom-protocol/v4", Version)
	}
}

func TestResolveFiltersRoleWithTurnRouting(t *testing.T) {
	contract, err := Resolve(Selection{
		Actor:       model.ActorCodex,
		Role:        model.RoleReviewer,
		RoutingMode: model.RoutingTurns,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := contract.Text()
	for _, fragment := range []string{
		Version,
		"actor: codex",
		"role: reviewer",
		"routing_mode: turns",
		"[authority.human]",
		"[delivery.single-turn]",
		"[delivery.next]",
		"[delivery.stop]",
		"[workflow.natural]",
		"[workflow.gate]",
		"[role.reviewer]",
		"HANDOFF + NEXT",
		"explicit @claude, @codex, or @peer",
		"@human and @user",
	} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("contract missing %q:\n%s", fragment, got)
		}
	}
	for _, fragment := range []string{"[role.driver]", "[role.peer]"} {
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
	for _, fragment := range []string{
		"[role.driver]", "[role.reviewer]", "[role.peer]",
		"[delivery.single-turn]", "[delivery.next]", "[delivery.stop]",
	} {
		if !strings.Contains(first.Text(), fragment) {
			t.Fatalf("complete contract missing %q", fragment)
		}
	}
}

func TestResolveRejectsInvalidSelection(t *testing.T) {
	for _, selection := range []Selection{
		{Actor: model.ActorID("other")},
		{Role: model.ParticipantRole("observer")},
		{RoutingMode: model.RoutingMode("automatic")},
		{RoutingMode: model.RoutingMode("manual")},
		{RoutingMode: model.RoutingMode("mentions")},
		{RoutingMode: model.RoutingMode("roundtable")},
	} {
		if _, err := Resolve(selection); err == nil {
			t.Fatalf("Resolve(%+v) succeeded", selection)
		}
	}
}
