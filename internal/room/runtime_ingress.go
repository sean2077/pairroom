package room

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

// Runtime ingress: the single entry for adapter events, turn boundaries,
// processing projection and stall notices.

// HandleRuntimeEvent is the single ingress from both vendor adapters. It
// records the canonical event before projecting state and chat messages.
func (e *Engine) HandleRuntimeEvent(runtimeEvent model.RuntimeEvent) {
	if runtimeEvent.CreatedAt.IsZero() {
		runtimeEvent.CreatedAt = time.Now().UTC()
	}
	if runtimeEvent.Agent.ValidParticipant() {
		e.mu.Lock()
		e.lastRuntimeActivity[runtimeEvent.Agent] = runtimeEvent.CreatedAt
		delete(e.stallWarnedTurn, runtimeEvent.Agent)
		e.mu.Unlock()
	}
	var sourceSeq uint64
	if isTransientRuntimeKind(runtimeEvent.Kind) {
		e.publishTransientRuntime(runtimeEvent)
	} else if recorded, err := e.record(EventRuntime, runtimeEvent.Agent, runtimeEvent); err == nil {
		sourceSeq = recorded.Seq
	}
	e.projectTurnSummary(runtimeEvent, sourceSeq)

	switch runtimeEvent.Kind {
	case model.RuntimeSession:
		e.updateParticipant(runtimeEvent.Agent, func(p *model.ParticipantSnapshot) {
			if runtimeEvent.SessionID != "" {
				p.SessionID = runtimeEvent.SessionID
			}
			p.LastActivity = runtimeEvent.CreatedAt
		})
	case model.RuntimeInfoUpdated:
		var info model.RuntimeInfo
		if runtimeEvent.Runtime != nil {
			info = *runtimeEvent.Runtime
		} else if len(runtimeEvent.Data) > 0 {
			_ = json.Unmarshal(runtimeEvent.Data, &info)
		}
		e.updateParticipant(runtimeEvent.Agent, func(p *model.ParticipantSnapshot) {
			p.Runtime = info
			if info.Model != "" {
				p.Model = info.Model
			}
			applyPermissionRuntimeProjection(p, runtimeEvent.Agent, e.cfg)
			p.LastActivity = runtimeEvent.CreatedAt
		})
	case model.RuntimeInputProcessing:
		if runtimeEvent.CorrelationID != "" && e.runtimeTurnMatches(runtimeEvent) {
			state := model.ProcessingWorking
			if runtimeEvent.Name == string(model.ProcessingWaiting) {
				state = model.ProcessingWaiting
			}
			e.processing(runtimeEvent.CorrelationID, runtimeEvent.Agent, state, runtimeEvent.Text, runtimeEvent.TurnID)
		}
	case model.RuntimeInputCompleted:
		matches := e.runtimeTurnMatches(runtimeEvent)
		if runtimeEvent.CorrelationID != "" && matches {
			e.processing(runtimeEvent.CorrelationID, runtimeEvent.Agent, model.ProcessingCompleted, runtimeEvent.Text, runtimeEvent.TurnID)
		}
		if matches {
			e.finishTurnIfIdle(runtimeEvent.Agent, false)
		}
	case model.RuntimeInputCancelled:
		matches := e.runtimeTurnMatches(runtimeEvent)
		if runtimeEvent.CorrelationID != "" && matches {
			e.processing(runtimeEvent.CorrelationID, runtimeEvent.Agent, model.ProcessingCancelled, runtimeEvent.Text, runtimeEvent.TurnID)
		}
		if matches {
			e.finishTurnIfIdle(runtimeEvent.Agent, false)
		}
	case model.RuntimeInputFailed:
		matches := e.runtimeTurnMatches(runtimeEvent)
		if runtimeEvent.CorrelationID != "" && matches {
			e.processing(runtimeEvent.CorrelationID, runtimeEvent.Agent, model.ProcessingFailed, runtimeEvent.Text, runtimeEvent.TurnID)
		}
		if matches {
			e.finishTurnIfIdle(runtimeEvent.Agent, false)
		}
	case model.RuntimeState:
		e.updateParticipant(runtimeEvent.Agent, func(p *model.ParticipantSnapshot) {
			if runtimeEvent.State != "" {
				p.State = runtimeEvent.State
			}
			if runtimeEvent.State == model.StateError {
				p.LastError = runtimeEvent.Text
			} else if runtimeEvent.State != "" {
				p.LastError = ""
			}
			p.LastActivity = runtimeEvent.CreatedAt
		})
	case model.RuntimeTurnStarted:
		if !e.runtimeTurnMatches(runtimeEvent) {
			return
		}
		e.turnMu.Lock()
		if e.turnOwner == runtimeEvent.Agent {
			e.turnBoundarySeen = false
		}
		e.turnMu.Unlock()
		if runtimeEvent.CorrelationID != "" {
			e.processing(runtimeEvent.CorrelationID, runtimeEvent.Agent, model.ProcessingWorking, "native turn started", runtimeEvent.TurnID)
		}
		e.updateParticipant(runtimeEvent.Agent, func(p *model.ParticipantSnapshot) {
			p.State = model.StateWorking
			p.CurrentTurn = runtimeEvent.TurnID
			p.LastActivity = runtimeEvent.CreatedAt
		})
	case model.RuntimeTurnCompleted:
		boundary := e.runtimeTurnMatches(runtimeEvent)
		if !boundary {
			return
		}
		e.settleTurnInputs(runtimeEvent)
		if boundary {
			e.updateParticipant(runtimeEvent.Agent, func(p *model.ParticipantSnapshot) {
				// A native Turn boundary ends any connection-local wait owned by that
				// Turn. Failed runtimes may immediately project StateError afterwards,
				// but a completed Turn must never leave the participant stuck Waiting.
				p.State = model.StateIdle
				if p.CurrentTurn == runtimeEvent.TurnID || runtimeEvent.TurnID == "" {
					p.CurrentTurn = ""
				}
				p.LastActivity = runtimeEvent.CreatedAt
			})
			e.expireApprovals(runtimeEvent.Agent, "turn_completed")
			e.turnMu.Lock()
			if e.turnOwner == runtimeEvent.Agent {
				e.turnBoundarySeen = true
			}
			e.turnMu.Unlock()
			e.finishTurnIfIdle(runtimeEvent.Agent, false)
		}
	case model.RuntimeApprovalRequested:
		if runtimeEvent.Approval != nil {
			_, _ = e.record(EventApprovalUpdated, runtimeEvent.Agent, *runtimeEvent.Approval)
			e.updateParticipant(runtimeEvent.Agent, func(p *model.ParticipantSnapshot) {
				p.State = model.StateWaiting
				p.LastActivity = runtimeEvent.CreatedAt
			})
		}
	case model.RuntimeApprovalResolved:
		if runtimeEvent.Approval != nil {
			_, _ = e.record(EventApprovalUpdated, runtimeEvent.Agent, *runtimeEvent.Approval)
		}
	case model.RuntimeError:
		// RuntimeError is diagnostic, not a native Turn boundary. Codex may emit
		// an `error` notification while the Turn continues, and stream/protocol
		// diagnostics can also precede a later authoritative turn.completed. Only
		// RuntimeInputFailed/Cancelled settle individual inputs; only a reliable
		// RuntimeTurnCompleted (including confirmed process exit), explicit stop,
		// or failed submission releases the Room owner.
		e.updateParticipant(runtimeEvent.Agent, func(p *model.ParticipantSnapshot) {
			p.LastError = runtimeEvent.Text
			if p.CurrentTurn == "" && p.State != model.StateStarting && p.State != model.StateWorking && p.State != model.StateWaiting {
				p.State = model.StateError
			}
			p.LastActivity = runtimeEvent.CreatedAt
		})
	case model.RuntimeFinal:
		if !e.runtimeTurnMatches(runtimeEvent) {
			return
		}
		e.onFinal(runtimeEvent)
	}
}

