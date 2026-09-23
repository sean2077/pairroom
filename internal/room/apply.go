package room

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// Durable record/apply: every fact is appended before it is projected, and
// replay uses the same applyLocked transition rules.

func (e *Engine) record(kind string, actor model.ActorID, payload any) (model.Event, error) {
	e.mu.RLock()
	roomID := e.snapshot.Meta.ID
	e.mu.RUnlock()
	event, err := model.NewEvent(roomID, kind, actor, payload)
	if err != nil {
		return model.Event{}, err
	}
	e.mu.Lock()
	if err := e.cfg.Store.Append(&event); err != nil {
		e.markStoreFatalLocked(fmt.Errorf("room event log write failed: %w", err), roomID)
		e.mu.Unlock()
		return model.Event{}, err
	}
	if err := e.applyLocked(event); err != nil {
		e.markStoreFatalLocked(fmt.Errorf("room event projection failed: %w", err), roomID)
		e.mu.Unlock()
		return model.Event{}, err
	}
	e.mu.Unlock()
	e.cfg.Hub.Publish(event)
	return event, nil
}

// RecordServiceEvent keeps the active Engine as the only Room Event Log
// writer while the service control plane commits runtime-discovered facts.
func (e *Engine) RecordServiceEvent(kind string, payload any) error {
	if kind != eventServiceBindingMaterialized {
		return fmt.Errorf("unsupported live service event %q", kind)
	}
	_, err := e.record(kind, model.ActorSystem, payload)
	return err
}

func (e *Engine) apply(event model.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applyLocked(event)
}

// decodeCurrentEventData is deliberately stricter than ordinary projection
// decoding. Store schema 10 has no migration or legacy-field compatibility;
// accepting a v4/v8 payload with ignored Handoff, Hop, Workflow, or routing
// fields would silently create a mixed-version Room that cannot be audited.
func decodeCurrentEventData(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("event payload contains multiple JSON values")
		}
		return err
	}
	return nil
}

func (e *Engine) applyLocked(event model.Event) error {
	e.snapshot.LatestSeq = event.Seq
	switch event.Kind {
	case EventPermissionsUpdated:
		var update permissionsUpdate
		if err := json.Unmarshal(event.Data, &update); err != nil {
			return err
		}
		if e.snapshot.Meta.Collaboration == nil || !update.Actor.ValidParticipant() || !update.Profile.Valid() {
			return errors.New("invalid permissions event")
		}
		p := e.snapshot.Participants[update.Actor]
		p.PermissionProfile = update.Profile
		e.snapshot.Participants[update.Actor] = p
	case EventRoomCreated:
		if err := json.Unmarshal(event.Data, &e.snapshot.Meta); err != nil {
			return err
		}
		if e.snapshot.Meta.Collaboration == nil {
			return errors.New("Room has no collaboration instructions; legacy Rooms are unsupported, create a new Room")
		}
		if err := e.snapshot.Meta.Collaboration.Validate(); err != nil {
			return err
		}
	case eventServiceRoomRenamed:
		var update serviceRoomRenamedProjection
		if err := json.Unmarshal(event.Data, &update); err != nil {
			return err
		}
		update.Name = strings.TrimSpace(update.Name)
		if update.Name == "" {
			return errors.New("service Room rename has an empty name")
		}
		e.snapshot.Meta.Name = update.Name
	case eventServiceBindingMaterialized:
		var update serviceBindingMaterializedProjection
		if err := json.Unmarshal(event.Data, &update); err != nil {
			return err
		}
		binding := update.Binding
		if !binding.Agent.ValidParticipant() || binding.Pending || strings.TrimSpace(binding.SessionID) == "" {
			return errors.New("service binding materialization is invalid")
		}
		participant := e.snapshot.Participants[binding.Agent]
		participant.ID = binding.Agent
		identity := model.ParticipantIdentityFor(binding.Agent, e.runtimeKinds())
		participant.DisplayName = identity.DisplayName
		participant.MentionHandle = identity.MentionHandle
		participant.SessionID = strings.TrimSpace(binding.SessionID)
		e.snapshot.Participants[binding.Agent] = participant
	case EventSettingsUpdated:
		var settings model.RoomSettings
		if err := decodeCurrentEventData(event.Data, &settings); err != nil {
			return err
		}
		e.snapshot.Settings = settings
	case EventParticipantUpdated:
		var participant model.ParticipantSnapshot
		if err := decodeCurrentEventData(event.Data, &participant); err != nil {
			return err
		}
		if !participant.ID.ValidParticipant() || strings.TrimSpace(participant.MentionHandle) == "" {
			return errors.New("participant update is missing a valid slot or mention handle")
		}
		if e.snapshot.Participants == nil {
			e.snapshot.Participants = make(map[model.ActorID]model.ParticipantSnapshot)
		}
		e.snapshot.Participants[participant.ID] = participant
	case "participants.batch.updated", "service.room.bindings.completed", "service.legacy.imported":
		return fmt.Errorf("unsupported retired Room event %q", event.Kind)
	case EventMessageCreated:
		var message model.Message
		if err := decodeCurrentEventData(event.Data, &message); err != nil {
			return err
		}
		if message.Intent == "" {
			message.Intent = model.IntentSteer
		} else if !message.Intent.Valid() {
			return fmt.Errorf("message %q uses unsupported intent %q", message.ID, message.Intent)
		}
		message.Seq = event.Seq
		e.snapshot.Messages = append(e.snapshot.Messages, message)
	case EventDeliveryUpdated:
		var update model.DeliveryUpdate
		if err := decodeCurrentEventData(event.Data, &update); err != nil {
			return err
		}
		if !update.Target.ValidParticipant() || !update.State.Valid() {
			return fmt.Errorf("invalid delivery update for %s: %q", update.Target, update.State)
		}
		for i := range e.snapshot.Messages {
			if e.snapshot.Messages[i].ID != update.MessageID {
				continue
			}
			if e.snapshot.Messages[i].Delivery == nil {
				e.snapshot.Messages[i].Delivery = make(map[model.ActorID]model.DeliveryState)
			}
			if e.snapshot.Messages[i].DeliveryDetail == nil {
				e.snapshot.Messages[i].DeliveryDetail = make(map[model.ActorID]string)
			}
			current := e.snapshot.Messages[i].Delivery[update.Target]
			if !deliveryTransitionAllowed(current, update.State) {
				break
			}
			e.snapshot.Messages[i].Delivery[update.Target] = update.State
			e.snapshot.Messages[i].DeliveryDetail[update.Target] = update.Detail
			break
		}
	case EventProcessingUpdated:
		var update model.ProcessingUpdate
		if err := decodeCurrentEventData(event.Data, &update); err != nil {
			return err
		}
		if !update.Target.ValidParticipant() || !update.State.Valid() {
			return fmt.Errorf("invalid processing update for %s: %q", update.Target, update.State)
		}
		for i := range e.snapshot.Messages {
			if e.snapshot.Messages[i].ID != update.MessageID {
				continue
			}
			ensureMessageLifecycleMaps(&e.snapshot.Messages[i])
			current := e.snapshot.Messages[i].Processing[update.Target]
			if !processingTransitionAllowed(current, update.State) {
				break
			}
			e.snapshot.Messages[i].Processing[update.Target] = update.State
			e.snapshot.Messages[i].ProcessingDetail[update.Target] = update.Detail
			e.snapshot.Messages[i].ProcessingTurn[update.Target] = update.TurnID
			e.snapshot.Messages[i].ProcessingLastUpdatedAt[update.Target] = update.UpdatedAt
			break
		}
	case EventApprovalUpdated:
		var approval model.Approval
		if err := json.Unmarshal(event.Data, &approval); err != nil {
			return err
		}
		replaced := false
		for i := range e.snapshot.Approvals {
			if e.snapshot.Approvals[i].ID == approval.ID {
				e.snapshot.Approvals[i] = approval
				replaced = true
				break
			}
		}
		if !replaced {
			e.snapshot.Approvals = append(e.snapshot.Approvals, approval)
		}
	case EventTurnSummaryUpdated:
		var summary model.TurnSummary
		if err := json.Unmarshal(event.Data, &summary); err != nil {
			return err
		}
		if summary.ID == "" || !summary.Agent.ValidParticipant() || summary.TurnID == "" {
			return errors.New("invalid turn summary event")
		}
		replaceTurnSummaryLocked(&e.snapshot, summary)
	case "workflow.updated":
		// Workflow orchestration and its durable event kind were removed in
		// protocol v5. Rejecting the old kind explicitly prevents a mixed
		// schema-9 log from appearing valid merely because its payload is
		// otherwise well-formed.
		return errors.New("workflow events are unsupported in protocol v5")
	}

	e.snapshot.Events = append(e.snapshot.Events, event)
	e.recentEventDataBytes += len(event.Data)
	for len(e.snapshot.Events) > 1 && (len(e.snapshot.Events) > recentEventLimit || e.recentEventDataBytes > recentEventBytes) {
		// Advance the bounded window rather than copying 600 structs per event.
		// Clear the retired entry so its payload can be reclaimed even while the
		// current window still shares the old backing array.
		e.recentEventDataBytes -= len(e.snapshot.Events[0].Data)
		e.snapshot.Events[0] = model.Event{}
		e.snapshot.Events = e.snapshot.Events[1:]
	}
	return nil
}

