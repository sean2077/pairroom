package enterpriseclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/protocol"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/review"
)

// RelayContext is an authenticated observation, not native model state. The
// server computes readiness through its shared Relay projection; reading this
// value neither claims an envelope nor renews any operation automatically.
type RelayContext struct {
	Revision       uint64                    `json:"revision"`
	Summary        relay.Summary             `json:"summary"`
	Binding        relay.Binding             `json:"binding"`
	Peer           relay.Binding             `json:"peer"`
	PeerGeneration uint64                    `json:"peer_generation"`
	Bootstrap      string                    `json:"bootstrap"`
	Collaboration  string                    `json:"collaboration"`
	ReplyExpected  bool                      `json:"reply_expected"`
	ClaimReady     bool                      `json:"claim_ready"`
	Next           *relay.EnterpriseIncoming `json:"next,omitempty"`
}

type nativeRelayRequest struct {
	ID                     string         `json:"id,omitempty"`
	Text                   string         `json:"text,omitempty"`
	To                     model.ActorID  `json:"to,omitempty"`
	ReportSeq              uint64         `json:"report_seq,omitempty"`
	ExpectedPeerGeneration uint64         `json:"expected_peer_generation,omitempty"`
	Receipt                string         `json:"receipt,omitempty"`
	Park                   bool           `json:"park,omitempty"`
	TimeoutSeconds         int            `json:"timeout_seconds,omitempty"`
	Enabled                bool           `json:"enabled,omitempty"`
	SessionID              string         `json:"session_id,omitempty"`
	TranscriptPath         string         `json:"transcript_path,omitempty"`
	AttachmentIDs          []string       `json:"attachment_ids,omitempty"`
	Review                 *review.Anchor `json:"review,omitempty"`
	QuoteID                string         `json:"quote_id,omitempty"`
}
type sendJournal struct {
	Version    int                             `json:"version"`
	BindingID  string                          `json:"binding_id"`
	Generation uint64                          `json:"generation"`
	ClientID   string                          `json:"client_id"`
	Intent     relay.EnterpriseOperationIntent `json:"intent"`
}
type deliveryJournal struct {
	Version                  int                    `json:"version"`
	BindingID                string                 `json:"binding_id"`
	Generation               uint64                 `json:"generation"`
	OperationID              string                 `json:"operation_id"`
	AckID                    string                 `json:"ack_id"`
	Park                     bool                   `json:"park"`
	Phase                    string                 `json:"phase"`
	Claim                    *relay.Claim           `json:"claim,omitempty"`
	StructuredClaim          *relay.StructuredClaim `json:"structured_claim,omitempty"`
	ExpectedMessageID        string                 `json:"expected_message_id,omitempty"`
	ExpectedTargetGeneration uint64                 `json:"expected_target_generation,omitempty"`
}
type RecoveryOperation struct {
	Kind      string `json:"kind"`
	ClientID  string `json:"client_id"`
	Outcome   string `json:"outcome"`
	MessageID string `json:"message_id,omitempty"`
}

func assign(value, result any) error {
	if result == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}
