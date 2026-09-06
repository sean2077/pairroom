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
	MaxEnvelopeOverheadBytes = 128
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

// Envelope carries dynamic sender/body/media only. Durable MessageID, ThreadID,
// ReplyTo, and Role remain available to native transport and Room diagnostics;
// they are not model instructions. The original body is never summarized.
func Envelope(input model.AgentInput) string {
	var b strings.Builder
	fmt.Fprintln(&b, "[PairRoom message]")
	from := input.FromHandle
	if from == "" {
		from = "@user"
	}
	fmt.Fprintf(&b, "from: %s\n", from)
	if len(input.Attachments) > 0 {
		fmt.Fprintln(&b, "attachments:")
		for _, a := range input.Attachments {
			fmt.Fprintf(&b, "- name: %q; type: %q", a.Name, a.MediaType)
			if a.Path != "" {
				fmt.Fprintf(&b, "; path: %q", a.Path)
			}
			fmt.Fprintln(&b)
		}
	}
	fmt.Fprintf(&b, "\n%s", input.Text)
	return b.String()
}
