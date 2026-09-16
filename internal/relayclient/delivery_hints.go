package relayclient

import (
	"fmt"
	"io"

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
	QueuedDelivery *queuedDeliveryHint `json:"queued_delivery,omitempty"`
}

type queuedInboxHint struct {
	Slot    model.ActorID `json:"slot"`
	Queued  int           `json:"queued"`
	Notice  string        `json:"notice"`
	Command string        `json:"command"`
}

func waitCommand(room string, slot model.ActorID) string {
	return fmt.Sprintf("pairroom relay wait --room %s --slot %d", room, slotNumber(slot))
}

func queuedDeliveryHintFor(c *Client, msg relay.Message) *queuedDeliveryHint {
	if msg.State != "queued" || msg.To != peerSlot(c.State.Slot) {
		return nil
	}
	return &queuedDeliveryHint{
		Notice:  "Message is queued and was not handed off at this response. PairRoom cannot wake an idle native model; if it remains queued, run this command in the peer's associated native session.",
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

func queuedInboxHints(c *Client, summary *relay.Summary) []queuedInboxHint {
	hints := make([]queuedInboxHint, 0, len(model.SlotActors()))
	for _, slot := range model.SlotActors() {
		queued := summary.Inboxes[slot].Queued
		if queued == 0 {
			continue
		}
		notice := "Peer inbox has queued input at this status snapshot. PairRoom cannot wake an idle native model; run this command in the peer's associated native session to collect it."
		if slot == c.State.Slot {
			notice = "This associated inbox has queued input at this status snapshot. PairRoom cannot wake an idle native model; run this command to collect it."
		}
		hints = append(hints, queuedInboxHint{Slot: slot, Queued: queued, Notice: notice, Command: waitCommand(c.State.Room, slot)})
	}
	return hints
}
