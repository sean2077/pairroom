package room

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

// Room admission: human/Agent messages, cancellation, retry, target and
// attachment resolution. The Event Log records each decision before effects.

// RemoveAttachment shares the routing gate with every new transcript reference.
// The reference check and removal must not race canonicalization + persistence.
func (e *Engine) RemoveAttachment(id string) error {
	e.routingMu.Lock()
	defer e.routingMu.Unlock()
	id = strings.TrimSpace(id)
	if e.AttachmentReferenced(id) {
		return ErrAttachmentReferenced
	}
	if e.cfg.Attachments == nil {
		return errors.New("attachment storage is unavailable")
	}
	return e.cfg.Attachments.Remove(id)
}

func (e *Engine) AttachmentReferenced(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, message := range e.snapshot.Messages {
		for _, value := range message.Attachments {
			if value.ID == id {
				return true
			}
		}
	}
	return false
}

func (e *Engine) Send(ctx context.Context, req SendRequest) (model.Message, error) {
	text := strings.TrimSpace(req.Text)
	intent := req.Intent
	if intent == "" {
		intent = model.IntentSteer
	}
	if !intent.Valid() {
		return model.Message{}, fmt.Errorf("invalid message intent %q", intent)
	}
	e.routingMu.Lock()
	defer e.routingMu.Unlock()
	attachments, err := e.canonicalAttachments(req.Attachments)
	if err != nil {
		return model.Message{}, err
	}
	if text == "" && len(attachments) == 0 {
		return model.Message{}, errors.New("message text or image is required")
	}

	if req.ReplyTo != "" {
		if _, err := e.messageForUserQuote(req.ReplyTo); err != nil {
			return model.Message{}, err
		}
	}

	targets, err := e.resolveUserTargets(text, req.To, req.ReplyTo)
	if err != nil {
		return model.Message{}, err
	}
	if len(targets) == 0 {
		return model.Message{}, errors.New("message has no target; choose an exact participant handle")
	}
	if len(targets) != 1 {
		return model.Message{}, errors.New("a Room message accepts exactly one starting Agent")
	}
	threadID := e.threadForReply(req.ReplyTo)
	message := model.Message{
		ID: model.NewID("msg"), From: model.ActorUser, To: targets, Text: text,
		ReplyTo: req.ReplyTo, Intent: intent,
		ThreadID: threadID, CreatedAt: time.Now().UTC(),
		Delivery:                make(map[model.ActorID]model.DeliveryState, len(targets)),
		DeliveryDetail:          make(map[model.ActorID]string, len(targets)),
		Processing:              make(map[model.ActorID]model.ProcessingState, len(targets)),
		ProcessingDetail:        make(map[model.ActorID]string, len(targets)),
		ProcessingTurn:          make(map[model.ActorID]string, len(targets)),
		ProcessingLastUpdatedAt: make(map[model.ActorID]time.Time, len(targets)),
		Attachments:             attachments,
	}
	for _, target := range targets {
		message.Delivery[target] = model.DeliveryPending
		message.Processing[target] = model.ProcessingWaiting
		message.ProcessingLastUpdatedAt[target] = message.CreatedAt
	}
	event, err := e.record(EventMessageCreated, model.ActorUser, message)
	if err != nil {
		return model.Message{}, err
	}
	message.Seq = event.Seq
	e.cancelQueuedAgentRelaysBefore(message.Seq)
	for _, target := range targets {
		e.scheduleDelivery(e.runtimeContext(ctx), message, target)
	}
	return message, nil
}

