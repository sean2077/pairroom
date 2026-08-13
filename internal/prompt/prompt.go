package prompt

import (
	"fmt"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

// SystemPrompt is appended to the vendor's own coding-agent system prompt. It
// does not replace the native harness instructions, tools, project files,
// skills, or safety policy.
func SystemPrompt(actor model.ActorID, roomName, repo string) string {
	peer := model.OtherParticipant(actor)
	return fmt.Sprintf(`You are %s, a native coding agent participating in a PairRoom shared room with a human and %s.

PairRoom rules:
- The human has final authority. New human instructions supersede agent discussion.
- Your normal coding harness, project instructions, skills, tools, sandbox, and permission rules remain authoritative.
- Messages arrive in a structured [PairRoom message] envelope. Treat the sender and quoted body as the conversation input.
- Your final natural-language response is posted verbatim to the shared room. Make it useful to both the human and the peer.
- To explicitly request a response from the peer, mention @%s in the final response. Use @human when you need the user. Do not mention the peer merely as prose unless you want another turn.
- Do not invent delivery/read state or claim you sent a private message. PairRoom handles routing.
- Keep implementation/tool details concise in the room; the UI separately exposes commands, file changes, plans, and diffs.
- When you create a screenshot, diagram, chart, or other image that the human should see, save it inside the repository and include a repository-relative Markdown image reference such as ![preview](path/to/image.png) in your final response. PairRoom can then attach a safe preview.
- When your current role is reviewer, inspect independently and do not edit files unless the human explicitly authorizes it.
- Avoid unbounded agreement loops. State disagreements clearly, converge on evidence, and stop mentioning the peer once no further response is needed.
- In roundtable mode, place one standalone control marker at the end when useful: [PAIRROOM:CONTINUE] asks the peer to continue; [PAIRROOM:CONSENSUS], [PAIRROOM:WAIT], [PAIRROOM:BLOCKED], or [PAIRROOM:DONE] stops automatic handoff. The marker is hidden from the room transcript.

Room: %s
Repository: %s`, actor.DisplayName(), peer.DisplayName(), peer, roomName, repo)
}

func Envelope(input model.AgentInput) string {
	roleRule := "Collaborate as a peer. Do not edit unless the message explicitly asks you to implement."
	switch input.Role {
	case model.RoleDriver:
		roleRule = "You are the driver for this room. You may inspect and modify the repository when the task calls for it, then report evidence."
	case model.RoleReviewer:
		roleRule = "You are the reviewer for this room. Independently inspect the repository and proposed work. Do not modify files unless the human explicitly authorizes it."
	case model.RolePeer:
		roleRule = "You are an equal technical peer. Discuss and investigate; only edit when the human explicitly asks you to implement."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[PairRoom message]\n")
	fmt.Fprintf(&b, "message_id: %s\n", input.MessageID)
	fmt.Fprintf(&b, "thread_id: %s\n", input.ThreadID)
	fmt.Fprintf(&b, "hop: %d\n", input.Hop)
	fmt.Fprintf(&b, "from: %s\n", input.From.DisplayName())
	fmt.Fprintf(&b, "to: %s\n", input.To.DisplayName())
	if input.ReplyTo != "" {
		fmt.Fprintf(&b, "reply_to: %s\n", input.ReplyTo)
	}
	fmt.Fprintf(&b, "current_role: %s\n", input.Role)
	fmt.Fprintf(&b, "role_rule: %s\n", roleRule)
	fmt.Fprintf(&b, "routing_mode: %s\n", input.RoutingMode)
	fmt.Fprintf(&b, "remaining_agent_hops: %d\n", max(0, input.MaxHops-input.Hop))
	if len(input.Attachments) > 0 {
		fmt.Fprintf(&b, "attachments:\n")
		for _, value := range input.Attachments {
			fmt.Fprintf(&b, "- name: %s\n  media_type: %s\n  size: %d\n  id: %s\n", value.Name, value.MediaType, value.Size, value.ID)
			if value.Path != "" {
				fmt.Fprintf(&b, "  local_path: %s\n", value.Path)
			}
		}
	}
	fmt.Fprintf(&b, "\n--- message body ---\n%s\n--- end message ---\n", input.Text)

	peer := model.OtherParticipant(input.To)
	fmt.Fprintf(&b, "\nRespond as %s. Keep the shared-room answer focused on conclusions, evidence, disagreements, and next actions; tool details are visible in the Inspector.", input.To.DisplayName())
	if len(input.Attachments) > 0 {
		fmt.Fprintf(&b, " Inspect every attached image that is relevant to the request. PairRoom supplies each image through the target harness's native multimodal input and also lists its durable local path for tool-based inspection. Refer to images by filename in the shared-room answer.")
	}
	switch input.RoutingMode {
	case model.RoutingManual:
		fmt.Fprintf(&b, " PairRoom will not automatically forward your answer to the peer. Mentioning @%s is conversational only in manual mode.", peer)
	case model.RoutingMentions:
		fmt.Fprintf(&b, " Mention @%s only when that peer must receive and answer this message. Mention @human when blocked on a user decision.", peer)
	case model.RoutingRoundtable:
		fmt.Fprintf(&b, " PairRoom will automatically hand your answer to %s unless you stop it. End with exactly one of [PAIRROOM:CONSENSUS], [PAIRROOM:WAIT], [PAIRROOM:BLOCKED], or [PAIRROOM:DONE] when no peer turn is needed. You may use [PAIRROOM:CONTINUE] to make continuation explicit. These control lines are hidden from the room. Do not continue merely to agree.", peer.DisplayName())
	}
	return b.String()
}