// runtimeTurnMatches rejects a late completion from an older native turn.
// Vendor streams can deliver notifications after a replacement turn has
// already started; treating that stale notification as the current boundary
// would release the Room owner and let FIFO work overtake the active turn.
// Test/fallback adapters may omit turn correlation entirely, so an event with
// no TurnID remains accepted and a correlated message without a recorded turn
// remains permissive.
func (e *Engine) runtimeTurnMatches(runtimeEvent model.RuntimeEvent) bool {
	if !runtimeEvent.Agent.ValidParticipant() || strings.TrimSpace(runtimeEvent.TurnID) == "" {
		return true
	}
	e.mu.RLock()
	participant := e.snapshot.Participants[runtimeEvent.Agent]
	if participant.CurrentTurn != "" && participant.CurrentTurn != runtimeEvent.TurnID {
		e.mu.RUnlock()
		return false
	}
	if runtimeEvent.CorrelationID != "" {
		if message, found := e.findMessageLocked(runtimeEvent.CorrelationID); found {
			if recorded := strings.TrimSpace(message.ProcessingTurn[runtimeEvent.Agent]); recorded != "" {
				match := recorded == runtimeEvent.TurnID
				e.mu.RUnlock()
				return match
			}
		}
	}
	turns := make(map[string]struct{})
	for _, message := range e.snapshot.Messages {
		state := message.Processing[runtimeEvent.Agent]
		if state != model.ProcessingWaiting && state != model.ProcessingWorking {
			continue
		}
		if turnID := strings.TrimSpace(message.ProcessingTurn[runtimeEvent.Agent]); turnID != "" {
			turns[turnID] = struct{}{}
		}
	}
	e.mu.RUnlock()
	if len(turns) == 0 {
		return true
	}
	if len(turns) != 1 {
		return false
	}
	_, ok := turns[runtimeEvent.TurnID]
	return ok
}

