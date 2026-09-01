// Package protocol owns the versioned collaboration contract shared by the
// PairRoom CLI and the native-agent instruction projections.
package protocol

import (
	"fmt"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

const Version = "pairroom-protocol/v4"

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
- [PairRoom message] is current input. Human instructions win; PairRoom owns the single active turn, transcript, lifecycle, and compiled workflow sequence.
- current_role: driver may implement; reviewer is independent/read-only; peer investigates. workflow_mode plan/review/audit is read-only; execute is allowed only after PairRoom's human-approval gate.
- Complete only the current turn or workflow stage. Never start an informal IM loop or wait on a hidden terminal prompt.
- To pass control, add a concise [PAIRROOM:HANDOFF]...[/PAIRROOM:HANDOFF] with goal, evidence, risks, and exact ask, then end [PAIRROOM:NEXT]. PairRoom starts the peer only after your native turn completes.
- End [PAIRROOM:DONE] when no peer turn is needed. Ask humans visibly with @human and [PAIRROOM:WAIT]; use [PAIRROOM:BLOCKED] for an unresolved blocker.
- Keep conclusions and evidence in chat, tool detail in Inspector.

%s: pairroom protocol --actor %s`, actor.DisplayName(), peer.DisplayName(), Version, actor)
}

var baseRules = []Rule{
	{ID: "authority.human", Text: "The human has final authority; newer human instructions supersede agent discussion."},
	{ID: "authority.harness", Text: "The native coding harness, project instructions, skills, tools, sandbox, permission rules, and safety policy remain authoritative."},
	{ID: "input.envelope", Text: "Treat each [PairRoom message] envelope as current input; fields describe Room state and the delimited body is the sender's message."},
	{ID: "output.verbatim", Text: "The final natural-language response is posted verbatim to the shared Room; make it useful without replaying tool chatter."},
	{ID: "delivery.single-turn", Text: "PairRoom permits one active participant turn. Same-participant messages steer that turn; peer work waits until the owner completes."},
	{ID: "delivery.next", Text: "Request the peer only with a concise HANDOFF followed by [PAIRROOM:NEXT]. Plain @peer-style prose never starts another Agent."},
	{ID: "delivery.stop", Text: "Use [PAIRROOM:DONE] when the task can return to the human, [PAIRROOM:WAIT] for a human decision, or [PAIRROOM:BLOCKED] for an unresolved blocker."},
	{ID: "workflow.natural", Text: "When workflow_id and workflow_mode are present, PairRoom compiled the human's natural-language stage sequence; complete only the current stage and preserve its order."},
	{ID: "workflow.gate", Text: "Planning, review, and audit stages are read-only. An execute stage following planning/review starts only after the human approves the current plan revision."},
	{ID: "workflow.questions", Text: "Ask for human choices visibly with @human and [PAIRROOM:WAIT]; never block on a hidden request_user_input, elicitation, or terminal prompt."},
	{ID: "observability.inspector", Text: "Keep shared-room responses focused on conclusions, evidence, disagreements, blockers, and next actions; detailed tool activity is projected separately."},
	{ID: "media.attachments", Text: "Inspect every attached image relevant to the request and refer to it by filename when useful."},
	{ID: "media.generated", Text: "Save user-facing generated images inside the repository and reference them with repository-relative Markdown image links."},
	{ID: "convergence.bounded", Text: "Avoid agreement loops; request only a peer turn that can materially change the outcome, and stop when it cannot."},
}

func Resolve(selection Selection) (Contract, error) {
	if selection.Actor != "" && !selection.Actor.ValidParticipant() {
		return Contract{}, fmt.Errorf("invalid actor %q: use claude or codex", selection.Actor)
	}
	if selection.Role != "" && !selection.Role.Valid() {
		return Contract{}, fmt.Errorf("invalid role %q: use driver, reviewer, or peer", selection.Role)
	}
	mode := selection.RoutingMode
	if mode == "" {
		mode = model.RoutingTurns
	}
	if !mode.Valid() {
		return Contract{}, fmt.Errorf("invalid routing mode %q: only %q is supported", selection.RoutingMode, model.RoutingTurns)
	}
	contract := Contract{Version: Version, Actor: selection.Actor, Role: selection.Role, RoutingMode: mode, Rules: append([]Rule(nil), baseRules...)}
	contract.Rules = append(contract.Rules, roleRules(selection.Role)...)
	return contract, nil
}

func roleRules(role model.ParticipantRole) []Rule {
	rules := map[model.ParticipantRole]Rule{
		model.RoleDriver:   {ID: "role.driver", Text: "As driver, implement and verify only when the current stage permits execution. Hand independent review to the peer with HANDOFF + NEXT."},
		model.RoleReviewer: {ID: "role.reviewer", Text: "As reviewer, inspect independently and read-only. Use DONE when approved; use HANDOFF + NEXT when the driver must address findings."},
		model.RolePeer:     {ID: "role.peer", Text: "As peer, investigate as an equal technical partner; edit only in an explicit execute stage or when the human directly authorizes it."},
	}
	if role != "" {
		return []Rule{rules[role]}
	}
	return []Rule{rules[model.RoleDriver], rules[model.RoleReviewer], rules[model.RolePeer]}
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
	fmt.Fprintf(&b, "routing_mode: %s\n", model.RoutingTurns)
	fmt.Fprintln(&b)
	for _, rule := range contract.Rules {
		fmt.Fprintf(&b, "[%s] %s\n", rule.ID, rule.Text)
	}
	return b.String()
}