func (b *BindingClient) readDelivery() (deliveryJournal, error) {
	var journal deliveryJournal
	err := privatefile.ReadJSON(filepath.Join(b.dir, "delivery.json"), maxBindingStateBytes, &journal)
	if errors.Is(err, os.ErrNotExist) {
		return journal, nil
	}
	if err != nil {
		return journal, ErrPrivateState
	}
	if journal.Version != 1 || !validID(journal.BindingID) || journal.Generation == 0 || !validID(journal.OperationID) || !validID(journal.AckID) {
		return journal, ErrPrivateState
	}
	if (journal.ExpectedMessageID == "") != (journal.ExpectedTargetGeneration == 0) || journal.ExpectedMessageID != "" && (!validID(journal.ExpectedMessageID) || journal.ExpectedTargetGeneration != journal.Generation) {
		return journal, ErrPrivateState
	}
	if journal.StructuredClaim != nil && (journal.StructuredClaim.ID != journal.ExpectedMessageID || journal.StructuredClaim.Message.TargetGeneration != journal.Generation || relay.ValidateEnterpriseOperationResult(relay.EnterpriseOperationResult{Version: 1, Kind: relay.EnterpriseOperationClaim, StructuredClaim: journal.StructuredClaim}) != nil) {
		return journal, ErrPrivateState
	}
	switch journal.Phase {
	case "claim_unknown", "claim_absent", "claimed", "printed", "ack_unknown":
	default:
		return journal, ErrPrivateState
	}
	if (journal.Phase == "claimed" || journal.Phase == "printed" || journal.Phase == "ack_unknown") && (journal.Claim == nil || journal.Claim.ID == "" || journal.Claim.Receipt == "" || journal.Claim.Envelope == "" || journal.Claim.PeerGeneration == 0) {
		return journal, ErrPrivateState
	}
	return journal, nil
}
func (b *BindingClient) saveDelivery(journal deliveryJournal) error {
	if err := privatefile.WriteJSON(filepath.Join(b.dir, "delivery.json"), journal); err != nil {
		return ErrPrivateState
	}
	return nil
}
func (b *BindingClient) clearDelivery() error {
	err := os.Remove(filepath.Join(b.dir, "delivery.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (b *BindingClient) send(ctx context.Context, r bindingRecord, id string, intent relay.EnterpriseOperationIntent) (relayReply, error) {
	if !validID(id) {
		return relayReply{}, ErrConfiguration
	}
	dir := filepath.Join(b.dir, "sends")
	if err := privatefile.Mkdir(dir); err != nil {
		return relayReply{}, ErrPrivateState
	}
	path := filepath.Join(dir, relay.Digest(id)+".json")
	var original sendJournal
	existing := false
	if err := privatefile.ReadJSON(path, maxBindingStateBytes, &original); err == nil {
		existing = true
		if original.Version != 1 || original.BindingID != r.BindID || original.Generation != r.Generation || original.ClientID != id || original.Intent != intent {
			return relayReply{}, &Error{Outcome: "conflict", Class: "payload_conflict"}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return relayReply{}, ErrPrivateState
	}
	if !existing {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return relayReply{}, ErrPrivateState
		}
		if len(entries) >= 32 {
			return relayReply{}, ErrPending
		}
		original = sendJournal{1, r.BindID, r.Generation, id, intent}
		if err := privatefile.WriteJSON(path, original); err != nil {
			return relayReply{}, ErrPrivateState
		}
	}
	var reply relayReply
	var err error
	if existing {
		reply, err = b.operation(ctx, r, id, intent, true)
		if err != nil {
			return reply, err
		}
	}
	if !existing || reply.Outcome == "not_committed" {
		reply, err = b.operation(ctx, r, id, intent, false)
		if err != nil {
			return reply, err
		}
	}
	// A confirmed send needs no local secret result. If cleanup fails its same
	// journal remains recoverable; that never permits another publication ID.
	_ = os.Remove(path)
	return reply, nil
}

// Relay adapts the existing Native client mechanics to the typed enterprise
// wire. No native session/path or local capability is serialized into HTTP.
func (b *BindingClient) Relay(ctx context.Context, a relay.Auth, action string, payload, result any) error {
	if action == "wait" {
		// Include initial private-lock and identity admission in the same
		// collection budget. Other actions retain their original auth order.
		data, err := json.Marshal(payload)
		if err != nil {
			return ErrConfiguration
		}
		var req nativeRelayRequest
		if payload != nil && decodeWire(data, &req) != nil {
			return ErrConfiguration
		}
		if len(req.AttachmentIDs) > 0 || req.QuoteID != "" || req.Review != nil {
			return ErrConfiguration
		}
		return b.collect(ctx, a, req, result)
	}
	unlock, err := b.lock(ctx)
	if err != nil {
		return err
	}
	defer func() { unlock() }()
	r, err := b.authenticated(ctx, a)
	if err != nil {
		return err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ErrConfiguration
	}
	if action == "history" {
		var query relay.HistoryQuery
		if decodeWire(data, &query) != nil {
			return ErrConfiguration
		}
		page, err := b.history(ctx, r, query)
		if err != nil {
			return err
		}
		return assign(page, result)
	}
	var req nativeRelayRequest
	if payload != nil {
		if err := decodeWire(data, &req); err != nil {
			return ErrConfiguration
		}
	}
	if (len(req.AttachmentIDs) > 0 || req.QuoteID != "" || req.Review != nil) && action != "send" {
		return ErrConfiguration
	}
	switch action {
	case "inspect", "confirm", "peer", "summary", "doctor":
		if action == "confirm" && (req.SessionID != a.SessionID || len(req.TranscriptPath) > 4096 || req.TranscriptPath != "" && !filepath.IsAbs(req.TranscriptPath)) {
			return relay.ErrAuth
		}
		view, err := b.context(ctx, r)
		if err != nil {
			return err
		}
		if action == "peer" {
			return assign(view.Peer, result)
		}
		if action == "summary" {
			return assign(view.Summary, result)
		}
		if action == "doctor" {
			return assign(map[string]any{"protocol": view.Summary.Protocol, "transport": "enterprise", "binding_active": view.Binding.Active, "peer_generation": view.PeerGeneration, "notice": "Use explicit foreground credential renewal when needed. This transport does not control native processes."}, result)
		}
		bound := view.Binding
		bound.SessionID = a.SessionID // local adapter output only
		return assign(bound, result)
	case "send":
		intent := relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, To: req.To, Text: req.Text, ExpectedPeerGeneration: req.ExpectedPeerGeneration}
		intent.QuoteID = req.QuoteID
		if req.Review != nil {
			intent.Review = *req.Review
		}
		intent.AttachmentIDs, err = model.NewEnterpriseAttachmentIDs(req.AttachmentIDs)
		if err != nil {
			return ErrConfiguration
		}
		reply, err := b.send(ctx, r, req.ID, intent)
		if err != nil {
			return err
		}
		return assign(reply.Result.Message, result)
	case "report", "publication":
		intent := relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationReport, Text: req.Text, ReportSeq: req.ReportSeq, ExpectedPeerGeneration: req.ExpectedPeerGeneration}
		reply, err := b.operation(ctx, r, fmt.Sprintf("report-%d", req.ReportSeq), intent, action == "publication")
		if err != nil {
			return err
		}
		if action == "publication" {
			if reply.Outcome == "not_committed" {
				return assign(map[string]any{"accepted": false, "publication": relay.Publication{}}, result)
			}
			return assign(map[string]any{"accepted": true, "publication": reply.Result.Publication}, result)
		}
		return assign(reply.Result.Publication, result)
	case "park":
		intent := relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationPark, ParkEnabled: req.Enabled}
		id, err := b.pendingPark(intent)
		if err != nil {
			return err
		}
		reply, err := b.send(ctx, r, id, intent)
		if err != nil {
			return err
		}
		return assign(reply.Result.Binding, result)
	case "ack":
		return b.ack(ctx, r, req, result)
	default:
		return errors.New("enterprise Native operation is not available; use body-free status and the enterprise Room view")
	}
}

