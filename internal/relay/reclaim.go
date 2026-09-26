package relay

import (
	"errors"
	"fmt"

	"github.com/sean2077/pairroom/internal/attachment"
)

// ReclaimAttachments removes up to attachment.ReclaimBatch stored uploads
// that are older than attachment.ReclaimGrace and referenced by no message in
// this Room, and returns how many it removed. Every message fact ever
// replayed stays in e.messages, whatever its state (queued, delivering,
// handed_off, unknown, cancelled, human) and whether it came from send,
// retry or a Stop publication; quoted images are merged into the quoting
// message's own attachments. The reference check and removal share e.mu with
// sendLocked, Retry and Claim, so a send either commits its reference first
// (and the attachment is kept) or resolves after removal and fails before
// anything is published. Removal touches only the attachment directory,
// never the Event Log.
func (e *Engine) ReclaimAttachments() (int, error) {
	if e.cfg.Media == nil {
		return 0, nil
	}
	cutoff := e.cfg.Now().Add(-attachment.ReclaimGrace)
	// Listing reads the directory and small manifests only; keep it outside
	// the admission lock. A stale list is safe: each ID is rechecked below.
	candidates, err := e.cfg.Media.ReclaimCandidates(cutoff)
	if err != nil || len(candidates) == 0 {
		return 0, err
	}
	return e.discardUnreferenced(candidates)
}

// discardUnreferenced rechecks a possibly stale candidate list against the
// current projection under the admission lock and removes the unreferenced
// entries, at most attachment.ReclaimBatch of them.
func (e *Engine) discardUnreferenced(candidates []string) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// After a failed append the Event Log may hold a reference the projection
	// never applied; only a healthy, open projection proves absence.
	if err := e.available(); err != nil {
		return 0, err
	}
	referenced := make(map[string]bool)
	for _, m := range e.messages {
		for _, a := range m.Attachments {
			referenced[a.ID] = true
		}
	}
	removed := 0
	var result error
	for _, id := range candidates {
		if removed >= attachment.ReclaimBatch {
			break
		}
		if referenced[id] {
			continue
		}
		ok, err := e.cfg.Media.Discard(id)
		if ok {
			removed++
		}
		if err != nil {
			result = errors.Join(result, err)
		}
	}
	return removed, result
}

// unavailableAttachment turns a missing upload into a clear, final send
// error. It happens when an unconfirmed draft outlived reclamation of its
// upload. Reclamation removes only IDs no message references, so nothing
// with this payload was published and the same ID can never succeed.
func unavailableAttachment(err error) error {
	if errors.Is(err, attachment.ErrUnknown) {
		return fmt.Errorf("%w; nothing was published: an upload that no message references is removed after %d days, so forget this unsent draft and attach the image again in a new message", err, int(attachment.ReclaimGrace.Hours()/24))
	}
	return err
}