func deliveryTransitionAllowed(current, next model.DeliveryState) bool {
	if next == "" {
		return false
	}
	if current == "" {
		return true
	}
	// Failure and explicit policy skips are terminal. This matters when a very
	// fast runtime emits an error before StartTurn returns its initial state.
	if current == model.DeliveryFailed || current == model.DeliverySkipped {
		return false
	}
	if next == model.DeliveryFailed || next == model.DeliverySkipped {
		return true
	}
	switch current {
	case model.DeliveryPending:
		return next == model.DeliveryQueued || next == model.DeliverySubmitting
	case model.DeliveryQueued:
		return next == model.DeliveryQueued || next == model.DeliverySubmitting
	case model.DeliverySubmitting:
		return next == model.DeliverySubmitting || next == model.DeliveryQueued || next == model.DeliveryStarted || next == model.DeliveryInjected
	default:
		// started/injected describe how the input entered the native harness;
		// don't let a late initial update rewrite that accepted boundary.
		return current == next
	}
}

func processingTransitionAllowed(current, next model.ProcessingState) bool {
	if next == "" {
		return false
	}
	if current == "" || current == model.ProcessingWaiting {
		return true
	}
	if current.Terminal() {
		return current == next
	}
	if next.Terminal() {
		return true
	}
	return current == next
}

func ensureMessageLifecycleMaps(message *model.Message) {
	if message.Delivery == nil {
		message.Delivery = make(map[model.ActorID]model.DeliveryState)
	}
	if message.DeliveryDetail == nil {
		message.DeliveryDetail = make(map[model.ActorID]string)
	}
	if message.Processing == nil {
		message.Processing = make(map[model.ActorID]model.ProcessingState)
	}
	if message.ProcessingDetail == nil {
		message.ProcessingDetail = make(map[model.ActorID]string)
	}
	if message.ProcessingTurn == nil {
		message.ProcessingTurn = make(map[model.ActorID]string)
	}
	if message.ProcessingLastUpdatedAt == nil {
		message.ProcessingLastUpdatedAt = make(map[model.ActorID]time.Time)
	}
}