func (b *BindingClient) sendJournals() ([]sendJournal, error) {
	dir := filepath.Join(b.dir, "sends")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || len(entries) > 32 {
		return nil, ErrPrivateState
	}
	var journals []sendJournal
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return nil, ErrPrivateState
		}
		var journal sendJournal
		if privatefile.ReadJSON(filepath.Join(dir, entry.Name()), maxBindingStateBytes, &journal) != nil || journal.Version != 1 || !validID(journal.ClientID) || entry.Name() != relay.Digest(journal.ClientID)+".json" || relay.ValidateEnterpriseOperationIntent(journal.Intent) != nil || (journal.Intent.Kind != relay.EnterpriseOperationSend && journal.Intent.Kind != relay.EnterpriseOperationPark) {
			return nil, ErrPrivateState
		}
		journals = append(journals, journal)
	}
	return journals, nil
}
func (b *BindingClient) pendingPark(intent relay.EnterpriseOperationIntent) (string, error) {
	journals, err := b.sendJournals()
	if err != nil {
		return "", err
	}
	for _, journal := range journals {
		if journal.Intent.Kind == relay.EnterpriseOperationPark {
			if journal.Intent != intent {
				return "", ErrPending
			}
			return journal.ClientID, nil
		}
	}
	return newID("park")
}

