package relayclient

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

type queuedDeliveryHint struct {
	Notice  string `json:"notice"`
	Command string `json:"command"`
}

type publicationReceipt struct {
	Published      string              `json:"published"`
	ClientID       string              `json:"client_id"`
	State          string              `json:"state,omitempty"`
	To             model.ActorID       `json:"to,omitempty"`
	QueuedDelivery *queuedDeliveryHint `json:"queued_delivery,omitempty"`
}

type queuedInboxHint struct {
	Slot        model.ActorID `json:"slot"`
	Queued      int           `json:"queued"`
	Notice      string        `json:"notice"`
	Command     string        `json:"command"`
	WakeCommand string        `json:"wake_command,omitempty"`
	WakeNotice  string        `json:"wake_notice,omitempty"`
}

const codexWakeNudge = "PairRoom inbox has messages for you. Run: pairroom relay wait"
const codexWakeNotice = "Human-only, body-free fallback; enabled Rooms manage wake automatically. Agents must not run this command."

type wakeTemplate struct {
	Command string
	Notice  string
}

func waitCommand(room string, slot model.ActorID) string {
	return fmt.Sprintf("pairroom relay wait --room %s --slot %d", room, slotNumber(slot))
}

func codexWakeTemplate(runtime model.RuntimeKind, sessionID string) wakeTemplate {
	if runtime.Canonical() != model.RuntimeCodex || sessionID == "" {
		return wakeTemplate{}
	}
	return wakeTemplate{Command: "codex queue --thread " + quoteShellPath(sessionID) + " --message " + quoteShellPath(codexWakeNudge), Notice: codexWakeNotice}
}

func peerWakeTemplate(ctx context.Context, c *Client) wakeTemplate {
	// Optional body-free advice must not consume the foreground collection budget
	// when a read-only metadata request stalls. Publication is already confirmed.
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var peer relay.Binding
	if c.call(ctx, "peer", nil, &peer) != nil {
		return wakeTemplate{}
	}
	return nativeWakeAdvice(peer.Runtime, peer.SessionID)
}

// queuedDeliveryHintFor is the per-send hint, printed into the sender's model
// context on nearly every send. Keep it short and lookup-free: the sender
// cannot collect for its peer, and agents never run a vendor wake command.
// status --brief carries the full wake advice for explicit inspection.
func queuedDeliveryHintFor(c *Client, msg relay.Message) *queuedDeliveryHint {
	if msg.State != "queued" || msg.To != peerSlot(c.State.Slot) {
		return nil
	}
	return &queuedDeliveryHint{
		Notice:  "Queued, not collected. Service-managed wake; if stalled, use relay status --brief. Peer-session command:",
		Command: waitCommand(c.State.Room, msg.To),
	}
}

func writeQueuedDeliveryHint(diagnostic io.Writer, c *Client, msg relay.Message) {
	hint := queuedDeliveryHintFor(c, msg)
	if hint == nil {
		return
	}
	_ = writeJSON(diagnostic, map[string]*queuedDeliveryHint{"queued_delivery": hint})
}

func queuedInboxHints(ctx context.Context, c *Client, summary *relay.Summary) []queuedInboxHint {
	hints := make([]queuedInboxHint, 0, len(model.SlotActors()))
	peerWake := wakeTemplate{}
	peerWakeLoaded := false
	for _, slot := range model.SlotActors() {
		queued := summary.Inboxes[slot].Queued
		if queued == 0 {
			continue
		}
		notice := "Peer inbox queued; snapshot only. Peer runs the command below. Wake details: relay doctor."
		if slot == c.State.Slot {
			notice = "Input queued; snapshot only. Run the command below. Wake details: relay doctor."
		}
		wake := wakeTemplate{}
		if slot == c.State.Slot {
			wake = nativeWakeAdvice(c.State.Runtime, c.State.SessionID)
		} else {
			if !peerWakeLoaded {
				peerWake = peerWakeTemplate(ctx, c)
				peerWakeLoaded = true
			}
			wake = peerWake
		}
		hints = append(hints, queuedInboxHint{Slot: slot, Queued: queued, Notice: notice, Command: waitCommand(c.State.Room, slot), WakeCommand: wake.Command, WakeNotice: wake.Notice})
	}
	return hints
}

// Advice never asserts that a captured capability or a vendor policy is usable.
// The detailed doctor is an explicit read, not an extra call on every send.
func nativeWakeAdvice(kind model.RuntimeKind, session string) wakeTemplate {
	if kind.Canonical() == model.RuntimeCodex {
		return codexWakeTemplate(kind, session)
	}
	if session == "" {
		return wakeTemplate{}
	}
	switch kind.Canonical() {
	case model.RuntimeClaude:
		return wakeTemplate{Notice: "Claude wake needs a captured inbox and inbound permission. Use relay doctor or receive-only wait; no raw socket command."}
	case model.RuntimeGrok, model.RuntimeGemini:
		return wakeTemplate{Notice: "No Service wake. Use tracked background wait only if completion reaches the model; otherwise foreground wait."}
	}
	return wakeTemplate{}
}
