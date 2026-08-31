package prompt

import (
	"fmt"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
)

// These budgets keep stable collaboration prose out of every native turn.
// Tests intentionally fail when either projection grows past its release gate.
const (
	MaxBootstrapBytes        = 1800
	MaxEnvelopeOverheadBytes = 560
)

// BootstrapPrompt is projected once at the native harness's instruction layer.
// The canonical contract and its deterministic mechanics live in
// internal/protocol and the `pairroom protocol` command.
func BootstrapPrompt(actor model.ActorID) string {
	return protocol.Bootstrap(actor)
}

// SystemPrompt remains the adapter-facing compatibility entry point. Room and
// repository identity are deliberately excluded so the stable bootstrap can be
// reused across Rooms; native cwd and per-turn envelope fields carry dynamics.
func SystemPrompt(actor model.ActorID, roomName, _ string) string {
	return BootstrapPrompt(actor)
}

func Envelope(input model.AgentInput) string {
	var b strings.Builder
	fmt.Fprintln(&b, "[PairRoom message]")
	fmt.Fprintf(&b, "protocol: %s\n", protocol.Version)
	fmt.Fprintf(&b, "message_id: %s\n", input.MessageID)
	fmt.Fprintf(&b, "thread_id: %s\n", input.ThreadID)
	fmt.Fprintf(&b, "hop: %d\n", input.Hop)
	fmt.Fprintf(&b, "from: %s\n", input.From)
	fmt.Fprintf(&b, "to: %s\n", input.To)
	if input.ReplyTo != "" {
		fmt.Fprintf(&b, "reply_to: %s\n", input.ReplyTo)
	}
	fmt.Fprintf(&b, "current_role: %s\n", input.Role)
	if input.Intent != "" {
		fmt.Fprintf(&b, "delivery_intent: %s\n", input.Intent)
	}
	fmt.Fprintf(&b, "routing_mode: %s\n", input.RoutingMode)
	if input.WorkflowID != "" {
		fmt.Fprintf(&b, "workflow_id: %s\n", input.WorkflowID)
		fmt.Fprintf(&b, "workflow_stage: %d\n", input.WorkflowStage+1)
		fmt.Fprintf(&b, "workflow_mode: %s\n", input.WorkflowMode)
	}
	fmt.Fprintf(&b, "remaining_agent_hops: %d\n", max(0, input.MaxHops-input.Hop))
	if len(input.Attachments) > 0 {
		fmt.Fprintln(&b, "attachments:")
		for _, value := range input.Attachments {
			fmt.Fprintf(&b, "- name: %s\n  media_type: %s\n  size: %d\n  id: %s\n", value.Name, value.MediaType, value.Size, value.ID)
			if value.Path != "" {
				fmt.Fprintf(&b, "  local_path: %s\n", value.Path)
			}
		}
	}
	fmt.Fprintf(&b, "\n--- message body ---\n%s\n--- end message ---\n", input.Text)
	return b.String()
}