// ReconcileOperations verifies pending explicit sends and park changes. It
// never retries a mutation or reflects message bodies into model context.
func (b *BindingClient) ReconcileOperations(ctx context.Context, a relay.Auth) ([]RecoveryOperation, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := b.authenticated(ctx, a)
	if err != nil {
		return nil, err
	}
	journals, err := b.sendJournals()
	if err != nil {
		return nil, err
	}
	var operations []RecoveryOperation
	for _, journal := range journals {
		if journal.BindingID != r.BindID || journal.Generation != r.Generation {
			return nil, ErrPrivateState
		}
		reply, err := b.operation(ctx, r, journal.ClientID, journal.Intent, true)
		if err != nil {
			return operations, err
		}
		op := RecoveryOperation{Kind: string(journal.Intent.Kind), ClientID: journal.ClientID, Outcome: reply.Outcome}
		if reply.Result != nil && reply.Result.Message != nil {
			op.MessageID = reply.Result.Message.ID
		}
		operations = append(operations, op)
		if reply.Outcome != "not_committed" {
			_ = os.Remove(filepath.Join(b.dir, "sends", relay.Digest(journal.ClientID)+".json"))
		}
	}
	return operations, nil
}

func (b *BindingClient) collect(ctx context.Context, a relay.Auth, req nativeRelayRequest, result any) error {
	if req.TimeoutSeconds < 1 || req.TimeoutSeconds > 30 {
		return ErrConfiguration
	}
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
	defer cancel()
	beforeClaimFailure := func(err error) error {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return ctx.Err()
		}
		if pollCtx.Err() != nil && errors.Is(err, pollCtx.Err()) {
			return assign(map[string]any{"claim": nil}, result)
		}
		return err
	}
	idle, reconnect := 0, 0
	var observedRevision uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if pollCtx.Err() != nil {
			return assign(map[string]any{"claim": nil}, result)
		}
		unlock, err := b.lock(pollCtx)
		if err != nil {
			return beforeClaimFailure(err)
		}
		r, err := b.authenticated(pollCtx, a)
		if err != nil {
			unlock()
			return beforeClaimFailure(err)
		}
		journal, err := b.readDelivery()
		if err != nil {
			unlock()
			return err
		}
		if journal.Version != 0 && (journal.BindingID != r.BindID || journal.Generation != r.Generation) {
			unlock()
			return ErrPrivateState
		}
		if journal.Version != 0 && journal.Phase != "claim_absent" {
			unlock()
			return ErrDeliveryUnresolved
		}
		unlock()
		view, err := b.context(pollCtx, r)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if pollCtx.Err() != nil {
				return assign(map[string]any{"claim": nil}, result)
			}
			if !transientContextFailure(err) {
				return err
			}
			delay := contextPollDelay(reconnect, false, rand.Uint64())
			var failure *Error
			if errors.As(err, &failure) && failure.Class == "replay_capacity" {
				delay = max(delay, replayCapacityRetryAfter)
			}
			if err := pauseContextPoll(pollCtx, delay); err != nil {
				continue
			}
			reconnect++
			continue
		}
		reconnect = 0
		if observedRevision != view.Revision {
			idle = 0
			observedRevision = view.Revision
		}
		if req.Park && !view.Binding.ParkEnabled {
			return assign(map[string]any{"claim": nil}, result)
		}
		if req.Park && r.Runtime == model.RuntimeGrok {
			return assign(map[string]any{"claim": nil, "foreground_required": view.Summary.Inboxes[r.Slot].Queued > 0}, result)
		}
		if view.ClaimReady {
			var prepared []model.AgentAttachment
			if view.Next != nil {
				if !validID(view.Next.MessageID) || view.Next.TargetGeneration != r.Generation {
					return unknown("invalid_response")
				}
				prepared, err = b.prefetchAttachments(pollCtx, r, view.Next.Attachments)
				if err != nil {
					return err
				}
			}
			// The collector lock is held by relayclient, but state/status/send
			// remain usable during these read-only polling intervals. Take the
			// short record lock only for claim persistence and its one request.
			unlock, err := b.lock(pollCtx)
			if err != nil {
				return beforeClaimFailure(err)
			}
			defer unlock()
			r, err = b.authenticated(pollCtx, a)
			if err != nil {
				return beforeClaimFailure(err)
			}
			journal, err = b.readDelivery()
			if err != nil {
				return err
			}
			if journal.Version != 0 && (journal.BindingID != r.BindID || journal.Generation != r.Generation || journal.Phase != "claim_absent") {
				return ErrDeliveryUnresolved
			}
			if journal.Version == 0 {
				id, err := newID("claim")
				if err != nil {
					return err
				}
				ackID, err := newID("ack")
				if err != nil {
					return err
				}
				journal = deliveryJournal{Version: 1, BindingID: r.BindID, Generation: r.Generation, OperationID: id, AckID: ackID, Park: req.Park, Phase: "claim_unknown"}
				if view.Next != nil {
					journal.ExpectedMessageID, journal.ExpectedTargetGeneration = view.Next.MessageID, view.Next.TargetGeneration
				}
			} else {
				if journal.ExpectedMessageID != "" && (view.Next == nil || view.Next.MessageID != journal.ExpectedMessageID || view.Next.TargetGeneration != journal.ExpectedTargetGeneration) {
					if err := b.clearDelivery(); err != nil {
						return ErrPrivateState
					}
					return errors.New("original absent claim head changed; inspect and wait again for the current message")
				}
				journal.Phase = "claim_unknown"
			}
			if err := b.saveDelivery(journal); err != nil {
				return err
			}
			reply, err := b.operation(pollCtx, r, journal.OperationID, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim, ParkEnabled: journal.Park, MessageID: journal.ExpectedMessageID, ExpectedTargetGeneration: journal.ExpectedTargetGeneration}, false)
			if err != nil {
				var failure *Error
				if errors.As(err, &failure) && failure.Class == "prepared_head_changed" && failure.Outcome == "refused" {
					journal.Phase = "claim_absent"
					if err := b.saveDelivery(journal); err != nil {
						return err
					}
				}
				return err
			}
			if reply.Result.Empty {
				if err := b.clearDelivery(); err != nil {
					return ErrPrivateState
				}
				return assign(map[string]any{"claim": nil}, result)
			}
			journal.StructuredClaim = reply.Result.StructuredClaim
			if journal.StructuredClaim != nil {
				if err := b.saveDelivery(journal); err != nil {
					return err
				}
				if journal.StructuredClaim.ID != journal.ExpectedMessageID || view.Next == nil {
					return unknown("invalid_response")
				}
				journal.Claim, err = enterpriseAttachmentEnvelope(r, journal.StructuredClaim, prepared)
				if err != nil {
					return err
				}
			} else {
				if len(prepared) != 0 {
					return unknown("invalid_response")
				}
				journal.Claim = reply.Result.Claim
			}
			journal.Phase = "claimed"
			if err := b.saveDelivery(journal); err != nil {
				return err
			}
			return assign(map[string]any{"claim": journal.Claim}, result)
		}
		if req.Park && view.Summary.Inboxes[r.Slot].Queued == 0 && view.Summary.Inboxes[r.Slot].Delivering == 0 && !view.ReplyExpected {
			return assign(map[string]any{"claim": nil}, result)
		}
		_ = pauseContextPoll(pollCtx, contextPollDelay(idle, true, rand.Uint64()))
		idle++
	}
}

