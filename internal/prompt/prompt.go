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
	return protocol.Bootstrap(actor, "", "")
}

func BootstrapPromptWithRuntime(actor model.ActorID, self, peer model.RuntimeKind) string {
	return protocol.Bootstrap(actor, self, peer)
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
	fmt.Fprintf(&b, "from_handle: %s\n", input.FromHandle)
	fmt.Fprintf(&b, "self_handle: %s\n", input.SelfHandle)
	if input.PeerHandle != "" {
		fmt.Fprintf(&b, "peer_handle: %s\n", input.PeerHandle)
	}
	if input.ReplyTo != "" {
		fmt.Fprintf(&b, "reply_to: %s\n", input.ReplyTo)
	}
	fmt.Fprintf(&b, "current_role: %s\n", input.Role)
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
