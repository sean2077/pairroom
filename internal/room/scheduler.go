package room

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
)

// The Room-owned FIFO and single native Turn owner. See the lock-order note on
// Engine before changing how these functions acquire locks.

// scheduleDelivery enforces PairRoom's turn-by-turn invariant. The active
// participant may receive steering messages, but the peer is queued until the
// current native turn reports completion. Reserving the owner before starting
// the goroutine closes the race where two user messages could start both
// runtimes before either adapter emitted a working state.
func (e *Engine) scheduleDelivery(ctx context.Context, message model.Message, target model.ActorID) {
	e.scheduleDeliveryMode(ctx, message, target, false)
}

func (e *Engine) scheduleDeliveryMode(ctx context.Context, message model.Message, target model.ActorID, forceQueue bool) {
	if !target.ValidParticipant() {
		return
	}
	e.turnMu.Lock()
	owner := e.turnOwner
	steer := false
	switch {
	case owner == "" && len(e.turnQueue) == 0:
		e.turnOwner = target
		e.turnSubmitting++
		e.turnBoundarySeen = false
	case owner == "":
		// A queue can outlive its owner during a cancellation or recovery race.
		// Keep the Event Log order authoritative: put the new item behind any
		// existing FIFO work, then reserve the oldest still-awaiting item.
		detail := "queued behind earlier Room FIFO work"
		if !e.delivery(message.ID, target, model.DeliveryQueued, detail) {
			e.turnMu.Unlock()
			return
		}
		e.enqueueDeliveryLocked(scheduledDelivery{message: message, target: target, forceQueue: forceQueue})
		next := e.reserveNextLocked()
		e.turnMu.Unlock()
		e.processing(message.ID, target, model.ProcessingWaiting, detail, "")
		e.startScheduledDelivery(next)
		return
	case forceQueue || owner != target || message.Intent == model.IntentQueue:
		detail := fmt.Sprintf("queued until %s completes the active turn", e.participantName(owner))
		if !e.delivery(message.ID, target, model.DeliveryQueued, detail) {
			e.turnMu.Unlock()
			return
		}
		e.enqueueDeliveryLocked(scheduledDelivery{message: message, target: target, forceQueue: forceQueue})
		e.turnMu.Unlock()
		e.processing(message.ID, target, model.ProcessingWaiting, detail, "")
		return
	default:
		steer = true
		e.turnSubmitting++
	}
	e.turnMu.Unlock()
	go e.runScheduledDelivery(ctx, message, target, steer)
}

func (e *Engine) resumeRestoredDeliveries(ctx context.Context) {
	e.turnMu.Lock()
	restored := append([]scheduledDelivery(nil), e.restoredDeliveries...)
	e.restoredDeliveries = nil
	e.turnMu.Unlock()
	for _, delivery := range restored {
		e.scheduleDeliveryMode(e.runtimeContext(ctx), delivery.message, delivery.target, delivery.forceQueue)
	}
}

func (e *Engine) runScheduledDelivery(ctx context.Context, message model.Message, target model.ActorID, steer bool) {
	fallback := e.deliver(ctx, message, target, steer)
	var next *scheduledDelivery
	e.turnMu.Lock()
	if e.turnOwner == target && e.turnSubmitting > 0 {
		e.turnSubmitting--
	}
	if fallback != "" && e.deliveryAwaitingNative(message.ID, target) {
		queued := scheduledDelivery{message: message, target: target}
		if e.queuedDeliveryExistsLocked(message.ID, target) {
			// A duplicate fallback callback must not enqueue the same Room message
			// twice. The durable Delivery state remains the single source of truth;
			// this in-memory check only closes the scheduling duplication window.
		} else {
			e.enqueueDeliveryLocked(queued)
		}
		if e.turnOwner == "" {
			next = e.reserveNextLocked()
		}
	}
	e.turnMu.Unlock()
	if fallback != "" {
		e.processingFallback(message.ID, target, model.ProcessingWaiting, fallback)
	}
	e.startScheduledDelivery(next)
	e.finishTurnIfIdle(target, true)
}

