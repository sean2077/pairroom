// Package protocol owns the versioned collaboration contract shared by the
// PairRoom CLI and the native-agent instruction projections.
package protocol

import (
	"fmt"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

const Version = "pairroom-protocol/v3"

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

func Bootstrap(actor model.ActorID) string {
	peer := model.OtherParticipant(actor)
	return fmt.Sprintf(`You are %s in PairRoom with a human and %s. Native harness, project, permission, sandbox, and safety rules remain authoritative.

PairRoom:
- [PairRoom message] is current input. Human instructions win; PairRoom owns routing, lifecycle, transcript, and any compiled workflow sequence.
- current_role: driver may implement; reviewer is independent/read-only; peer investigates. workflow_mode plan/review/audit is read-only; execute is allowed only after PairRoom's human-approval gate.
- Follow the human's natural sequence exactly (for example Claude plan → Codex review → Codex execute → Claude audit). Complete only the current workflow stage.
- Ask the peer only when it can change the outcome. For peer turns add [PAIRROOM:HANDOFF]...[/PAIRROOM:HANDOFF] with goal, evidence, risks, exact ask.
- Ask humans visibly with @human and [PAIRROOM:WAIT]; never wait on a hidden request_user_input or terminal prompt.
- manual never auto-routes; mentions requires @%s. Explicit peer mentions win over generic stop markers; do not combine them. Driver may end [PAIRROOM:IMPLEMENTED]; reviewer [PAIRROOM:REVIEW_CHANGES] or [PAIRROOM:REVIEW_APPROVED]. CONTINUE continues roundtable; CONSENSUS/WAIT/BLOCKED/DONE stop implicit continuation.
- Avoid agreement loops; keep conclusions and evidence in chat, tool detail in Inspector.

%s: pairroom protocol --actor %s`, actor.DisplayName(), peer.DisplayName(), peer, Version, actor)
}

var baseRules = []Rule{
	{ID: "authority.human", Text: "The human has final authority; newer human instructions supersede agent discussion."},
	{ID: "authority.harness", Text: "The native coding harness, project instructions, skills, tools, sandbox, permission rules, and safety policy remain authoritative."},
	{ID: "input.envelope", Text: "Treat each [PairRoom message] envelope as current input; fields describe Room state and the delimited body is the sender's message."},
	{ID: "output.verbatim", Text: "The final natural-language response is posted verbatim to the shared Room; make it useful without replaying tool chatter."},
	{ID: "delivery.pairroom", Text: "PairRoom owns routing, delivery, processing state, workflow sequencing, and transcript projection."},
	{ID: "workflow.natural", Text: "When workflow_id and workflow_mode are present, PairRoom compiled the human's natural-language stage sequence; complete only the current stage and preserve its order."},
	{ID: "workflow.gate", Text: "Planning, review, and audit stages are read-only. An execute stage following planning/review starts only after the human approves the current plan revision."},
	{ID: "workflow.questions", Text: "Ask for human choices visibly with @human and [PAIRROOM:WAIT]; never block on a hidden request_user_input, elicitation, or terminal prompt."},
	{ID: "observability.inspector", Text: "Keep shared-room responses focused on conclusions, evidence, disagreements, blockers, and next actions; detailed tool activity is projected separately."},
	{ID: "collaboration.selective", Text: "Request a peer turn only when independent analysis or review can materially change the outcome."},
	{ID: "context.handoff", Text: "When a peer turn is needed, add a concise [PAIRROOM:HANDOFF] block with goal, changed scope, evidence, risks, and exact ask."},
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
	contract := Contract{Version: Version, Actor: selection.Actor, Role: selection.Role, RoutingMode: selection.RoutingMode, Rules: append([]Rule(nil), baseRules...)}
	contract.Rules = append(contract.Rules, mentionRules(selection.Actor)...)
	contract.Rules = append(contract.Rules, roleRules(selection.Role)...)
	contract.Rules = append(contract.Rules, routingRules(selection.RoutingMode)...)
	return contract, nil
}

func mentionRules(actor model.ActorID) []Rule {
	peerRule := Rule{ID: "routing.peer-mention", Text: "Claude Code uses @codex and Codex uses @claude only when that peer must receive and answer the response."}
	if actor.ValidParticipant() {
		peer := model.OtherParticipant(actor)
		peerRule.Text = fmt.Sprintf("Use @%s only when %s must receive and answer the response.", peer, peer.DisplayName())
	}
	return []Rule{peerRule, {ID: "routing.human-mention", Text: "Use @human when blocked on a user decision."}}
}

func roleRules(role model.ParticipantRole) []Rule {
	rules := map[model.ParticipantRole]Rule{
		model.RoleDriver:   {ID: "role.driver", Text: "As driver, implement and verify requested work only when the current stage permits execution. For independent review, provide a compact handoff and end [PAIRROOM:IMPLEMENTED]."},
		model.RoleReviewer: {ID: "role.reviewer", Text: "As reviewer, inspect independently and read-only. End with [PAIRROOM:REVIEW_APPROVED], or [PAIRROOM:REVIEW_CHANGES] plus a compact handoff."},
		model.RolePeer:     {ID: "role.peer", Text: "As peer, discuss and investigate as an equal technical partner; edit only in an explicit execute stage or when the human directly authorizes it."},
	}
	if role != "" {
		return []Rule{rules[role]}
	}
	return []Rule{rules[model.RoleDriver], rules[model.RoleReviewer], rules[model.RolePeer]}
}

func routingRules(mode model.RoutingMode) []Rule {
	rules := map[model.RoutingMode]Rule{
		model.RoutingManual:     {ID: "routing.manual", Text: "Manual mode never starts an ordinary peer turn automatically; compiled workflow stages remain explicit human routing and still advance."},
		model.RoutingMentions:   {ID: "routing.mentions", Text: "Mentions mode routes explicit @claude, @codex, or @peer. Valid implementation/review markers hand work between the current Driver and Reviewer outside compiled workflows."},
		model.RoutingRoundtable: {ID: "routing.roundtable", Text: "Roundtable alternates peers within the hop budget. [PAIRROOM:IMPLEMENTED] hands Driver work to Reviewer; [PAIRROOM:REVIEW_CHANGES] returns it; [PAIRROOM:REVIEW_APPROVED], [PAIRROOM:CONSENSUS], [PAIRROOM:WAIT], [PAIRROOM:BLOCKED], and [PAIRROOM:DONE] stop."},
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
