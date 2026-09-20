package relayclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// deliverForeground uses one HTTP poll for finite waits <= 30 seconds.
// Longer or unbounded waits renew the existing bounded HTTP operation, without a model
// turn, a held slot-state lock, a new API, or a change to Stop-hook parking.
func deliverForeground(ctx context.Context, c *Client, seconds int, out io.Writer) (bool, error) {
	if err := validateForegroundTimeout(seconds); err != nil {
		return false, err
	}
	if seconds > 0 && seconds <= 30 {
		return deliverOnce(ctx, c, false, seconds, out)
	}
	budget := time.Duration(seconds) * time.Second
	return waitForInbox(ctx, budget, 30*time.Second, func(ctx context.Context, span time.Duration) (bool, error) {
		// The wire API accepts whole seconds. Round a finite last window up,
		// by less than one second, rather than silently dropping its remainder.
		pollSeconds := int((span + time.Second - 1) / time.Second)
		return deliverOnce(ctx, c, false, pollSeconds, out)
	})
}

// validatePublicationReceipt checks identity and idempotency without reflecting the body.
func validatePublicationReceipt(c *Client, o options, msg relay.Message, text string) error {
	to := peerSlot(c.State.Slot)
	if o.to == "@user" {
		to = model.ActorUser
	}
	if !safePart(msg.ID) || len(msg.ID) > 128 || msg.From != c.State.Slot || msg.To != to || msg.State == "" {
		return fmt.Errorf("publication receipt invalid; inspect relay status; recover only with the SAME --id %s", o.id)
	}
	if msg.Text != text {
		return fmt.Errorf("--id %s already refers to a different body; no new message was published; inspect relay status", o.id)
	}
	return nil
}

// finishExchange is the receive half of an explicit send followed by wait.
// Publication and collection are NOT one transaction. --id is supplied before
// send, so even a lost send receipt has a recoverable original idempotency key.
// The result is the next FIFO input, not proof of a reply to this publication.
func finishExchange(ctx context.Context, c *Client, o options, msg relay.Message, out, diagnostic io.Writer) error {
	switch msg.State {
	case "queued", "delivering", "handed_off":
	default:
		return fmt.Errorf("exchange publication %s is not pending or handed off; inspect relay status before continuing", msg.ID)
	}
	// Do not echo the body, credentials or full send response into the model.
	// Stdout remains reserved for the one incoming envelope or its file receipt.
	receipt := publicationReceipt{Published: msg.ID, ClientID: o.id, QueuedDelivery: queuedDeliveryHintFor(ctx, c, msg)}
	if err := writeJSON(diagnostic, receipt); err != nil {
		return fmt.Errorf("publication %s confirmed but diagnostic output failed: %w; inspect relay status, do not resend", msg.ID, err)
	}
	delivered, err := deliverForeground(ctx, c, o.timeout, out)
	if err != nil {
		return fmt.Errorf("publication %s confirmed; collection failed: %w; inspect relay status before further collection, never automatically replay or resend", msg.ID, err)
	}
	if !delivered {
		command := foregroundWaitCommand(c, o.timeout)
		if o.outputFile != "" {
			command += " --output-file " + quoteShellPath(o.outputFile)
		}
		return fmt.Errorf("publication %s confirmed; %w; continue with %s, not another send/exchange", msg.ID, errExchangeWaiting, command)
	}
	return nil
}

var errExchangeWaiting = errors.New("no incoming message before wait timeout (not task completion)")

func foregroundWaitCommand(c *Client, seconds int) string {
	return fmt.Sprintf("pairroom relay wait --repo %s --room %s --slot %d --timeout %d", quoteShellPath(c.State.Workspace), c.State.Room, slotNumber(c.State.Slot), seconds)
}