// Backoff applies only to side-effect-free context reads. Idle observations
// cap at five seconds; reconnect failures cap at ten, with independent jitter.
// A successful changed revision resets idle delay. Claim/ACK/send never enter
// this retry loop, and the wait deadline also bounds every HTTP read itself.
func contextPollDelay(attempt int, idle bool, entropy uint64) time.Duration {
	limit := 10 * time.Second
	if idle {
		limit = 5 * time.Second
	}
	base := time.Second
	for i := 0; i < attempt && base < limit; i++ {
		base *= 2
	}
	if base > limit {
		base = limit
	}
	delay := base * time.Duration(800+entropy%201) / 1000
	if delay > limit {
		delay = limit
	}
	return delay
}
func pauseContextPoll(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// replayCapacityRetryAfter matches the Service's Retry-After for replay
// admission refusals; a context poll never comes back sooner.
const replayCapacityRetryAfter = time.Second

func replayCapacityRefusal(data []byte) bool {
	var refusal struct {
		Error string `json:"error"`
	}
	return decodeWire(data, &refusal) == nil && refusal.Error == "replay_capacity"
}

func transientContextFailure(err error) bool {
	var failure *Error
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Class {
	case "deployment_unavailable", "response_lost", "connection_failed", "connection_exception", "operation_budget_exceeded", "lease_lost", "replay_capacity":
		return true
	}
	return false
}

// PendingSend returns only this caller's original private write-ahead intent.
// CLI recovery consults it before mutable peer-context admission checks.
func (b *BindingClient) PendingSend(ctx context.Context, a relay.Auth, id string) (relay.EnterpriseOperationIntent, bool, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return relay.EnterpriseOperationIntent{}, false, err
	}
	defer unlock()
	r, err := b.authenticated(ctx, a)
	if err != nil {
		return relay.EnterpriseOperationIntent{}, false, err
	}
	if !validID(id) {
		return relay.EnterpriseOperationIntent{}, false, ErrConfiguration
	}
	var journal sendJournal
	err = privatefile.ReadJSON(filepath.Join(b.dir, "sends", relay.Digest(id)+".json"), maxBindingStateBytes, &journal)
	if errors.Is(err, os.ErrNotExist) {
		return relay.EnterpriseOperationIntent{}, false, nil
	}
	if err != nil {
		return relay.EnterpriseOperationIntent{}, false, ErrPrivateState
	}
	if journal.Version != 1 || journal.BindingID != r.BindID || journal.Generation != r.Generation || journal.ClientID != id || journal.Intent.Kind != relay.EnterpriseOperationSend || relay.ValidateEnterpriseOperationIntent(journal.Intent) != nil {
		return relay.EnterpriseOperationIntent{}, false, ErrPrivateState
	}
	return journal.Intent, true, nil
}