func (e *Engine) cancelQueuedAgentRelaysBefore(humanSeq uint64) {
	if humanSeq == 0 {
		return
	}
	type deliveryKey struct {
		messageID string
		target    model.ActorID
	}
	e.turnMu.Lock()
	e.mu.RLock()
	candidates := make([]deliveryKey, 0)
	for _, message := range e.snapshot.Messages {
		if !message.From.ValidParticipant() || message.Seq >= humanSeq {
			continue
		}
		for target, state := range message.Delivery {
			if !target.ValidParticipant() || message.Processing[target].Terminal() {
				continue
			}
			if state == model.DeliveryPending || state == model.DeliveryQueued {
				candidates = append(candidates, deliveryKey{messageID: message.ID, target: target})
			}
		}
	}
	e.mu.RUnlock()

	cancelled := make(map[deliveryKey]struct{}, len(candidates))
	for _, candidate := range candidates {
		if e.deliveryIf(candidate.messageID, candidate.target, model.DeliverySkipped, "cancelled by a newer user instruction before native submission", func(current model.DeliveryState) bool {
			return current == model.DeliveryPending || current == model.DeliveryQueued
		}) {
			cancelled[candidate] = struct{}{}
		}
	}
	kept := e.turnQueue[:0]
	for _, delivery := range e.turnQueue {
		if _, ok := cancelled[deliveryKey{messageID: delivery.message.ID, target: delivery.target}]; ok {
			continue
		}
		kept = append(kept, delivery)
	}
	for index := len(kept); index < len(e.turnQueue); index++ {
		e.turnQueue[index] = scheduledDelivery{}
	}
	e.turnQueue = kept
	e.turnMu.Unlock()
	for delivery := range cancelled {
		e.processing(delivery.messageID, delivery.target, model.ProcessingCancelled, "newer user instruction cancelled the pending Agent relay", "")
	}
	if len(cancelled) > 0 {
		e.notice("info", fmt.Sprintf("A newer user instruction cancelled %d pending Agent relay message(s).", len(cancelled)))
	}
}

func (e *Engine) CancelMessage(ctx context.Context, messageID string, target model.ActorID) error {
	if !target.ValidParticipant() {
		return errors.New("cancel target must be a participant slot: slot1 or slot2")
	}
	e.mu.RLock()
	message, found := e.findMessageLocked(messageID)
	e.mu.RUnlock()
	if !found {
		return fmt.Errorf("unknown message %q", messageID)
	}
	state := message.Processing[target]
	if state != model.ProcessingWaiting && state != model.ProcessingWorking {
		return fmt.Errorf("message is not in flight for %s", e.participantName(target))
	}
	// A Room-level queued delivery has not entered either native harness. Remove
	// only that FIFO item; interrupting the target here would cancel unrelated
	// work in its active native Turn and would make a single-message action lie.
	if e.cancelRoomQueuedDelivery(messageID, target) {
		e.delivery(messageID, target, model.DeliverySkipped, "cancelled before native runtime submission")
		e.processing(messageID, target, model.ProcessingCancelled, "cancelled while waiting in the Room turn queue; no native Turn was interrupted", "")
		return nil
	}

	// Serialize with native submission and reviewer snapshot refresh. Some adapters emit the
	// terminal callback synchronously from Interrupt; holding the delivery scope
	// prevents the next Room FIFO item from entering the Runtime before we have
	// captured the exact set of inputs affected by this native interruption.
	unlock, err := e.lockDelivery(ctx, target)
	if err != nil {
		return err
	}
	defer unlock()
	// Recheck after waiting for a submission boundary. A delivery can become
	// Room-queued or terminal while this cancellation waits for the lock.
	if e.cancelRoomQueuedDelivery(messageID, target) {
		e.delivery(messageID, target, model.DeliverySkipped, "cancelled before native runtime submission")
		e.processing(messageID, target, model.ProcessingCancelled, "cancelled while waiting in the Room turn queue; no native Turn was interrupted", "")
		return nil
	}

	e.mu.RLock()
	message, found = e.findMessageLocked(messageID)
	if !found {
		e.mu.RUnlock()
		return fmt.Errorf("unknown message %q", messageID)
	}
	state = message.Processing[target]
	if state != model.ProcessingWaiting && state != model.ProcessingWorking {
		e.mu.RUnlock()
		return fmt.Errorf("message is no longer in flight for %s", e.participantName(target))
	}
	if message.Delivery[target] == model.DeliveryPending || message.Delivery[target] == model.DeliveryQueued {
		e.mu.RUnlock()
		// The scheduler may already have reserved this item as the next owner, but
		// the delivery lock proves it has not crossed the native boundary. Mark it
		// skipped; deliver() rechecks this state after acquiring the same lock.
		e.delivery(messageID, target, model.DeliverySkipped, "cancelled before native runtime submission")
		e.processing(messageID, target, model.ProcessingCancelled, "cancelled before native runtime submission; no native Turn was interrupted", "")
		return nil
	}
	// Native runtimes often cancel an entire active turn or their own accepted
	// input queue rather than one logical room message. Snapshot every input that
	// has already crossed the native boundary before Interrupt; a synchronous
	// completion callback may release the owner, but later Room FIFO items must
	// never be swept into this cancellation.
	var affected []string
	for _, candidate := range e.snapshot.Messages {
		candidateState := candidate.Processing[target]
		if candidateState != model.ProcessingWaiting && candidateState != model.ProcessingWorking {
			continue
		}
		delivery := candidate.Delivery[target]
		switch delivery {
		case model.DeliverySubmitting, model.DeliveryStarted, model.DeliveryInjected:
			affected = append(affected, candidate.ID)
		case model.DeliveryPending:
			// The requested message may be inside StartTurn's acceptance window. Its
			// cancellation remains explicit even though no narrower native API is
			// available at this boundary. Other pending messages remain in Room FIFO.
			if candidate.ID == messageID {
				affected = append(affected, candidate.ID)
			}
		}
	}
	e.mu.RUnlock()

	adapter, err := e.adapter(target)
	if err != nil {
		return err
	}
	if err := adapter.Interrupt(ctx); err != nil {
		return err
	}
	for _, id := range affected {
		e.processing(id, target, model.ProcessingCancelled, "cancelled by the PairRoom user; native interruption affects inputs already accepted by this participant, while Room-level queued turns are preserved", "")
	}
	e.expireApprovals(target, "message_cancelled")
	e.finishTurnIfIdle(target, false)
	return nil
}

