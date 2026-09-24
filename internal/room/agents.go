package room

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
)

// Participant lifecycle and permission projection for the two native slots.

func (e *Engine) StartAgent(ctx context.Context, actor model.ActorID) error {
	unlock, err := e.lockDelivery(ctx, actor)
	if err != nil {
		return err
	}
	defer unlock()
	return e.startAgentLocked(ctx, actor)
}

func (e *Engine) startAgentLocked(ctx context.Context, actor model.ActorID) error {
	adapter, err := e.adapter(actor)
	if err != nil {
		return err
	}
	e.updateParticipant(actor, func(p *model.ParticipantSnapshot) {
		p.State = model.StateStarting
		p.LastError = ""
		p.LastActivity = time.Now().UTC()
	})
	if err := adapter.Start(ctx); err != nil {
		e.updateParticipant(actor, func(p *model.ParticipantSnapshot) {
			p.State = model.StateError
			p.LastError = err.Error()
			p.LastActivity = time.Now().UTC()
		})
		return err
	}
	return nil
}

func (e *Engine) StopAgent(ctx context.Context, actor model.ActorID) error {
	unlock, err := e.lockDelivery(ctx, actor)
	if err != nil {
		return err
	}
	defer unlock()
	if err := e.stopAgentLocked(ctx, actor); err != nil {
		return err
	}
	e.finishTurn(actor)
	return nil
}

func (e *Engine) stopAgentLocked(ctx context.Context, actor model.ActorID) error {
	adapter, err := e.adapter(actor)
	if err != nil {
		return err
	}
	if err := adapter.Stop(ctx); err != nil {
		return err
	}
	_ = e.flushTurnSummaries(actor, time.Time{})
	e.cancelInFlight(actor, "native runtime was stopped")
	e.expireApprovals(actor, "runtime_stopped")
	e.updateParticipant(actor, func(p *model.ParticipantSnapshot) {
		p.State = model.StateStopped
		p.CurrentTurn = ""
		p.LastActivity = time.Now().UTC()
	})
	return nil
}

func (e *Engine) RestartAgent(ctx context.Context, actor model.ActorID) error {
	unlock, err := e.lockDelivery(ctx, actor)
	if err != nil {
		return err
	}
	defer unlock()
	if err := e.stopAgentLocked(ctx, actor); err != nil {
		return err
	}
	err = e.startAgentLocked(ctx, actor)
	e.finishTurn(actor)
	return err
}

func (e *Engine) cancelInFlight(actor model.ActorID, detail string) {
	type item struct {
		messageID string
		turnID    string
	}
	e.mu.RLock()
	var items []item
	for _, message := range e.snapshot.Messages {
		state := message.Processing[actor]
		if state != model.ProcessingWaiting && state != model.ProcessingWorking {
			continue
		}
		items = append(items, item{messageID: message.ID, turnID: message.ProcessingTurn[actor]})
	}
	e.mu.RUnlock()
	for _, pending := range items {
		e.processing(pending.messageID, actor, model.ProcessingCancelled, detail, pending.turnID)
	}
}

func (e *Engine) expireApprovals(actor model.ActorID, decision string) {
	e.mu.RLock()
	var approvals []model.Approval
	for _, approval := range e.snapshot.Approvals {
		if approval.Agent == actor && approval.Status == "pending" {
			approvals = append(approvals, approval)
		}
	}
	e.mu.RUnlock()
	for _, approval := range approvals {
		now := time.Now().UTC()
		approval.Status = "expired"
		approval.Decision = decision
		approval.ResolvedAt = &now
		_, _ = e.record(EventApprovalUpdated, model.ActorSystem, approval)
	}
}

func (e *Engine) Interrupt(ctx context.Context, actor model.ActorID) error {
	adapter, err := e.adapter(actor)
	if err != nil {
		return err
	}
	if err := adapter.Interrupt(ctx); err != nil {
		return err
	}
	e.expireApprovals(actor, "runtime_interrupted")
	return nil
}

func (e *Engine) ResolveApproval(ctx context.Context, approvalID string, resolution model.ApprovalResolution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	var current *model.Approval
	for i := range e.snapshot.Approvals {
		if e.snapshot.Approvals[i].ID == approvalID {
			copy := e.snapshot.Approvals[i]
			current = &copy
			break
		}
	}
	if current == nil || current.Status != "pending" || e.approvalSubmitting[approvalID] {
		e.mu.Unlock()
		if current == nil {
			return fmt.Errorf("unknown approval %q", approvalID)
		}
		if current.Status != "pending" {
			return fmt.Errorf("approval %q is already %s", approvalID, current.Status)
		}
		return fmt.Errorf("approval %q is already being submitted", approvalID)
	}
	if e.approvalSubmitting == nil {
		e.approvalSubmitting = make(map[string]bool)
	}
	e.approvalSubmitting[approvalID] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.approvalSubmitting, approvalID)
		e.mu.Unlock()
	}()
	adapter, err := e.adapter(current.Agent)
	if err != nil {
		return err
	}
	if err := adapter.ResolveApproval(ctx, approvalID, resolution); err != nil {
		return err
	}
	now := time.Now().UTC()
	current.Status = "resolved"
	current.Decision = resolution.Decision
	current.ResolvedAt = &now
	_, err = e.record(EventApprovalUpdated, model.ActorUser, *current)
	return err
}

