// Package protocol owns the versioned collaboration contract shared by the
// PairRoom CLI and the native-agent instruction projections.
package protocol

import (
	"fmt"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

const Version = "pairroom-protocol/v5"

type Selection struct {
	Actor model.ActorID
	Role  model.ParticipantRole
}

type Rule struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Contract struct {
	Version string                `json:"version"`
	Actor   model.ActorID         `json:"actor,omitempty"`
	Role    model.ParticipantRole `json:"role,omitempty"`
	Rules   []Rule                `json:"rules"`
}

func Bootstrap(actor model.ActorID, selfRuntime, peerRuntime model.RuntimeKind) string {
	peer := model.OtherParticipant(actor)
	runtimes := map[model.ActorID]model.RuntimeKind{actor: selfRuntime, peer: peerRuntime}
	identities := model.ParticipantIdentities(runtimes)
	self := identities[actor]
	other := identities[peer]
	return fmt.Sprintf(`You are %s (%s) in PairRoom with a human and %s (%s). Native harness, project, permission, sandbox, and safety rules remain authoritative.

PairRoom:
- [PairRoom message] is current input. The envelope gives exact sender, self, and peer handles. Human instructions win; PairRoom owns the single active turn, transcript, and lifecycle.
- current_role: driver may implement; reviewer is independent/read-only; peer is an equal collaborator. Follow the native harness and workspace permissions for that role.
- Complete all useful work you can in this turn. Include %s only when another response from that Agent is genuinely necessary to finish the human's request. Do not mention the peer merely to acknowledge, agree, thank, or ceremonially return the turn.
- A response without %s ends Agent relay and returns the Room to idle. Any Agent may deliver the final result. No fixed relay packet or control marker exists.
- Use @user when a human decision is required. If @user and an Agent handle both appear, @user wins and PairRoom does not relay.
- Keep conclusions and evidence in chat, tool detail in Inspector.

%s: pairroom protocol --actor %s`, self.DisplayName, self.MentionHandle, other.DisplayName, other.MentionHandle, other.MentionHandle, other.MentionHandle, Version, actor)
}

var baseRules = []Rule{
	{ID: "authority.human", Text: "The human has final authority; newer human instructions take precedence over agent discussion."},
	{ID: "authority.harness", Text: "The native coding harness, project instructions, skills, tools, sandbox, permission rules, and safety policy remain authoritative."},
	{ID: "input.envelope", Text: "Treat each [PairRoom message] envelope as current input; from_handle, self_handle, and peer_handle name the participants, and the delimited body is the sender's message."},
	{ID: "output.verbatim", Text: "The final natural-language response is posted verbatim to the shared Room; make it useful without replaying tool chatter."},
	{ID: "delivery.single-turn", Text: "PairRoom permits one active participant turn. Accepted steer input enters that turn; queued and cross-Agent work waits for a reliable native turn boundary."},
	{ID: "delivery.peer", Text: "Include the exact peer_handle only when another response is necessary to finish the request. That explicit handle is the sole Agent-relay signal."},
	{ID: "delivery.stop", Text: "Without the exact peer_handle, Agent relay ends and the Room returns to idle. Any Agent may deliver the final result."},
	{ID: "delivery.human", Text: "Use @user for a required human decision. @user overrides every Agent handle in the same response."},
	{ID: "observability.inspector", Text: "Keep shared-room responses focused on conclusions, evidence, disagreements, blockers, and next actions; detailed tool activity is projected separately."},
	{ID: "media.attachments", Text: "Inspect every attached image relevant to the request and refer to it by filename when useful."},
	{ID: "media.generated", Text: "Save user-facing generated images inside the repository and reference them with repository-relative Markdown image links."},
	{ID: "convergence.intentional", Text: "Do not mention the peer for acknowledgement, agreement, thanks, or ceremonial turn return. Continue only when another independent response can materially change or complete the outcome."},
}

func Resolve(selection Selection) (Contract, error) {
	if selection.Actor != "" && !selection.Actor.ValidParticipant() {
		return Contract{}, fmt.Errorf("invalid actor %q: use claude or codex", selection.Actor)
	}
	if selection.Role != "" && !selection.Role.Valid() {
		return Contract{}, fmt.Errorf("invalid role %q: use driver, reviewer, or peer", selection.Role)
	}
	contract := Contract{Version: Version, Actor: selection.Actor, Role: selection.Role, Rules: append([]Rule(nil), baseRules...)}
	contract.Rules = append(contract.Rules, roleRules(selection.Role)...)
	return contract, nil
}

func roleRules(role model.ParticipantRole) []Rule {
	rules := map[model.ParticipantRole]Rule{
		model.RoleDriver:   {ID: "role.driver", Text: "As driver, implement and verify within the current native and workspace permissions. Mention the exact peer_handle only when an independent response is still needed."},
		model.RoleReviewer: {ID: "role.reviewer", Text: "As reviewer, inspect independently and read-only. Report a complete verdict; mention the exact peer_handle only when that Agent must respond."},
		model.RolePeer:     {ID: "role.peer", Text: "As peer, investigate as an equal technical partner and follow the current native and workspace permissions."},
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
	fmt.Fprintln(&b)
	for _, rule := range contract.Rules {
		fmt.Fprintf(&b, "[%s] %s\n", rule.ID, rule.Text)
	}
	return b.String()
}