// settleTurnInputs supplies a conservative terminal fallback for adapters that
// report a native Turn boundary but omit per-input terminal events. Inputs
// explicitly queued for a later native Turn are left waiting until that Turn
// starts and completes.
func (e *Engine) settleTurnInputs(runtimeEvent model.RuntimeEvent) {
	if !runtimeEvent.Agent.ValidParticipant() {
		return
	}
	state := model.ProcessingCompleted
	detail := "native turn completed"
	status := strings.ToLower(strings.TrimSpace(runtimeEvent.Name))
	switch {
	case strings.Contains(status, "cancel"), strings.Contains(status, "interrupt"):
		state = model.ProcessingCancelled
		detail = "native turn was cancelled"
	case strings.Contains(status, "fail"), strings.Contains(status, "error"), strings.Contains(status, "exit"):
		state = model.ProcessingFailed
		detail = "native turn failed"
	}

	e.mu.RLock()
	messageIDs := make([]string, 0, 2)
	fallbackIDs := make([]string, 0, 2)
	activeTurnIDs := make(map[string]struct{})
	for _, message := range e.snapshot.Messages {
		current := message.Processing[runtimeEvent.Agent]
		if current != model.ProcessingWaiting && current != model.ProcessingWorking {
			continue
		}
		turnID := message.ProcessingTurn[runtimeEvent.Agent]
		if turnID != "" {
			activeTurnIDs[turnID] = struct{}{}
		}
		if message.ID == runtimeEvent.CorrelationID ||
			(runtimeEvent.TurnID != "" && turnID == runtimeEvent.TurnID) {
			messageIDs = append(messageIDs, message.ID)
			continue
		}
		// Some lightweight adapters provide a turn ID on the boundary but do not
		// put that ID in their earlier processing projection. Preserve the
		// conservative one-message fallback for that case. If another recorded
		// turn exists, however, a mismatching completion is stale and must not
		// settle the current input.
		fallbackAllowed := runtimeEvent.CorrelationID == "" &&
			(runtimeEvent.TurnID == "" || len(activeTurnIDs) == 0)
		if fallbackAllowed {
			switch message.Delivery[runtimeEvent.Agent] {
			case model.DeliveryStarted, model.DeliveryInjected:
				fallbackIDs = append(fallbackIDs, message.ID)
			}
		}
	}
	e.mu.RUnlock()
	if len(messageIDs) == 0 && len(fallbackIDs) == 1 {
		messageIDs = fallbackIDs
	}
	for _, messageID := range messageIDs {
		e.processing(messageID, runtimeEvent.Agent, state, detail, runtimeEvent.TurnID)
	}
}