func (b *BindingClient) ack(ctx context.Context, r bindingRecord, req nativeRelayRequest, result any) error {
	journal, err := b.readDelivery()
	if err != nil {
		return err
	}
	if journal.Version == 0 || journal.BindingID != r.BindID || journal.Generation != r.Generation || journal.Claim == nil || journal.Claim.ID != req.ID || journal.Claim.Receipt != req.Receipt || (journal.Phase != "claimed" && journal.Phase != "printed" && journal.Phase != "ack_unknown") {
		return relay.ErrAuth
	}
	// The existing relayclient calls this seam only after stdout succeeded.
	// Persist that fact before sending its ACK; recovery never reprints stdout.
	journal.Phase = "printed"
	if err := b.saveDelivery(journal); err != nil {
		return err
	}
	journal.Phase = "ack_unknown"
	if err := b.saveDelivery(journal); err != nil {
		return err
	}
	reply, err := b.operation(ctx, r, journal.AckID, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: req.ID, Receipt: req.Receipt}, false)
	if err != nil {
		return err
	}
	if !reply.Result.HandedOff {
		return unknown("invalid_response")
	}
	if err := b.clearDelivery(); err != nil {
		return ErrPrivateState
	}
	return assign(map[string]bool{"handed_off": true}, result)
}

// ReconcileDelivery verifies an original delivery key. A committed claim is
// never returned as an envelope here: its original stdout outcome is unknown.
// retryAck is an explicit foreground request, valid only for saved stdout proof.
func (b *BindingClient) ReconcileDelivery(ctx context.Context, a relay.Auth, retryAck bool) ([]RecoveryOperation, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := b.authenticated(ctx, a)
	if err != nil {
		return nil, err
	}
	journal, err := b.readDelivery()
	if err != nil {
		return nil, err
	}
	if journal.Version == 0 {
		return nil, nil
	}
	if journal.BindingID != r.BindID || journal.Generation != r.Generation {
		return nil, ErrPrivateState
	}
	ack := journal.Phase == "printed" || journal.Phase == "ack_unknown"
	id := journal.OperationID
	intent := relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim, ParkEnabled: journal.Park, MessageID: journal.ExpectedMessageID, ExpectedTargetGeneration: journal.ExpectedTargetGeneration}
	if ack {
		id = journal.AckID
		intent = relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: journal.Claim.ID, Receipt: journal.Claim.Receipt}
	}
	reply, err := b.operation(ctx, r, id, intent, true)
	if err != nil {
		return nil, err
	}
	if ack && reply.Outcome == "not_committed" && retryAck {
		reply, err = b.operation(ctx, r, id, intent, false)
		if err != nil {
			return nil, err
		}
	}
	op := RecoveryOperation{Kind: string(intent.Kind), ClientID: id, Outcome: reply.Outcome}
	if journal.Claim != nil {
		op.MessageID = journal.Claim.ID
	}
	if ack {
		if reply.Outcome != "not_committed" && reply.Result.HandedOff {
			if err := b.clearDelivery(); err != nil {
				return nil, ErrPrivateState
			}
		}
		return []RecoveryOperation{op}, nil
	}
	if reply.Outcome == "not_committed" {
		journal.Phase = "claim_absent"
		if err := b.saveDelivery(journal); err != nil {
			return nil, err
		}
		return []RecoveryOperation{op}, nil
	}
	if reply.Result.Empty {
		if err := b.clearDelivery(); err != nil {
			return nil, ErrPrivateState
		}
		return []RecoveryOperation{op}, nil
	}
	if reply.Result.StructuredClaim != nil {
		op.MessageID = reply.Result.StructuredClaim.ID
	} else {
		op.MessageID = reply.Result.Claim.ID
	}
	// Keep the original private receipt in a bounded ledger. Later collection
	// may claim different queued work after the server settles this delivery;
	// this record is never a permission to print its old envelope again.
	dir := filepath.Join(b.dir, "uncertain-deliveries")
	if err := privatefile.Mkdir(dir); err != nil {
		return nil, ErrPrivateState
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, ErrPrivateState
	}
	if len(entries) >= 16 {
		return nil, ErrPending
	}
	journal.Phase, journal.Claim = "claimed", reply.Result.Claim
	journal.StructuredClaim = reply.Result.StructuredClaim
	if err := privatefile.WriteJSON(filepath.Join(dir, relay.Digest(journal.OperationID)+".json"), journal); err != nil {
		return nil, ErrPrivateState
	}
	if err := b.clearDelivery(); err != nil {
		return nil, ErrPrivateState
	}
	return []RecoveryOperation{op}, nil
}