func (e *Engine) cancelRoomQueuedDelivery(messageID string, target model.ActorID) bool {
	e.turnMu.Lock()
	defer e.turnMu.Unlock()
	for i, scheduled := range e.turnQueue {
		if scheduled.message.ID != messageID || scheduled.target != target {
			continue
		}
		copy(e.turnQueue[i:], e.turnQueue[i+1:])
		e.turnQueue[len(e.turnQueue)-1] = scheduledDelivery{}
		e.turnQueue = e.turnQueue[:len(e.turnQueue)-1]
		return true
	}
	return false
}

// Retry creates a new auditable message rather than mutating a past message.
// Reusing the original ID would make a late vendor acknowledgment ambiguous
// and could hide duplicate execution. The caller can retry only targets whose
// previous delivery or processing state is terminal and unsuccessful.
func (e *Engine) Retry(ctx context.Context, messageID string, req RetryRequest) (model.Message, error) {
	e.routingMu.Lock()
	defer e.routingMu.Unlock()

	e.mu.RLock()
	original, found := e.findMessageLocked(messageID)
	if found {
		original = cloneMessage(original) // Late native receipts may still update lifecycle maps.
	}
	e.mu.RUnlock()
	if !found {
		return model.Message{}, fmt.Errorf("unknown message %q", messageID)
	}

	targets, err := normalizeExplicitActors(req.To)
	if err != nil {
		return model.Message{}, err
	}
	if len(targets) == 0 {
		for _, target := range original.To {
			if !target.ValidParticipant() || !retryableTarget(original, target) {
				continue
			}
			targets = append(targets, target)
		}
		targets = model.NormalizeActors(targets)
	}
	if len(targets) == 0 {
		return model.Message{}, errors.New("message has no failed, cancelled, or skipped target to retry")
	}
	if len(targets) != 1 {
		return model.Message{}, errors.New("turn-by-turn retry accepts exactly one Agent target")
	}
	for _, target := range targets {
		if !retryableTarget(original, target) {
			return model.Message{}, fmt.Errorf("delivery to %s is not retryable", e.participantName(target))
		}
	}

	// One source/target may have only one pending retry, even when independent
	// browser sessions race. The original remains auditable and retryable after
	// that child settles; no model input is silently replayed or deduplicated by text.
	e.mu.RLock()
	for _, message := range e.snapshot.Messages {
		if message.RetryOf != original.ID {
			continue
		}
		for _, target := range targets {
			if state := message.Processing[target]; state == model.ProcessingWaiting || state == model.ProcessingWorking {
				e.mu.RUnlock()
				return model.Message{}, errors.New("a retry for this message and participant is already pending")
			}
		}
	}
	e.mu.RUnlock()

	intent := original.Intent
	if intent == "" {
		intent = model.IntentSteer
	}
	if !intent.Valid() {
		return model.Message{}, fmt.Errorf("cannot retry message with invalid intent %q", intent)
	}
	now := time.Now().UTC()
	retry := model.Message{
		ID:                      model.NewID("msg"),
		From:                    original.From,
		To:                      targets,
		Text:                    original.Text,
		ReplyTo:                 original.ReplyTo,
		RetryOf:                 original.ID,
		Intent:                  intent,
		ThreadID:                original.ThreadID,
		CreatedAt:               now,
		Delivery:                make(map[model.ActorID]model.DeliveryState, len(targets)),
		DeliveryDetail:          make(map[model.ActorID]string, len(targets)),
		Processing:              make(map[model.ActorID]model.ProcessingState, len(targets)),
		ProcessingDetail:        make(map[model.ActorID]string, len(targets)),
		ProcessingTurn:          make(map[model.ActorID]string, len(targets)),
		ProcessingLastUpdatedAt: make(map[model.ActorID]time.Time, len(targets)),
		Attachments:             append([]model.Attachment(nil), original.Attachments...),
	}
	if retry.ThreadID == "" {
		retry.ThreadID = model.NewID("thread")
	}
	for _, target := range targets {
		retry.Delivery[target] = model.DeliveryPending
		retry.Processing[target] = model.ProcessingWaiting
		retry.ProcessingLastUpdatedAt[target] = now
	}
	event, err := e.record(EventMessageCreated, model.ActorUser, retry)
	if err != nil {
		return model.Message{}, err
	}
	retry.Seq = event.Seq
	for _, target := range targets {
		e.scheduleDelivery(e.runtimeContext(ctx), retry, target)
	}
	return retry, nil
}