func (e *Engine) queuedDeliveryExistsLocked(messageID string, target model.ActorID) bool {
	for _, queued := range e.turnQueue {
		if queued.message.ID == messageID && queued.target == target {
			return true
		}
	}
	return false
}

// enqueueDeliveryLocked keeps the Room FIFO ordered by durable Message Seq.
// Delivery goroutines can report a steer fallback after a later message has
// already been queued; appending in callback order would let that older
// message overtake the Event Log order. Zero-sequence test/recovery fixtures
// retain insertion order because they have no durable ordering key.
func (e *Engine) enqueueDeliveryLocked(delivery scheduledDelivery) {
	insertAt := len(e.turnQueue)
	if delivery.message.Seq > 0 {
		for index, existing := range e.turnQueue {
			if existing.message.Seq == 0 || delivery.message.Seq < existing.message.Seq ||
				(delivery.message.Seq == existing.message.Seq && delivery.target < existing.target) {
				insertAt = index
				break
			}
		}
	}
	e.turnQueue = append(e.turnQueue, scheduledDelivery{})
	copy(e.turnQueue[insertAt+1:], e.turnQueue[insertAt:])
	e.turnQueue[insertAt] = delivery
}

// reserveNextLocked removes the oldest still-awaiting FIFO item and reserves
// its native owner before the submission goroutine starts. turnMu must be held.
func (e *Engine) reserveNextLocked() *scheduledDelivery {
	for len(e.turnQueue) > 0 {
		candidate := e.turnQueue[0]
		e.turnQueue[0] = scheduledDelivery{}
		e.turnQueue = e.turnQueue[1:]
		if !e.deliveryAwaitingNative(candidate.message.ID, candidate.target) {
			e.recordSupersededQueueDrop(candidate.message.ID, candidate.target)
			continue
		}
		e.turnOwner = candidate.target
		e.turnSubmitting = 1
		e.turnBoundarySeen = false
		return &candidate
	}
	return nil
}

// finishTurn releases the current owner and starts exactly one queued
// delivery. The next delivery reserves ownership before its goroutine begins,
// preserving serialization even when runtime callbacks arrive concurrently.
func (e *Engine) finishTurn(actor model.ActorID) {
	e.turnMu.Lock()
	next := e.finishTurnLocked(actor)
	e.turnMu.Unlock()
	e.startScheduledDelivery(next)
}

// finishTurnLocked transitions ownership while turnMu is held.
func (e *Engine) finishTurnLocked(actor model.ActorID) *scheduledDelivery {
	if e.turnOwner != actor {
		return nil
	}
	e.turnOwner = ""
	e.turnSubmitting = 0
	e.turnBoundarySeen = false
	return e.reserveNextLocked()
}

func (e *Engine) startScheduledDelivery(next *scheduledDelivery) {
	if next == nil {
		return
	}
	go e.runScheduledDelivery(e.runtimeContext(context.Background()), next.message, next.target, next.steer)
}

// finishTurnIfIdle releases ownership only when no immediate submission is in
// flight and no input already accepted by the owner's native runtime remains
// unfinished. Room-queued deliveries do not block the release that starts them.
func (e *Engine) finishTurnIfIdle(actor model.ActorID, allowWithoutBoundary bool) {
	e.turnMu.Lock()
	if e.turnOwner != actor || e.turnSubmitting > 0 || (!allowWithoutBoundary && !e.turnBoundarySeen) {
		e.turnMu.Unlock()
		return
	}
	e.mu.RLock()
	busy := false
	for _, message := range e.snapshot.Messages {
		state := message.Processing[actor]
		if state != model.ProcessingWaiting && state != model.ProcessingWorking {
			continue
		}
		switch message.Delivery[actor] {
		case model.DeliverySubmitting, model.DeliveryStarted, model.DeliveryInjected:
			busy = true
		}
		if busy {
			break
		}
	}
	e.mu.RUnlock()
	if busy {
		e.turnMu.Unlock()
		return
	}
	next := e.finishTurnLocked(actor)
	e.turnMu.Unlock()
	e.startScheduledDelivery(next)
}

