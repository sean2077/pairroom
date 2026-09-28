package relayclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
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
	ctx, orphaned := watchHarness(ctx)
	budget := time.Duration(seconds) * time.Second
	delivered, err := waitForInbox(ctx, budget, 30*time.Second, func(ctx context.Context, span time.Duration) (bool, error) {
		// The wire API accepts whole seconds. Round a finite last window up,
		// by less than one second, rather than silently dropping its remainder.
		pollSeconds := int((span + time.Second - 1) / time.Second)
		return deliverOnce(ctx, c, false, pollSeconds, out)
	})
	if orphaned() {
		return delivered, errHarnessExited
	}
	return delivered, err
}

var errHarnessExited = errors.New("the native harness that started this collector has exited; collection stopped so input stays queued for the live session")

// harnessWatchInterval bounds how long an orphaned collector can keep claiming.
var harnessWatchInterval = 2 * time.Second

// watchHarness cancels a long foreground collection once the native harness
// that launched it is gone. A background wait can outlive its harness (a
// grandchild is not always killed with the tool shell); left running, it would
// keep claiming input into output nobody reads, hold the collector lock that
// Stop parking needs, and suppress automatic wake as a live collector.
// Lineage is only a liveness observation: no ancestor at start (a plain
// terminal) or an unreadable process table never stops a wait. Cancellation
// usually lands in the Service's blocking wait, before any claim; if it races
// a just-released envelope, the delivery settles as unknown for inspection
// rather than as a handoff into output nobody reads.
func watchHarness(parent context.Context) (context.Context, func() bool) {
	pid, _, ok := harnessAncestor()
	if !ok {
		return parent, func() bool { return false }
	}
	ctx, cancel := context.WithCancelCause(parent)
	go func() {
		ticker := time.NewTicker(harnessWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if harnessGone(pid) {
					cancel(errHarnessExited)
					return
				}
			}
		}
	}()
	return ctx, func() bool {
		orphaned := errors.Is(context.Cause(ctx), errHarnessExited)
		cancel(nil)
		return orphaned
	}
}

// harnessGone reports that the recorded harness process no longer exists
// under a harness name. An intermediate shell exiting does not count: the
// harness itself may still be reading this collector's output.
var harnessGone = func(pid int) bool {
	table, err := processTable()
	if err != nil || len(table) == 0 {
		return false
	}
	entry, ok := table[pid]
	if !ok {
		return true
	}
	_, isHarness := harnessRuntimes[strings.TrimSuffix(strings.ToLower(entry.name), ".exe")]
	return !isHarness
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
	// The sender is about to collect; a peer-collection hint would only add
	// context, and a timeout below prints its own recovery command.
	receipt := publicationReceipt{Published: msg.ID, ClientID: o.id}
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