func (e *Engine) UpdateSettings(settings model.RoomSettings) error {
	if settings.StallWarningSeconds == 0 {
		settings.StallWarningSeconds = model.DefaultRoomSettings().StallWarningSeconds
	}
	if settings.StallWarningSeconds < -1 || settings.StallWarningSeconds > 86400 || (settings.StallWarningSeconds > 0 && settings.StallWarningSeconds < 30) {
		return errors.New("stall_warning_seconds must be -1 (disabled) or between 30 and 86400")
	}
	_, err := e.record(EventSettingsUpdated, model.ActorUser, settings)
	return err
}

func permissionChangeSafe(participant model.ParticipantSnapshot) error {
	switch participant.State {
	case model.StateStopped, model.StateIdle, model.StateError:
		return nil
	default:
		return fmt.Errorf("interrupt or stop %s before changing permissions", participant.DisplayName)
	}
}

func (e *Engine) runtimeKinds() map[model.ActorID]model.RuntimeKind {
	return runtimeKindsForConfig(e.cfg)
}

func runtimeKindsForConfig(cfg Config) map[model.ActorID]model.RuntimeKind {
	return map[model.ActorID]model.RuntimeKind{
		model.ActorSlot1: cfg.Slot1Config.Runtime.CanonicalForSlot(model.ActorSlot1),
		model.ActorSlot2: cfg.Slot2Config.Runtime.CanonicalForSlot(model.ActorSlot2),
	}
}

func (e *Engine) participantName(actor model.ActorID) string {
	return model.ParticipantIdentityFor(actor, e.runtimeKinds()).DisplayName
}

func slotAgentConfig(cfg Config, actor model.ActorID) agent.Config {
	if actor == model.ActorSlot2 {
		return cfg.Slot2Config
	}
	return cfg.Slot1Config
}

func applyPermissionRuntimeProjection(participant *model.ParticipantSnapshot, actor model.ActorID, cfg Config) {
	slot := slotAgentConfig(cfg, actor)
	slot.Actor = actor
	slot = agent.PermissionConfig(slot, participant.PermissionProfile)
	readOnly := nativeAccess(*participant) == model.NativeAccessReadOnly
	kind := slot.Runtime.CanonicalForSlot(actor)
	identity := model.ParticipantIdentityFor(actor, runtimeKindsForConfig(cfg))
	participant.RuntimeKind = kind
	participant.DisplayName = identity.DisplayName
	participant.MentionHandle = identity.MentionHandle
	// Rebuild the policy projection from the immutable slot selection on every
	// permission transition. Never retain policy
	// fields from the previous process in the public snapshot.
	participant.Runtime.PermissionMode = ""
	participant.Runtime.ApprovalPolicy = ""
	participant.Runtime.Sandbox = ""
	switch kind {
	case model.RuntimeClaude:
		if readOnly {
			participant.Runtime.PermissionMode = "plan"
		} else {
			participant.Runtime.PermissionMode = slot.PermissionMode
		}
	case model.RuntimeCodex:
		if readOnly {
			participant.Runtime.Sandbox = "readOnly"
			participant.Runtime.ApprovalPolicy = slot.ApprovalPolicy
		} else {
			participant.Runtime.ApprovalPolicy = slot.ApprovalPolicy
			participant.Runtime.Sandbox = slot.Sandbox
		}
	case model.RuntimeGrok:
		if readOnly {
			participant.Runtime.PermissionMode = "plan"
			participant.Runtime.Sandbox = "read-only"
		} else {
			participant.Runtime.PermissionMode = slot.PermissionMode
			participant.Runtime.Sandbox = slot.Sandbox
		}
	}
}

func (e *Engine) adapter(actor model.ActorID) (agent.Adapter, error) {
	if !actor.ValidParticipant() {
		return nil, errors.New("invalid participant")
	}
	e.mu.RLock()
	adapter := e.adapters[actor]
	started := e.started
	e.mu.RUnlock()
	if !started {
		return nil, errors.New("room engine has not started")
	}
	if adapter == nil {
		return nil, errors.New("participant adapter is unavailable")
	}
	return adapter, nil
}

func (e *Engine) runtimeContext(requestCtx context.Context) context.Context {
	e.mu.RLock()
	engineCtx := e.ctx
	e.mu.RUnlock()
	if engineCtx == nil {
		return requestCtx
	}
	return engineCtx
}

func (e *Engine) updateParticipant(actor model.ActorID, mutate func(*model.ParticipantSnapshot)) {
	if !actor.ValidParticipant() {
		return
	}
	_ = e.mutateParticipant(actor, actor, mutate)
}

// mutateParticipant serializes read-modify-write projections so concurrent
// runtime events cannot overwrite a freshly persisted session ID or state.
func (e *Engine) mutateParticipant(eventActor, participantID model.ActorID, mutate func(*model.ParticipantSnapshot)) error {
	e.mu.Lock()
	participant := e.snapshot.Participants[participantID]
	if participant.ID == "" {
		participant.ID = participantID
		identity := model.ParticipantIdentityFor(participantID, e.runtimeKinds())
		participant.DisplayName = identity.DisplayName
		participant.MentionHandle = identity.MentionHandle
		participant.Role = model.RolePeer
	}
	mutate(&participant)
	event, err := model.NewEvent(e.snapshot.Meta.ID, EventParticipantUpdated, eventActor, participant)
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
	if err != nil {
		return err
	}
	e.cfg.Hub.Publish(event)
	return nil
}

func (e *Engine) actorHasPendingApprovalLocked(actor model.ActorID) bool {
	for _, approval := range e.snapshot.Approvals {
		if approval.Agent == actor && approval.Status == "pending" {
			return true
		}
	}
	return false
}