func (e *Engine) resolveUserTargets(text string, explicit []model.ActorID, replyTo string) ([]model.ActorID, error) {
	targets, err := normalizeExplicitActors(explicit)
	if err != nil {
		return nil, err
	}
	if len(targets) > 0 {
		return targets, nil
	}
	mentions := prompt.ParseMentions(text, model.ActorUser, e.runtimeKinds())
	if len(mentions.Ambiguous) > 0 {
		identities := model.ParticipantIdentities(e.runtimeKinds())
		return nil, fmt.Errorf("ambiguous Agent handle %s; use %s or %s", strings.Join(mentions.Ambiguous, ", "), identities[model.ActorSlot1].MentionHandle, identities[model.ActorSlot2].MentionHandle)
	}
	// Removed aliases are ordinary prose once a valid current handle is also
	// present. Reject only an otherwise unaddressed message that still relies on
	// a retired alias, so legacy text cannot silently fall back to Agent 1.
	if len(mentions.RemovedAliases) > 0 && len(mentions.Targets) == 0 {
		return nil, fmt.Errorf("removed Agent handle %s is not routable; use the participant's displayed mention handle", strings.Join(mentions.RemovedAliases, ", "))
	}
	if len(mentions.Targets) > 0 {
		return mentions.Targets, nil
	}
	if replyTo != "" {
		e.mu.RLock()
		replied, found := e.findMessageLocked(replyTo)
		e.mu.RUnlock()
		if found && replied.From.ValidParticipant() {
			return []model.ActorID{replied.From}, nil
		}
	}
	return []model.ActorID{model.ActorSlot1}, nil
}

func normalizeExplicitActors(values []model.ActorID) ([]model.ActorID, error) {
	canonical := make([]model.ActorID, 0, len(values))
	for _, value := range values {
		actor := canonicalHTTPActor(value)
		if !actor.ValidParticipant() {
			return nil, fmt.Errorf("invalid Agent recipient %q; use slot1 or slot2", value)
		}
		canonical = append(canonical, actor)
	}
	return model.NormalizeActors(canonical), nil
}

// canonicalHTTPActor accepts only the canonical durable IDs and the documented
// numeric HTTP aliases. Legacy runtime-named values are deliberately rejected.
func canonicalHTTPActor(value model.ActorID) model.ActorID {
	switch strings.ToLower(strings.TrimSpace(string(value))) {
	case "slot1", "1":
		return model.ActorSlot1
	case "slot2", "2":
		return model.ActorSlot2
	default:
		return model.ActorID(strings.ToLower(strings.TrimSpace(string(value))))
	}
}

