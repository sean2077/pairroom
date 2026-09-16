package relayclient

import (
	"context"
	"fmt"
	"io"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

type queuedDeliveryHint struct {
	Notice      string `json:"notice"`
	Command     string `json:"command"`
	WakeCommand string `json:"wake_command,omitempty"`
}

type publicationReceipt struct {
	Published      string              `json:"published"`
	ClientID       string              `json:"client_id"`
	QueuedDelivery *queuedDeliveryHint `json:"queued_delivery,omitempty"`
}

type queuedInboxHint struct {
	Slot        model.ActorID `json:"slot"`
	Queued      int           `json:"queued"`
	Notice      string        `json:"notice"`
	Command     string        `json:"command"`
	WakeCommand string        `json:"wake_command,omitempty"`
}

const codexWakeNudge = "PairRoom inbox has messages for you. Run: pairroom relay wait"

func waitCommand(room string, slot model.ActorID) string {
	return fmt.Sprintf("pairroom relay wait --room %s --slot %d", room, slotNumber(slot))
}

func codexWakeCommand(runtime model.RuntimeKind, sessionID string) string {
	if runtime.Canonical() != model.RuntimeCodex || sessionID == "" {
		return ""
	}
	return "codex queue --thread " + quoteShellPath(sessionID) + " --message " + quoteShellPath(codexWakeNudge)
}

func peerWakeCommand(ctx context.Context, c *Client) string {
	var peer relay.Binding
	if c.call(ctx, "peer", nil, &peer) != nil {
		return ""
	}
	return codexWakeCommand(peer.Runtime, peer.SessionID)
}

func queuedDeliveryHintFor(ctx context.Context, c *Client, msg relay.Message) *queuedDeliveryHint {
	if msg.State != "queued" || msg.To != peerSlot(c.State.Slot) {
		return nil
	}
	return &queuedDeliveryHint{
		Notice:      "Message is queued and was not handed off at this response. PairRoom cannot wake an idle native model; if it remains queued, run this command in the peer's associated native session.",
		Command:     waitCommand(c.State.Room, msg.To),
		WakeCommand: peerWakeCommand(ctx, c),
	}
}

func writeQueuedDeliveryHint(ctx context.Context, diagnostic io.Writer, c *Client, msg relay.Message) {
	hint := queuedDeliveryHintFor(ctx, c, msg)
	if hint == nil {
		return
	}
	_ = writeJSON(diagnostic, map[string]*queuedDeliveryHint{"queued_delivery": hint})
}

func queuedInboxHints(ctx context.Context, c *Client, summary *relay.Summary) []queuedInboxHint {
	hints := make([]queuedInboxHint, 0, len(model.SlotActors()))
	peerWake := ""
	peerWakeLoaded := false
	for _, slot := range model.SlotActors() {
		queued := summary.Inboxes[slot].Queued
		if queued == 0 {
			continue
		}
		notice := "Peer inbox has queued input at this status snapshot. PairRoom cannot wake an idle native model; run this command in the peer's associated native session to collect it."
		if slot == c.State.Slot {
			notice = "This associated inbox has queued input at this status snapshot. PairRoom cannot wake an idle native model; run this command to collect it."
		}
		wakeCommand := ""
		if slot == c.State.Slot {
			wakeCommand = codexWakeCommand(c.State.Runtime, c.State.SessionID)
		} else {
			if !peerWakeLoaded {
				peerWake = peerWakeCommand(ctx, c)
				peerWakeLoaded = true
			}
			wakeCommand = peerWake
		}
		hints = append(hints, queuedInboxHint{Slot: slot, Queued: queued, Notice: notice, Command: waitCommand(c.State.Room, slot), WakeCommand: wakeCommand})
	}
	return hints
}
