package protocol

import (
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestProtocolVersionMatchesNaturalWorkflowContract(t *testing.T) {
	if Version != "pairroom-protocol/v3" {
		t.Fatalf("protocol version = %q, want pairroom-protocol/v3", Version)
	}
}

func TestResolveFiltersRoleAndRoutingRules(t *testing.T) {
	contract, err := Resolve(Selection{
		Actor:       model.ActorCodex,
		Role:        model.RoleReviewer,
		RoutingMode: model.RoutingRoundtable,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := contract.Text()
	for _, fragment := range []string{
		Version,
		"actor: codex",
		"role: reviewer",
		"routing_mode: roundtable",
		"[authority.human]",
		"[collaboration.selective]",
		"[context.handoff]",
		"[workflow.natural]",
		"[workflow.gate]",
		"Use @claude only",
		"[role.reviewer]",
		"[routing.roundtable]",
		"[PAIRROOM:IMPLEMENTED]",
		"[PAIRROOM:REVIEW_CHANGES]",
		"[PAIRROOM:REVIEW_APPROVED]",
	} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("contract missing %q:\n%s", fragment, got)
		}
	}
	for _, fragment := range []string{"[role.driver]", "[role.peer]", "[routing.manual]", "[routing.mentions]"} {
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
		"[routing.manual]", "[routing.mentions]", "[routing.roundtable]",
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
	} {
		if _, err := Resolve(selection); err == nil {
			t.Fatalf("Resolve(%+v) succeeded", selection)
		}
	}
}