// High-volume display telemetry is useful while a turn is running but should
// never stall a vendor stdout reader on per-token disk sync. Durable state and
// audit events still go through record() before publication. Sequence zero marks
// an intentionally ephemeral SSE event; reconnects resume from durable events.
func (e *Engine) publishTransientRuntime(runtimeEvent model.RuntimeEvent) {
	e.mu.RLock()
	roomID := e.snapshot.Meta.ID
	e.mu.RUnlock()
	event, err := model.NewEvent(roomID, EventRuntime, runtimeEvent.Agent, runtimeEvent)
	if err != nil {
		return
	}
	event.Seq = 0
	e.cfg.Hub.Publish(event)
}

func isTransientRuntimeKind(kind string) bool {
	switch kind {
	case model.RuntimeTextDelta, model.RuntimeCommandOutput, model.RuntimeDiffUpdated, model.RuntimeUsageUpdated:
		return true
	default:
		return false
	}
}

func (e *Engine) onFinal(runtimeEvent model.RuntimeEvent) {
	text := runtimeEvent.Text
	if strings.TrimSpace(text) == "" {
		return
	}
	e.routingMu.Lock()
	defer e.routingMu.Unlock()

	e.mu.RLock()
	if runtimeEvent.CorrelationID != "" && runtimeEvent.TurnID != "" && e.finalAlreadyProjectedLocked(runtimeEvent) {
		e.mu.RUnlock()
		return
	}
	incoming, found := e.findMessageLocked(runtimeEvent.CorrelationID)
	latestHumanSeq := uint64(0)
	if found {
		latestHumanSeq = e.latestHumanSeqForRelayLocked(incoming)
	}
	e.mu.RUnlock()
	if !found {
		incoming = model.Message{ID: runtimeEvent.CorrelationID, ThreadID: model.NewID("thread")}
	}

	targets := e.agentTargets(runtimeEvent.Agent, text, incoming.Seq, latestHumanSeq)
	to := []model.ActorID{model.ActorUser}
	to = append(to, targets...)
	message := model.Message{
		ID: model.NewID("msg"), From: runtimeEvent.Agent, To: to,
		Text: text, ReplyTo: incoming.ID, Intent: model.IntentQueue,
		ThreadID: incoming.ThreadID, TurnID: runtimeEvent.TurnID,
		CreatedAt:               time.Now().UTC(),
		Delivery:                make(map[model.ActorID]model.DeliveryState, len(targets)),
		DeliveryDetail:          make(map[model.ActorID]string, len(targets)),
		Processing:              make(map[model.ActorID]model.ProcessingState, len(targets)),
		ProcessingDetail:        make(map[model.ActorID]string, len(targets)),
		ProcessingTurn:          make(map[model.ActorID]string, len(targets)),
		ProcessingLastUpdatedAt: make(map[model.ActorID]time.Time, len(targets)),
		Attachments:             mergeAttachments(incoming.Attachments, e.discoverAgentImages(runtimeEvent.Agent, text)),
	}
	if message.ThreadID == "" {
		message.ThreadID = model.NewID("thread")
	}
	for _, target := range targets {
		message.Delivery[target] = model.DeliveryPending
		message.Processing[target] = model.ProcessingWaiting
		message.ProcessingLastUpdatedAt[target] = message.CreatedAt
	}
	event, err := e.record(EventMessageCreated, runtimeEvent.Agent, message)
	if err != nil {
		return
	}
	message.Seq = event.Seq
	for _, target := range targets {
		e.scheduleDelivery(e.runtimeContext(context.Background()), message, target)
	}
}

func (e *Engine) finalAlreadyProjectedLocked(runtimeEvent model.RuntimeEvent) bool {
	for _, message := range e.snapshot.Messages {
		if message.From == runtimeEvent.Agent && message.ReplyTo == runtimeEvent.CorrelationID && message.TurnID == runtimeEvent.TurnID {
			return true
		}
	}
	return false
}

