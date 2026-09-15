package relayclient

import (
	"context"
	"io"

	"github.com/sean2077/pairroom/internal/relay"
)

const grokClippedReplyNotice = "PairRoom did not publish this Stop reply because Grok clipped it. If relay was intended, use pairroom relay send/exchange with the COMPLETE original text (send --to @user for a human escalation), not this clipped prefix. If it was already sent explicitly, do not resend. Finish with a short unaddressed reply; never repeat a peer handle after explicit publication."

// grokContinuation carries only a bounded local instruction, never inbox text.
// Readiness/recovery hints share the block cap; empty parks spend no block.
// Nothing here acknowledges a message or claims native/model acceptance.
func grokContinuation(ctx context.Context, c *Client, reason string, out io.Writer) error {
	release, err := lockSlot(ctx, c.Dir)
	if err != nil {
		return err
	}
	defer release()
	current, err := load(c.Dir)
	if err != nil {
		return err
	}
	if current.State.BindID != c.State.BindID || current.State.Generation != c.State.Generation || current.State.SessionID != c.State.SessionID {
		return relay.ErrAuth
	}
	if current.State.Blocks >= relay.MaxBlocks {
		return writeJSON(out, map[string]any{})
	}
	// A pathological workspace path must not overflow Grok's 10,000-character
	// feedback cap. The CLI can resolve or explain any missing selector.
	if len(reason) > 8000 {
		reason = "PairRoom has pending input. In the original associated Room workspace, run pairroom relay wait and process its full result. Do not resend earlier messages."
	}
	next := current.State
	next.Blocks++
	if err := current.persist(next); err != nil {
		return err
	}
	return writeJSON(out, map[string]string{"decision": "block", "reason": reason})
}