// recordSupersededQueueDrop writes the DeliverySkipped transition for a FIFO
// item dropped because its processing reached a terminal state (cancelled or
// failed) while it was still queued, so the projection never shows a message
// stuck in queued+cancelled.
func (e *Engine) recordSupersededQueueDrop(messageID string, target model.ActorID) {
	e.mu.RLock()
	message, ok := e.findMessageLocked(messageID)
	skip := false
	if ok {
		delivery := message.Delivery[target]
		skip = message.Processing[target].Terminal() &&
			(delivery == model.DeliveryPending || delivery == model.DeliveryQueued)
	}
	e.mu.RUnlock()
	if skip {
		e.delivery(messageID, target, model.DeliverySkipped, "removed from the FIFO after the message reached a terminal processing state while queued")
	}
}

func (e *Engine) deliveryAwaitingNative(messageID string, target model.ActorID) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	message, ok := e.findMessageLocked(messageID)
	if !ok {
		return false
	}
	processing := message.Processing[target]
	delivery := message.Delivery[target]
	return !processing.Terminal() && (delivery == model.DeliveryPending || delivery == model.DeliveryQueued)
}

// deliveryCanFallback is the narrow acceptance-window check used after a
// native steer reports unavailable/rejected. The message is still in
// `submitting` at that point, so it cannot use deliveryAwaitingNative (which is
// intentionally limited to Room-owned FIFO states).
func (e *Engine) deliveryCanFallback(messageID string, target model.ActorID) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	message, ok := e.findMessageLocked(messageID)
	if !ok || message.Processing[target].Terminal() {
		return false
	}
	switch message.Delivery[target] {
	case model.DeliverySubmitting, model.DeliveryPending, model.DeliveryQueued:
		return true
	default:
		return false
	}
}

func (e *Engine) lockDelivery(ctx context.Context, actor model.ActorID) (func(), error) {
	lock := e.deliveryMu[actor]
	if lock == nil {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case lock <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-lock
			return nil, err
		}
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *Engine) lockAllDeliveries(ctx context.Context) (func(), error) {
	slot1Unlock, err := e.lockDelivery(ctx, model.ActorSlot1)
	if err != nil {
		return nil, err
	}
	slot2Unlock, err := e.lockDelivery(ctx, model.ActorSlot2)
	if err != nil {
		slot1Unlock()
		return nil, err
	}
	return func() {
		slot2Unlock()
		slot1Unlock()
	}, nil
}