type relayReply struct {
	Outcome  string                           `json:"outcome"`
	Class    string                           `json:"class,omitempty"`
	Error    string                           `json:"error,omitempty"`
	Seq      uint64                           `json:"seq,omitempty"`
	Revision uint64                           `json:"revision,omitempty"`
	Result   *relay.EnterpriseOperationResult `json:"result,omitempty"`
}

func (b *BindingClient) context(ctx context.Context, r bindingRecord) (RelayContext, error) {
	if !r.Grant.ExpiresAt.After(time.Now()) {
		return RelayContext{}, ErrBindingCredentialExpired
	}
	status, data, err := b.parent.request(ctx, http.MethodGet, devicePath+"/rooms/"+r.RoomID+"/relay/context", nil, r.Grant.Token, 64<<10)
	if err != nil {
		return RelayContext{}, err
	}
	if status == http.StatusTooManyRequests && replayCapacityRefusal(data) {
		// Replay admission refuses before authentication or any read, so this
		// side-effect-free observation definitely did not run.
		return RelayContext{}, &Error{Outcome: "not_committed", Class: "replay_capacity"}
	}
	if status != http.StatusOK {
		return RelayContext{}, wireFailure(status, data)
	}
	var view RelayContext
	if err := decodeWire(data, &view); err != nil {
		return view, err
	}
	if view.Revision == 0 || view.Summary.RoomID != r.RoomID || view.Summary.HostMode != model.HostNative || !view.Summary.DynamicBindings || view.Summary.Protocol != protocol.NativeDynamicVersion || view.Binding.BindID != r.BindID || view.Binding.Generation != r.Generation || view.Binding.Slot != r.Slot || view.Binding.Runtime != r.Runtime || !view.Binding.Active || view.Binding.SessionID != "" || view.Binding.TranscriptPath != "" || view.Binding.RemoteKey != "" || view.Peer.Slot != model.OtherParticipant(r.Slot) || view.Peer.SessionID != "" || view.Peer.TranscriptPath != "" || view.Peer.RemoteKey != "" || view.PeerGeneration == 0 || view.Summary.TargetGenerations[model.OtherParticipant(r.Slot)] != view.PeerGeneration || view.Bootstrap == "" || view.Collaboration == "" {
		return RelayContext{}, unknown("invalid_response")
	}
	if _, err := b.currentDevice(r, false); err != nil {
		return RelayContext{}, err
	}
	return view, nil
}

func (b *BindingClient) Context(ctx context.Context, a relay.Auth) (RelayContext, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return RelayContext{}, err
	}
	defer unlock()
	r, err := b.authenticated(ctx, a)
	if err != nil {
		return RelayContext{}, err
	}
	return b.context(ctx, r)
}

