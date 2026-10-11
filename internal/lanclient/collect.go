package lanclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
	"github.com/sean2077/pairroom/internal/prompt"
	"github.com/sean2077/pairroom/internal/protocol"
	"github.com/sean2077/pairroom/internal/relay"
)

type collectResult struct {
	Claim              *relay.Claim `json:"claim"`
	ForegroundRequired bool         `json:"foreground_required,omitempty"`
}

func (c *Client) collect(ctx context.Context, req relayRequest, auth relay.Auth) (collectResult, error) {
	seconds := req.TimeoutSeconds
	if seconds <= 0 {
		seconds = int(relay.DefaultPark / time.Second)
	}
	if seconds > int(relay.MaxPark/time.Second) {
		return collectResult{}, errors.New("wait timeout exceeds Native park limit")
	}
	collectorDir := filepath.Join(c.dir, "collector")
	if err := privatefile.Mkdir(collectorDir); err != nil {
		return collectResult{}, err
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, 25*time.Millisecond)
	unlock, err := privatelock.Lock(lockCtx, collectorDir)
	cancelLock()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return collectResult{}, ErrCollectorBusy
		}
		return collectResult{}, err
	}
	defer unlock()
	// Foreground use is sufficient for recovery without a resident Service.
	// Settle only original receipts already marked stdout; never reprint a
	// claimed envelope or republish an uncertain response body.
	if err := c.reconcileDeliveryReceipts(ctx); err != nil {
		return collectResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	for {
		r, err := c.authenticated(ctx, auth, false)
		if err != nil {
			return collectResult{}, err
		}
		var head lanshare.HeadResponse
		if err := c.call(ctx, r, "head", lanshare.HeadRequest{Park: req.Park, TimeoutSeconds: seconds}, &head); err != nil {
			// A cancelled or expired wait window is an empty poll, not a host
			// outage: the pinned host answered, and the caller ended the wait.
			if ctx.Err() != nil {
				return collectResult{}, nil
			}
			return collectResult{}, safeError(err)
		}
		if head.Head == nil {
			return collectResult{}, nil
		}
		if req.Park && r.Runtime == model.RuntimeGrok {
			return collectResult{ForegroundRequired: true}, nil
		}
		m := head.Head.Message
		if m.To != auth.Slot || m.TargetGeneration != auth.Generation || m.State != "queued" || !lanshare.ValidID(m.ID) || !lanshare.ValidFingerprint(head.Head.Digest) {
			return collectResult{}, errors.New("LAN head identity mismatch")
		}
		if retainedDelivery(r, m.ID) {
			return collectResult{}, errors.New("LAN original delivery is already retained; inspect its receipt before collecting")
		}
		// Download and verify every byte before the remote claim lease starts.
		// Slow or failed evidence retrieval has no inbox consumption effect.
		envelope, err := c.prepareEnvelope(ctx, r, m)
		if ctx.Err() != nil {
			// Preparation has no claim lease or output receipt. Ending the
			// wait during headers, body, or cache verification is an empty
			// poll; retained unknown deliveries remain untouched.
			return collectResult{}, nil
		}
		if err != nil {
			return collectResult{}, err
		}
		r, err = c.authenticated(ctx, auth, false)
		if err != nil {
			if ctx.Err() != nil {
				return collectResult{}, nil
			}
			return collectResult{}, err
		}
		if err := deliveryCapacity(r); err != nil {
			return collectResult{}, err
		}
		if retainedDelivery(r, m.ID) {
			return collectResult{}, errors.New("LAN original delivery is already retained; inspect its receipt before collecting")
		}
		var response lanshare.ClaimResponse
		if err := c.call(ctx, r, "claim", lanshare.ClaimRequest{ID: m.ID, Digest: head.Head.Digest, Generation: auth.Generation, Park: req.Park}, &response); err != nil {
			if ctx.Err() != nil {
				// The wait window ended — or the caller cancelled — while the
				// claim was in flight. The pinned host answered, so this is not a
				// network outage: a claim it committed and never handed over
				// surfaces later as unknown in history --pending, exactly as a
				// cancelled or expired head returns an empty poll rather than
				// claiming the host was unreachable.
				return collectResult{}, nil
			}
			return collectResult{}, safeError(err)
		}
		if response.Claim == nil {
			if ctx.Err() != nil {
				return collectResult{}, nil
			}
			continue // Never claim an unprepared successor when the head changed.
		}
		claim := response.Claim
		if claim.ID != m.ID || !lanshare.ValidID(claim.Receipt) || claim.Message.ID != m.ID || claim.Message.To != auth.Slot || claim.Message.TargetGeneration != auth.Generation || !sameClaimContent(m, claim.Message) {
			return collectResult{}, errors.New("LAN claim receipt identity mismatch")
		}
		if err := c.retainDelivery(ctx, auth, claim); err != nil {
			return collectResult{}, err
		}
		return collectResult{Claim: &relay.Claim{ID: claim.ID, Receipt: claim.Receipt, Envelope: envelope}}, nil
	}
}

func sameClaimContent(head, claimed relay.Message) bool {
	claimed.State, claimed.UpdatedAt, claimed.ClaimedAt = head.State, head.UpdatedAt, head.ClaimedAt
	claimed.Receipt = head.Receipt
	a, errA := json.Marshal(head)
	b, errB := json.Marshal(claimed)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func (c *Client) prepareEnvelope(ctx context.Context, r record, m relay.Message) (string, error) {
	if r.Room == nil || len(m.Attachments) > attachment.MaxImagesPerMessage {
		return "", errors.New("LAN attachment manifest exceeds limit")
	}
	localKey, _ := r.Identity.Fingerprint()
	var handle string
	if m.From == model.ActorUser {
		// Human provenance is authenticated on the host: the hosting human
		// ("host_owner") or this client's own human round-tripped through the
		// host ("lan:<own key>"). Anything else cannot be attributed, so it
		// fails closed instead of inventing a participant handle.
		switch m.Author {
		case "host_owner":
			handle = protocol.RemoteRoomOwnerHandle
		case "lan:" + localKey:
			handle = protocol.LocalRoomOwnerHandle
		default:
			return "", errors.New("LAN user message has no authenticated author provenance")
		}
	} else {
		handle = model.ParticipantIdentities(r.Room.Runtimes)[m.From].MentionHandle
		if handle == "" {
			return "", errors.New("LAN sender has no admitted Runtime identity")
		}
	}
	input := model.AgentInput{From: m.From, To: m.To, FromHandle: handle, Text: m.Text, Quote: m.Quote}
	if m.From == model.ActorUser && m.Author == "host_owner" {
		input.Text += "\n" + protocol.SharedRoomEnvelopeNotice
	}
	if m.Review != nil {
		input.Text += m.Review.Envelope()
	}
	var total int64
	for _, expected := range m.Attachments {
		if err := validateAttachment(expected); err != nil {
			return "", err
		}
		total += expected.Size
		if total > attachment.MaxTotalImageBytes {
			return "", errors.New("LAN attachment manifest exceeds total limit")
		}
		metadata, path, err := c.cachedEvidence(ctx, r, expected)
		if err != nil {
			return "", err
		}
		input.Attachments = append(input.Attachments, model.AgentAttachment{Attachment: metadata, Path: path})
	}
	return prompt.Envelope(input), nil
}