// processingFallback gives adapters that only return a DeliveryState a minimal
// processing lifecycle without overwriting richer runtime events emitted during
// native submission. Native adapters can report a turn ID and vendor-specific
// detail before StartTurn returns; those events remain authoritative.
func (e *Engine) processingFallback(messageID string, target model.ActorID, state model.ProcessingState, detail string) {
	if messageID == "" || !target.ValidParticipant() {
		return
	}
	update := model.ProcessingUpdate{
		MessageID: messageID, Target: target, State: state, Detail: detail, UpdatedAt: time.Now().UTC(),
	}

	e.mu.Lock()
	found := false
	for i := range e.snapshot.Messages {
		message := &e.snapshot.Messages[i]
		if message.ID != messageID {
			continue
		}
		ensureMessageLifecycleMaps(message)
		current := message.Processing[target]
		switch state {
		case model.ProcessingWorking:
			// A native working/terminal event already carries better correlation.
			if current != "" && current != model.ProcessingWaiting {
				e.mu.Unlock()
				return
			}
		case model.ProcessingWaiting:
			// Preserve native queue diagnostics and any turn correlation.
			if current != "" && current != model.ProcessingWaiting {
				e.mu.Unlock()
				return
			}
			if message.ProcessingDetail[target] != "" || message.ProcessingTurn[target] != "" {
				e.mu.Unlock()
				return
			}
		}
		found = true
		break
	}
	if !found {
		e.mu.Unlock()
		return
	}
	event, err := model.NewEvent(e.snapshot.Meta.ID, EventProcessingUpdated, target, update)
	if err == nil {
		if appendErr := e.cfg.Store.Append(&event); appendErr != nil {
			err = appendErr
			e.markStoreFatalLocked(fmt.Errorf("room event log write failed: %w", appendErr), e.snapshot.Meta.ID)
		}
	}
	if err == nil {
		if applyErr := e.applyLocked(event); applyErr != nil {
			err = applyErr
			e.markStoreFatalLocked(fmt.Errorf("room event projection failed: %w", applyErr), e.snapshot.Meta.ID)
		}
	}
	e.mu.Unlock()
	if err == nil {
		e.cfg.Hub.Publish(event)
	}
}

func (e *Engine) processing(messageID string, target model.ActorID, state model.ProcessingState, detail, turnID string) {
	if messageID == "" || !target.ValidParticipant() {
		return
	}
	update := model.ProcessingUpdate{
		MessageID: messageID,
		Target:    target,
		State:     state,
		Detail:    detail,
		TurnID:    turnID,
		UpdatedAt: time.Now().UTC(),
	}

	e.mu.Lock()
	current := model.ProcessingState("")
	found := false
	for i := range e.snapshot.Messages {
		if e.snapshot.Messages[i].ID != messageID {
			continue
		}
		current = e.snapshot.Messages[i].Processing[target]
		found = true
		break
	}
	if !found || !processingTransitionAllowed(current, state) {
		e.mu.Unlock()
		return
	}
	event, err := model.NewEvent(e.snapshot.Meta.ID, EventProcessingUpdated, target, update)
	if err == nil {
		if appendErr := e.cfg.Store.Append(&event); appendErr != nil {
			err = appendErr
			e.markStoreFatalLocked(fmt.Errorf("room event log write failed: %w", appendErr), e.snapshot.Meta.ID)
		}
	}
	if err == nil {
		if applyErr := e.applyLocked(event); applyErr != nil {
			err = applyErr
			e.markStoreFatalLocked(fmt.Errorf("room event projection failed: %w", applyErr), e.snapshot.Meta.ID)
		}
	}
	e.mu.Unlock()
	if err == nil {
		e.cfg.Hub.Publish(event)
	}
}

func (e *Engine) agentTargets(actor model.ActorID, text string, sourceSeq, latestHumanSeq uint64) []model.ActorID {
	mentions := prompt.ParseMentions(text, actor, e.runtimeKinds())
	// A newer human instruction takes precedence over an older Agent result, including an
	// explicit peer address in that stale result.
	if sourceSeq > 0 && latestHumanSeq > sourceSeq {
		e.notice("info", fmt.Sprintf("A newer user message cancelled %s's pending Agent relay; the response remains visible in the Room.", e.participantName(actor)))
		return nil
	}
	if len(mentions.Ambiguous) > 0 {
		identities := model.ParticipantIdentities(e.runtimeKinds())
		e.notice("warning", fmt.Sprintf("%s used ambiguous handle %s; use %s or %s. No Agent relay was started.", e.participantName(actor), strings.Join(mentions.Ambiguous, ", "), identities[model.ActorSlot1].MentionHandle, identities[model.ActorSlot2].MentionHandle))
		return nil
	}
	return mentions.Targets
}