func (b *BindingClient) operation(ctx context.Context, r bindingRecord, clientID string, intent relay.EnterpriseOperationIntent, resolve bool) (relayReply, error) {
	if !validID(clientID) || relay.ValidateEnterpriseOperationIntent(intent) != nil {
		return relayReply{}, ErrConfiguration
	}
	if !r.Grant.ExpiresAt.After(time.Now()) {
		return relayReply{}, ErrBindingCredentialExpired
	}
	if intent.Kind == relay.EnterpriseOperationRetry || intent.Kind == relay.EnterpriseOperationCancel {
		return relayReply{}, &Error{Outcome: "refused", Class: "authorization"}
	}
	body := struct {
		ClientID   string                          `json:"client_id"`
		BindingID  string                          `json:"binding_id"`
		Generation uint64                          `json:"generation"`
		Intent     relay.EnterpriseOperationIntent `json:"intent"`
	}{clientID, r.BindID, r.Generation, intent}
	path := devicePath + "/rooms/" + r.RoomID + "/relay"
	if resolve {
		path += "/resolve"
	}
	status, data, err := b.parent.request(ctx, http.MethodPost, path, body, r.Grant.Token, 2<<20)
	if err != nil {
		return relayReply{}, err
	}
	if status != http.StatusOK {
		return relayReply{}, wireFailure(status, data)
	}
	var reply relayReply
	if err := decodeWire(data, &reply); err != nil {
		return reply, err
	}
	if reply.Outcome == "not_committed" {
		if !resolve || reply.Result != nil {
			return relayReply{}, unknown("invalid_response")
		}
		return reply, nil
	}
	if (reply.Outcome != "committed" && reply.Outcome != "replayed") || reply.Result == nil || reply.Result.Kind != intent.Kind || relay.ValidateEnterpriseOperationResult(*reply.Result) != nil {
		return relayReply{}, wireFailure(status, data)
	}
	if err := checkRelayResult(r, intent, *reply.Result); err != nil {
		return relayReply{}, err
	}
	if _, err := b.currentDevice(r, false); err != nil {
		return relayReply{}, err
	}
	return reply, nil
}

func checkRelayResult(r bindingRecord, intent relay.EnterpriseOperationIntent, result relay.EnterpriseOperationResult) error {
	switch intent.Kind {
	case relay.EnterpriseOperationSend:
		to := intent.To
		if to == "" {
			to = model.OtherParticipant(r.Slot)
		}
		m := result.Message
		if m == nil || m.From != r.Slot || m.FromRuntime != r.Runtime || m.FromGeneration != r.Generation || m.To != to || m.Text != intent.Text || to.ValidParticipant() && m.TargetGeneration != intent.ExpectedPeerGeneration {
			return unknown("invalid_response")
		}
		if relay.MatchEnterpriseSendEvidence(intent, *m) != nil {
			return unknown("invalid_response")
		}
	case relay.EnterpriseOperationReport:
		p := result.Publication
		if p == nil || p.BindID != r.BindID || p.Generation != r.Generation || p.ReportSeq != intent.ReportSeq || p.ExpectedPeerGeneration != intent.ExpectedPeerGeneration || p.Message != nil && (p.Message.From != r.Slot || p.Message.FromRuntime != r.Runtime || p.Message.FromGeneration != r.Generation || p.Message.Text != intent.Text) {
			return unknown("invalid_response")
		}
	case relay.EnterpriseOperationPark:
		i := result.Binding
		if i == nil || i.RoomID != r.RoomID || i.BindID != r.BindID || i.Generation != r.Generation || i.Slot != r.Slot || i.AgentID != r.AgentID || i.Runtime != r.Runtime || i.ParkEnabled != intent.ParkEnabled {
			return unknown("invalid_response")
		}
	case relay.EnterpriseOperationClaim:
		if result.StructuredClaim != nil {
			c := result.StructuredClaim
			if intent.MessageID == "" || c.ID != intent.MessageID || c.Message.To != r.Slot || c.Message.TargetGeneration != r.Generation || intent.ExpectedTargetGeneration != r.Generation {
				return unknown("invalid_response")
			}
		} else if result.Claim != nil && intent.MessageID != "" && result.Claim.ID != intent.MessageID {
			return unknown("invalid_response")
		}
	case relay.EnterpriseOperationAck:
	default:
		return errors.New("unsupported enterprise endpoint operation")
	}
	return nil
}