func (e *Engine) deliver(ctx context.Context, message model.Message, target model.ActorID, steer bool) string {
	unlock, err := e.lockDelivery(ctx, target)
	if err != nil {
		detail := "wait for participant delivery serialization: " + err.Error()
		e.delivery(message.ID, target, model.DeliveryFailed, detail)
		e.processing(message.ID, target, model.ProcessingFailed, detail, "")
		return ""
	}
	defer unlock()
	// A queued item can be cancelled after the scheduler reserves ownership but
	// before this goroutine acquires the delivery lock. Recheck durable lifecycle
	// state at the actual native submission boundary to prevent ghost execution.
	if !e.deliveryAwaitingNative(message.ID, target) {
		return ""
	}
	adapter, err := e.adapter(target)
	if err != nil {
		e.delivery(message.ID, target, model.DeliveryFailed, err.Error())
		e.processing(message.ID, target, model.ProcessingFailed, "input was not submitted: "+err.Error(), "")
		return ""
	}

	e.mu.RLock()
	participant := e.snapshot.Participants[target]
	e.mu.RUnlock()
	quote, media, err := e.deliveryQuote(message)
	if err != nil {
		e.delivery(message.ID, target, model.DeliveryFailed, err.Error())
		e.processing(message.ID, target, model.ProcessingFailed, "quoted message resolution failed: "+err.Error(), "")
		return ""
	}
	attachments, err := e.agentAttachments(media)
	if err != nil {
		e.delivery(message.ID, target, model.DeliveryFailed, err.Error())
		e.processing(message.ID, target, model.ProcessingFailed, "image resolution failed: "+err.Error(), "")
		return ""
	}
	runtimes := e.runtimeKinds()
	identities := model.ParticipantIdentities(runtimes)
	fromHandle := "@user"
	if message.From.ValidParticipant() {
		fromHandle = identities[message.From].MentionHandle
	}
	input := model.AgentInput{
		MessageID:   message.ID,
		ThreadID:    message.ThreadID,
		From:        message.From,
		To:          target,
		FromHandle:  fromHandle,
		SelfHandle:  identities[target].MentionHandle,
		PeerHandle:  identities[model.OtherParticipant(target)].MentionHandle,
		Text:        message.Text,
		ReplyTo:     message.ReplyTo,
		Quote:       quote,
		Access:      nativeAccess(participant),
		Attachments: attachments,
		Intent:      message.Intent,
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if !e.delivery(message.ID, target, model.DeliverySubmitting, "crossing the native submission boundary") {
		return ""
	}
	state := model.DeliveryStarted
	if steer {
		outcome := adapter.Steer(deliveryCtx, input)
		switch outcome.State {
		case agent.SteerAccepted:
			state = model.DeliveryInjected
		case agent.SteerUnavailable, agent.SteerRejected:
			detail := strings.TrimSpace(outcome.Detail)
			if detail == "" {
				detail = "native runtime did not accept same-turn steering"
			}
			// Cancellation can race the native outcome while the delivery is still
			// in `submitting`. Never turn a terminally cancelled input back into a
			// FIFO item; only an actually waiting message gets the one fallback
			// transition.
			if !e.deliveryCanFallback(message.ID, target) {
				return ""
			}
			if !e.delivery(message.ID, target, model.DeliveryQueued, "queued after steer fallback: "+detail) {
				return ""
			}
			return "queued after steer fallback: " + detail
		default:
			detail := strings.TrimSpace(outcome.Detail)
			if detail == "" {
				detail = "native steer ownership is unknown"
			}
			e.delivery(message.ID, target, model.DeliveryFailed, detail)
			e.processing(message.ID, target, model.ProcessingFailed, "steer outcome unknown; explicit retry required: "+detail, "")
			return ""
		}
	} else if err := adapter.StartTurn(deliveryCtx, input); err != nil {
		e.delivery(message.ID, target, model.DeliveryFailed, err.Error())
		e.processing(message.ID, target, model.ProcessingFailed, "runtime did not accept input: "+err.Error(), "")
		e.updateParticipant(target, func(p *model.ParticipantSnapshot) {
			p.State = model.StateError
			p.LastError = err.Error()
			p.LastActivity = time.Now().UTC()
		})
		return ""
	}
	e.delivery(message.ID, target, state, "")
	if state == model.DeliveryStarted && e.cfg.OnSessionMaterialized != nil {
		// StartTurn's deadline governs native input acceptance. Once accepted, the
		// binding commit follows the Engine lifetime so a nearly exhausted delivery
		// deadline cannot discard a vendor identity that already owns real input.
		if err := e.cfg.OnSessionMaterialized(ctx, target, adapter.SessionID()); err != nil {
			_ = adapter.Interrupt(context.Background())
			detail := "native input was accepted but its durable binding could not be materialized: " + err.Error()
			e.processing(message.ID, target, model.ProcessingFailed, detail, "")
			e.updateParticipant(target, func(p *model.ParticipantSnapshot) {
				p.State = model.StateError
				p.LastError = detail
				p.LastActivity = time.Now().UTC()
			})
			return ""
		}
	}
	// Third-party adapters are only required to return a delivery disposition;
	// the richer processing events are optional. Project a conservative fallback
	// so every accepted message has a visible execution lifecycle. Native
	// adapters may emit the same state earlier; identical transitions are safe.
	switch state {
	case model.DeliveryStarted, model.DeliveryInjected:
		e.processingFallback(message.ID, target, model.ProcessingWorking, "accepted by native runtime")
	case model.DeliveryQueued:
		e.processingFallback(message.ID, target, model.ProcessingWaiting, "queued for the next safe turn boundary")
	case model.DeliveryFailed:
		e.processing(message.ID, target, model.ProcessingFailed, "runtime rejected the input before execution", "")
	case model.DeliverySkipped:
		e.processing(message.ID, target, model.ProcessingCancelled, "runtime skipped the input before execution", "")
	}
	return ""
}