func (e *Engine) delivery(messageID string, target model.ActorID, state model.DeliveryState, detail string) bool {
	return e.deliveryIf(messageID, target, state, detail, func(current model.DeliveryState) bool {
		return deliveryTransitionAllowed(current, state)
	})
}

func (e *Engine) deliveryIf(messageID string, target model.ActorID, state model.DeliveryState, detail string, allowed func(model.DeliveryState) bool) bool {
	update := model.DeliveryUpdate{
		MessageID: messageID,
		Target:    target,
		State:     state,
		Detail:    detail,
	}

	// Validate and persist a delivery transition under the same room lock. This
	// prevents a fast runtime error from being followed by a late StartTurn return
	// that would otherwise publish a misleading started/injected/queued event.
	e.mu.Lock()
	current := model.DeliveryState("")
	found := false
	for i := range e.snapshot.Messages {
		if e.snapshot.Messages[i].ID != messageID {
			continue
		}
		current = e.snapshot.Messages[i].Delivery[target]
		found = true
		break
	}
	if !found || allowed == nil || !allowed(current) {
		e.mu.Unlock()
		return false
	}
	event, err := model.NewEvent(e.snapshot.Meta.ID, EventDeliveryUpdated, target, update)
	if err == nil {
		if appendErr := e.cfg.Store.Append(&event); appendErr != nil {
			err = appendErr
			e.markStoreFatalLocked(fmt.Errorf("room event log write failed: %w", appendErr), e.snapshot.Meta.ID)
		}
	}
	if err == nil {
		if applyErr := e.applyLocked(event); applyErr != nil {
			err = applyErr
			e.markStoreFatalLocked(fmt.Errorf("room event projection failed: %w", applyErr), e.snapshot.Meta.ID)
		}
	}
	e.mu.Unlock()
	if err == nil {
		e.cfg.Hub.Publish(event)
	}
	return err == nil
}

func (e *Engine) monitorStalledTurns() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-ticker.C:
			// A quiet Turn still reaches its checkpoint without a new event.
			_ = e.flushTurnSummaries("", now.UTC())
			type warning struct {
				actor model.ActorID
				turn  string
				age   time.Duration
			}
			var warnings []warning
			e.mu.Lock()
			seconds := e.snapshot.Settings.StallWarningSeconds
			if seconds <= 0 {
				e.mu.Unlock()
				continue
			}
			threshold := time.Duration(seconds) * time.Second
			for _, actor := range []model.ActorID{model.ActorSlot1, model.ActorSlot2} {
				participant := e.snapshot.Participants[actor]
				if participant.State != model.StateWorking && participant.State != model.StateWaiting {
					continue
				}
				if participant.State == model.StateWaiting && e.actorHasPendingApprovalLocked(actor) {
					continue
				}
				last := e.lastRuntimeActivity[actor]
				if last.IsZero() || now.Sub(last) < threshold {
					continue
				}
				key := participant.CurrentTurn
				if key == "" {
					key = string(participant.State)
				}
				if e.stallWarnedTurn[actor] == key {
					continue
				}
				e.stallWarnedTurn[actor] = key
				warnings = append(warnings, warning{actor: actor, turn: participant.CurrentTurn, age: now.Sub(last)})
			}
			e.mu.Unlock()
			for _, item := range warnings {
				detail := fmt.Sprintf("%s has produced no runtime event for %s", e.participantName(item.actor), item.age.Round(time.Second))
				if item.turn != "" {
					detail += " during turn " + item.turn
				}
				e.notice("warning", detail+". Silence alone is not treated as a stall. Check Inspector for a long command; if there is no visible work or approval, steer, interrupt, or retry the turn.")
			}
		}
	}
}
