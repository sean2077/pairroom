// Package protocol owns the versioned collaboration contract shared by the
// PairRoom CLI and the native-agent instruction projections.
package protocol

import (
	"fmt"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

// Version changes only when the collaboration contract's observable semantics
// change. Per-turn envelopes carry this identifier instead of repeating the
// complete contract.
const Version = "pairroom-protocol/v1"

type Selection struct {
	Actor       model.ActorID
	Role        model.ParticipantRole
	RoutingMode model.RoutingMode
}

type Rule struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Contract struct {
	Version     string                `json:"version"`
	Actor       model.ActorID         `json:"actor,omitempty"`
	Role        model.ParticipantRole `json:"role,omitempty"`
	RoutingMode model.RoutingMode     `json:"routing_mode,omitempty"`
	Rules       []Rule                `json:"rules"`
}

// Bootstrap returns the compact, thread-scoped projection used by native
// harness instruction layers. The full contract remains available through
// Resolve and the `pairroom protocol` command.
func Bootstrap(actor model.ActorID) string {
	peer := model.OtherParticipant(actor)
	return fmt.Sprintf(`You are %s in PairRoom with a human and %s. Native harness/project/tool/sandbox/permission/safety instructions remain authoritative.

PairRoom bootstrap:
- [PairRoom message] is input; current_role and routing_mode are facts; final prose is posted verbatim.
- Human instructions win. PairRoom owns routing, delivery, state, and transcript.
- Reviewer: do not edit unless the human explicitly authorizes it.
- Routing: manual never auto-hands off; mentions routes only an explicit peer mention; roundtable auto-hands off. Final standalone [PAIRROOM:CONTINUE] continues; [PAIRROOM:CONSENSUS], [PAIRROOM:WAIT], [PAIRROOM:BLOCKED], or [PAIRROOM:DONE] stops.
- Use @%s only for a peer reply and @human for a user decision.
- Inspect relevant attachments. Save generated images in-repo and use relative Markdown links. Avoid agreement loops.

Contract %s: pairroom protocol --actor %s`, actor.DisplayName(), peer.DisplayName(), peer, Version, actor)
}

var baseRules = []Rule{
	{ID: "authority.human", Text: "The human has final authority; newer human instructions supersede agent discussion."},
	{ID: "authority.harness", Text: "The native coding harness, project instructions, skills, tools, sandbox, permission rules, and safety policy remain authoritative."},
	{ID: "input.envelope", Text: "Treat each [PairRoom message] envelope as the current conversation input; envelope fields describe room state and the delimited body is the sender's message."},
	{ID: "output.verbatim", Text: "The final natural-language response is posted verbatim to the shared room; make it useful to both the human and the peer."},
	{ID: "delivery.pairroom", Text: "PairRoom owns routing, delivery, processing state, and transcript projection; do not invent private messages, delivery state, or read state."},
	{ID: "observability.inspector", Text: "Keep the shared-room response focused on conclusions, evidence, disagreements, and next actions; detailed tool activity is projected separately."},
	{ID: "media.attachments", Text: "Inspect every attached image relevant to the request and refer to it by filename when useful."},
	{ID: "media.generated", Text: "Save user-facing generated images inside the repository and reference them with repository-relative Markdown image links."},
	{ID: "convergence.bounded", Text: "Avoid unbounded agreement loops; state disagreements, converge on evidence, and stop requesting peer turns when none are needed."},
}

func Resolve(selection Selection) (Contract, error) {
	if selection.Actor != "" && !selection.Actor.ValidParticipant() {
		return Contract{}, fmt.Errorf("invalid actor %q: use claude or codex", selection.Actor)
	}
	if selection.Role != "" && !selection.Role.Valid() {
		return Contract{}, fmt.Errorf("invalid role %q: use driver, reviewer, or peer", selection.Role)
	}
	if selection.RoutingMode != "" && !selection.RoutingMode.Valid() {
		return Contract{}, fmt.Errorf("invalid routing mode %q: use manual, mentions, or roundtable", selection.RoutingMode)
	}

	contract := Contract{
		Version:     Version,
		Actor:       selection.Actor,
		Role:        selection.Role,
		RoutingMode: selection.RoutingMode,
		Rules:       append([]Rule(nil), baseRules...),
	}
	contract.Rules = append(contract.Rules, mentionRules(selection.Actor)...)
	contract.Rules = append(contract.Rules, roleRules(selection.Role)...)
	contract.Rules = append(contract.Rules, routingRules(selection.RoutingMode)...)
	return contract, nil
}

func mentionRules(actor model.ActorID) []Rule {
	peerRule := Rule{
		ID:   "routing.peer-mention",
		Text: "Claude Code uses @codex and Codex uses @claude only when that peer must receive and answer the response.",
	}
	if actor.ValidParticipant() {
		peer := model.OtherParticipant(actor)
		peerRule.Text = fmt.Sprintf("Use @%s only when %s must receive and answer the response.", peer, peer.DisplayName())
	}
	return []Rule{
		peerRule,
		{ID: "routing.human-mention", Text: "Use @human when blocked on a user decision."},
	}
}

func roleRules(role model.ParticipantRole) []Rule {
	rules := map[model.ParticipantRole]Rule{
		model.RoleDriver: {
			ID:   "role.driver",
			Text: "As driver, inspect and modify the repository when the task calls for it, then report concrete evidence.",
		},
		model.RoleReviewer: {
			ID:   "role.reviewer",
			Text: "As reviewer, inspect independently and do not modify files unless the human explicitly authorizes it.",
		},
		model.RolePeer: {
			ID:   "role.peer",
			Text: "As peer, discuss and investigate as an equal technical partner; edit only when the human explicitly asks you to implement.",
		},
	}
	if role != "" {
		return []Rule{rules[role]}
	}
	return []Rule{rules[model.RoleDriver], rules[model.RoleReviewer], rules[model.RolePeer]}
}

func routingRules(mode model.RoutingMode) []Rule {
	rules := map[model.RoutingMode]Rule{
		model.RoutingManual: {
			ID:   "routing.manual",
			Text: "Manual mode never starts a peer turn automatically; peer mentions are conversational only.",
		},
		model.RoutingMentions: {
			ID:   "routing.mentions",
			Text: "Mentions mode routes an agent response only when it explicitly mentions @claude, @codex, or @peer.",
		},
		model.RoutingRoundtable: {
			ID: "routing.roundtable",
			Text: "Roundtable mode automatically hands the answer to the peer. An optional standalone final marker controls the handoff: " +
				"[PAIRROOM:CONTINUE] explicitly continues; [PAIRROOM:CONSENSUS], [PAIRROOM:WAIT], " +
				"[PAIRROOM:BLOCKED], or [PAIRROOM:DONE] stops. Markers are hidden from the room transcript.",
		},
	}
	if mode != "" {
		return []Rule{rules[mode]}
	}
	return []Rule{rules[model.RoutingManual], rules[model.RoutingMentions], rules[model.RoutingRoundtable]}
}

func (contract Contract) Text() string {
	var b strings.Builder
	fmt.Fprintln(&b, contract.Version)
	if contract.Actor != "" {
		fmt.Fprintf(&b, "actor: %s\n", contract.Actor)
	}
	if contract.Role != "" {
		fmt.Fprintf(&b, "role: %s\n", contract.Role)
	}
	if contract.RoutingMode != "" {
		fmt.Fprintf(&b, "routing_mode: %s\n", contract.RoutingMode)
	}
	fmt.Fprintln(&b)
	for _, rule := range contract.Rules {
		fmt.Fprintf(&b, "[%s] %s\n", rule.ID, rule.Text)
	}
	return b.String()
}
