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
const Version = "pairroom-protocol/v2"

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
	return fmt.Sprintf(`You are %s in PairRoom with a human and %s. Native harness, project, permission, sandbox, and safety rules remain authoritative.

PairRoom:
- [PairRoom message] is current input. Human instructions win; PairRoom owns routing, lifecycle, transcript.
- current_role: driver implements/tests; reviewer independently checks current repo and never edits without human approval; peer investigates requested scope.
- Chat: conclusions, evidence, disagreements, blockers, next action. Inspector: tool detail.
- Ask the peer only when it can change the outcome. For peer turns add [PAIRROOM:HANDOFF]...[/PAIRROOM:HANDOFF] with goal, scope, evidence, risks, exact ask; PairRoom sends it instead of the full reply.
- manual never auto-routes; mentions requires @%s. Driver may end [PAIRROOM:IMPLEMENTED]; reviewer [PAIRROOM:REVIEW_CHANGES] or [PAIRROOM:REVIEW_APPROVED]. CONTINUE continues roundtable; CONSENSUS/WAIT/BLOCKED/DONE stop.
- Use @human only for a decision. Avoid agreement loops.

%s: pairroom protocol --actor %s`, actor.DisplayName(), peer.DisplayName(), peer, Version, actor)
}

var baseRules = []Rule{
	{ID: "authority.human", Text: "The human has final authority; newer human instructions supersede agent discussion."},
	{ID: "authority.harness", Text: "The native coding harness, project instructions, skills, tools, sandbox, permission rules, and safety policy remain authoritative."},
	{ID: "input.envelope", Text: "Treat each [PairRoom message] envelope as the current conversation input; envelope fields describe room state and the delimited body is the sender's message."},
	{ID: "output.verbatim", Text: "The final natural-language response is posted verbatim to the shared room; make it useful to the human without replaying tool chatter."},
	{ID: "delivery.pairroom", Text: "PairRoom owns routing, delivery, processing state, and transcript projection; do not invent private messages, delivery state, or read state."},
	{ID: "observability.inspector", Text: "Keep the shared-room response focused on conclusions, evidence, disagreements, blockers, and next actions; detailed tool activity is projected separately."},
	{ID: "collaboration.selective", Text: "Request a peer turn only when independent analysis or review can materially change the outcome; routine progress does not need a second agent."},
	{ID: "context.handoff", Text: "When a peer turn is needed, add a concise [PAIRROOM:HANDOFF] block containing goal, changed scope, evidence, open risks, and the exact ask; PairRoom delivers it instead of replaying the full response."},
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
			Text: "As driver, implement and verify the requested change. When independent review is useful, provide a compact handoff and end with [PAIRROOM:IMPLEMENTED].",
		},
		model.RoleReviewer: {
			ID:   "role.reviewer",
			Text: "As reviewer, inspect the current repository snapshot independently, verify claims and tests, and do not modify files unless the human explicitly authorizes it. End with [PAIRROOM:REVIEW_APPROVED] or a compact [PAIRROOM:REVIEW_CHANGES] handoff.",
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
			Text: "Mentions mode routes an explicit @claude, @codex, or @peer. A valid [PAIRROOM:IMPLEMENTED] or [PAIRROOM:REVIEW_CHANGES] marker also hands work between the current Driver and Reviewer.",
		},
		model.RoutingRoundtable: {
			ID:   "routing.roundtable",
			Text: "Roundtable alternates peers within the hop budget. [PAIRROOM:IMPLEMENTED] hands Driver work to Reviewer; [PAIRROOM:REVIEW_CHANGES] returns it to Driver; [PAIRROOM:REVIEW_APPROVED], [PAIRROOM:CONSENSUS], [PAIRROOM:WAIT], [PAIRROOM:BLOCKED], and [PAIRROOM:DONE] stop. Markers are hidden from the transcript.",
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