func (e *Engine) canonicalAttachments(values []model.Attachment) ([]model.Attachment, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) > 8 {
		return nil, errors.New("a message can include at most 8 images")
	}
	if e.cfg.Attachments == nil {
		return nil, errors.New("image storage is unavailable")
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]model.Attachment, 0, len(values))
	var total int64
	for _, value := range values {
		id := strings.TrimSpace(value.ID)
		if id == "" {
			return nil, errors.New("image attachment id is required")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		resolved, _, err := e.cfg.Attachments.Resolve(id)
		if err != nil {
			return nil, fmt.Errorf("resolve image %q: %w", id, err)
		}
		if resolved.Kind != "image" || !strings.HasPrefix(strings.ToLower(resolved.MediaType), "image/") {
			return nil, fmt.Errorf("attachment %q is not a supported image", id)
		}
		total += resolved.Size
		if total > 20<<20 {
			return nil, errors.New("message images exceed the 20 MiB total limit")
		}
		seen[id] = struct{}{}
		out = append(out, resolved)
	}
	return out, nil
}

func mergeAttachments(groups ...[]model.Attachment) []model.Attachment {
	seen := make(map[string]struct{})
	var merged []model.Attachment
	for _, group := range groups {
		for _, attachment := range group {
			if attachment.ID == "" {
				continue
			}
			if _, ok := seen[attachment.ID]; ok {
				continue
			}
			seen[attachment.ID] = struct{}{}
			merged = append(merged, attachment)
		}
	}
	return merged
}

func (e *Engine) agentAttachments(values []model.Attachment) ([]model.AgentAttachment, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if e.cfg.Attachments == nil {
		return nil, errors.New("image storage is unavailable")
	}
	out := make([]model.AgentAttachment, 0, len(values))
	for _, value := range values {
		resolved, path, err := e.cfg.Attachments.Resolve(value.ID)
		if err != nil {
			return nil, fmt.Errorf("resolve image %q: %w", value.ID, err)
		}
		// Revalidating the current file against its current manifest is not
		// enough: queued/retried input must still reference the bytes accepted
		// in the durable Message, even if both stored files were replaced.
		if value.SHA256 != "" && !strings.EqualFold(value.SHA256, resolved.SHA256) {
			return nil, fmt.Errorf("image %q content changed since the message was accepted", value.ID)
		}
		out = append(out, model.AgentAttachment{Attachment: resolved, Path: path})
	}
	return out, nil
}

func (e *Engine) discoverAgentImages(actor model.ActorID, text string) []model.Attachment {
	if e.cfg.Attachments == nil {
		return nil
	}
	return e.cfg.Attachments.DiscoverRepoImages(text, string(actor)+"-artifact")
}

func (e *Engine) threadForReply(replyTo string) string {
	if replyTo == "" {
		return model.NewID("thread")
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if message, ok := e.findMessageLocked(replyTo); ok && message.ThreadID != "" {
		return message.ThreadID
	}
	return model.NewID("thread")
}

func (e *Engine) findMessageLocked(id string) (model.Message, bool) {
	if id == "" {
		return model.Message{}, false
	}
	for i := len(e.snapshot.Messages) - 1; i >= 0; i-- {
		if e.snapshot.Messages[i].ID == id {
			return e.snapshot.Messages[i], true
		}
	}
	return model.Message{}, false
}

func (e *Engine) latestHumanSeqForRelayLocked(incoming model.Message) uint64 {
	for i := len(e.snapshot.Messages) - 1; i >= 0; i-- {
		message := e.snapshot.Messages[i]
		if message.From != model.ActorUser {
			continue
		}
		if message.Seq <= incoming.Seq {
			return 0
		}
		return message.Seq
	}
	return 0
}

func retryableTarget(message model.Message, target model.ActorID) bool {
	processing := message.Processing[target]
	if processing == model.ProcessingFailed || processing == model.ProcessingCancelled {
		return true
	}
	delivery := message.Delivery[target]
	return delivery == model.DeliveryFailed || delivery == model.DeliverySkipped
}
