package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/bus"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

const (
	EventRoomCreated        = "room.created"
	EventSettingsUpdated    = "room.settings.updated"
	EventParticipantUpdated = "participant.updated"
	EventMessageCreated     = "message.created"
	EventDeliveryUpdated    = "message.delivery.updated"
	EventProcessingUpdated  = "message.processing.updated"
	EventRuntime            = "runtime.event"
	EventApprovalUpdated    = "approval.updated"
	EventSystemNotice       = "system.notice"
	EventTurnSummaryUpdated = "turn.summary.updated"

	eventServiceRoomRenamed         = "service.room.renamed"
	eventServiceBindingMaterialized = "service.room.binding.materialized"
	recentEventLimit                = 600
	// recentEventBytes bounds the in-memory replay/snapshot tail by payload
	// size as well as count; a cursor older than the tail receives a reset.
	recentEventBytes = 4 << 20
)

type Config struct {
	Collaboration             *model.Collaboration
	RequireCollaborationMatch bool
	Name                      string
	Repo                      string
	Settings                  model.RoomSettings
	Store                     *store.JSONLStore
	Hub                       *bus.Hub
	Slot1Factory              agent.Factory
	Slot2Factory              agent.Factory
	Slot1Config               agent.Config
	Slot2Config               agent.Config
	Attachments               AttachmentStore
	AutoStart                 bool
	OnSessionMaterialized     func(context.Context, model.ActorID, string) error
}

// AttachmentStore keeps presentation metadata durable while resolving an
// opaque attachment ID to a local path only at the native-agent boundary.
type AttachmentStore interface {
	Resolve(id string) (model.Attachment, string, error)
	DiscoverRepoImages(text, source string) []model.Attachment
	Remove(id string) error
}

type serviceRoomRenamedProjection struct {
	Name string `json:"name"`
}

type serviceBindingProjection struct {
	Agent     model.ActorID `json:"agent"`
	SessionID string        `json:"session_id"`
	Pending   bool          `json:"pending"`
}

type serviceBindingMaterializedProjection struct {
	Binding serviceBindingProjection `json:"binding"`
}

type SendRequest struct {
	Text        string              `json:"text"`
	To          []model.ActorID     `json:"to,omitempty"`
	ReplyTo     string              `json:"reply_to,omitempty"`
	Attachments []model.Attachment  `json:"attachments,omitempty"`
	Intent      model.MessageIntent `json:"intent,omitempty"`
}

type RetryRequest struct {
	To []model.ActorID `json:"to,omitempty"`
}

type CancelRequest struct {
	Target model.ActorID `json:"target"`
}

type scheduledDelivery struct {
	message model.Message
	target  model.ActorID
	steer   bool
	// forceQueue is used while restoring Room-owned FIFO entries. Their
	// original intent may be `steer`, but after restart there is no live native
	// Turn to steer, so recovery must preserve Event Log order.
	forceQueue bool
}

type Engine struct {
	// lifecycleMu serializes permission-driven process replacement with Close.
	lifecycleMu sync.Mutex
	mu          sync.RWMutex
	routingMu   sync.Mutex
	turnMu      sync.Mutex
	// summaryMu serializes Turn summary projection and checkpoint flushes.
	// It is acquired before mu, never while holding mu.
	summaryMu     sync.Mutex
	turnSummaries turnSummaryTracking
	// recentEventDataBytes is the payload total of snapshot.Events. Guarded by mu.
	recentEventDataBytes int

	cfg      Config
	snapshot model.RoomSnapshot
	adapters map[model.ActorID]agent.Adapter
	ctx      context.Context
	cancel   context.CancelFunc
	started  bool
	closed   bool
	// storeFatal records the first durable-store failure. The Event Log writer
	// closes itself on any append/sync error; from that point the Room must not
	// pretend mutations are recorded. Guarded by mu.
	storeFatal error

	lastRuntimeActivity map[model.ActorID]time.Time
	stallWarnedTurn     map[model.ActorID]string
	approvalSubmitting  map[string]bool
	deliveryMu          map[model.ActorID]chan struct{}
	turnOwner           model.ActorID
	turnQueue           []scheduledDelivery
	turnSubmitting      int
	turnBoundarySeen    bool
	restoredDeliveries  []scheduledDelivery
}

func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil {
		return nil, errors.New("room store is required")
	}
	if cfg.Hub == nil {
		cfg.Hub = bus.New(256)
	}
	if cfg.Settings.StallWarningSeconds == 0 {
		cfg.Settings.StallWarningSeconds = model.DefaultRoomSettings().StallWarningSeconds
	}
	if cfg.Settings.StallWarningSeconds != -1 && (cfg.Settings.StallWarningSeconds < 30 || cfg.Settings.StallWarningSeconds > 86400) {
		return nil, errors.New("stall_warning_seconds must be -1 (disabled) or between 30 and 86400")
	}
	if cfg.Slot1Factory == nil {
		cfg.Slot1Factory = agent.SlotFactory(false, cfg.Slot1Config.Runtime.CanonicalForSlot(model.ActorSlot1))
	}
	if cfg.Slot2Factory == nil {
		cfg.Slot2Factory = agent.SlotFactory(false, cfg.Slot2Config.Runtime.CanonicalForSlot(model.ActorSlot2))
	}

	e := &Engine{
		cfg:                 cfg,
		adapters:            make(map[model.ActorID]agent.Adapter, 2),
		lastRuntimeActivity: make(map[model.ActorID]time.Time, 2),
		stallWarnedTurn:     make(map[model.ActorID]string, 2),
		deliveryMu: map[model.ActorID]chan struct{}{
			model.ActorSlot1: make(chan struct{}, 1),
			model.ActorSlot2: make(chan struct{}, 1),
		},
	}
	if err := e.restore(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) restore() error {
	events, err := e.cfg.Store.Load()
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := e.apply(event); err != nil {
			return fmt.Errorf("replay event %d (%s): %w", event.Seq, event.Kind, err)
		}
	}
	if e.snapshot.Meta.ID != "" {
		if e.cfg.RequireCollaborationMatch && (e.snapshot.Meta.Collaboration == nil || e.cfg.Collaboration == nil || *e.snapshot.Meta.Collaboration != *e.cfg.Collaboration) {
			return errors.New("collaboration is immutable; create a new Room to change its mode or instructions")
		}
		if err := e.ensureSnapshotDefaults(); err != nil {
			return err
		}
		return e.expireRestoredTransientState()
	}

	name := strings.TrimSpace(e.cfg.Name)
	if name == "" {
		name = defaultRoomName(e.cfg.Slot1Config.Runtime, e.cfg.Slot2Config.Runtime)
	}
	collaboration := model.CloneCollaboration(e.cfg.Collaboration)
	if collaboration == nil {
		defaults, err := (model.Collaboration{}).ForCreation()
		if err != nil {
			return err
		}
		collaboration = &defaults
	} else if err := collaboration.Validate(); err != nil {
		return err
	}

	meta := model.RoomMeta{
		Collaboration: collaboration,
		ID:            model.NewID("room"),
		Name:          name,
		Repo:          e.cfg.Repo,
		CreatedAt:     time.Now().UTC(),
	}
	e.mu.Lock()
	e.snapshot = model.RoomSnapshot{
		Meta:         meta,
		Settings:     e.cfg.Settings,
		Participants: make(map[model.ActorID]model.ParticipantSnapshot, 2),
		Messages:     make([]model.Message, 0, 128),
		Approvals:    make([]model.Approval, 0),
		Events:       make([]model.Event, 0, 128),
	}
	e.mu.Unlock()
	if _, err := e.record(EventRoomCreated, model.ActorSystem, meta); err != nil {
		return err
	}
	if _, err := e.record(EventSettingsUpdated, model.ActorSystem, e.cfg.Settings); err != nil {
		return err
	}
	runtimes := e.runtimeKinds()
	identities := model.ParticipantIdentities(runtimes)
	participants := []model.ParticipantSnapshot{
		{
			ID: model.ActorSlot1, DisplayName: identities[model.ActorSlot1].DisplayName, MentionHandle: identities[model.ActorSlot1].MentionHandle,
			Role: model.RolePeer, State: model.StateStopped, Model: e.cfg.Slot1Config.Model,
			RuntimeKind: e.cfg.Slot1Config.Runtime.CanonicalForSlot(model.ActorSlot1),
		},
		{
			ID: model.ActorSlot2, DisplayName: identities[model.ActorSlot2].DisplayName, MentionHandle: identities[model.ActorSlot2].MentionHandle,
			Role: model.RolePeer, State: model.StateStopped, Model: e.cfg.Slot2Config.Model,
			RuntimeKind: e.cfg.Slot2Config.Runtime.CanonicalForSlot(model.ActorSlot2),
		},
	}
	for _, participant := range participants {
		participant.PermissionProfile = model.PermissionConfigured
		participant.Responsibility = meta.Collaboration.Responsibility(participant.ID)
		if _, err := e.record(EventParticipantUpdated, participant.ID, participant); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) ensureSnapshotDefaults() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snapshot.Settings.StallWarningSeconds == 0 {
		e.snapshot.Settings.StallWarningSeconds = model.DefaultRoomSettings().StallWarningSeconds
	}
	if e.snapshot.Participants == nil {
		e.snapshot.Participants = make(map[model.ActorID]model.ParticipantSnapshot, 2)
	}
	identities := model.ParticipantIdentities(e.runtimeKinds())
	for _, actor := range []model.ActorID{model.ActorSlot1, model.ActorSlot2} {
		participant, ok := e.snapshot.Participants[actor]
		if !ok {
			return fmt.Errorf("Room is missing participant %s", actor)
		}
		participant.DisplayName = identities[actor].DisplayName
		participant.MentionHandle = identities[actor].MentionHandle
		if participant.Role != model.RolePeer {
			return fmt.Errorf("unsupported stored participant role %q; create a new Room", participant.Role)
		}
		participant.Responsibility = e.snapshot.Meta.Collaboration.Responsibility(actor)
		if !participant.PermissionProfile.Valid() {
			return fmt.Errorf("invalid stored permission profile %q", participant.PermissionProfile)
		}

		// Runtime processes do not survive PairRoom restarts. Session IDs do.
		participant.State = model.StateStopped
		participant.CurrentTurn = ""
		e.snapshot.Participants[actor] = participant
	}
	if e.snapshot.Messages == nil {
		e.snapshot.Messages = make([]model.Message, 0)
	}
	if e.snapshot.Turns == nil {
		e.snapshot.Turns = make([]model.TurnSummary, 0)
	}
	for i := range e.snapshot.Messages {
		message := &e.snapshot.Messages[i]
		ensureMessageLifecycleMaps(message)
		for target, delivery := range message.Delivery {
			if _, ok := message.Processing[target]; ok {
				continue
			}
			switch delivery {
			case model.DeliveryStarted, model.DeliveryInjected:
				message.Processing[target] = model.ProcessingWorking
			default:
				message.Processing[target] = model.ProcessingWaiting
			}
		}
	}
	if e.snapshot.Approvals == nil {
		e.snapshot.Approvals = make([]model.Approval, 0)
	}
	return nil
}

// expireRestoredTransientState preserves Room-owned FIFO entries that never
// crossed a native boundary. A submission already in its acceptance window has
// unknown ownership after a crash and therefore fails for explicit retry.
// Vendor server-request IDs are connection-local, so pending approvals cannot
// safely survive a daemon restart.
func (e *Engine) expireRestoredTransientState() error {
	e.mu.RLock()
	type transientDelivery struct {
		messageID string
		target    model.ActorID
		state     model.DeliveryState
	}
	var deliveries []transientDelivery
	var recoverable []scheduledDelivery
	for _, message := range e.snapshot.Messages {
		for target, state := range message.Delivery {
			switch state {
			case model.DeliveryPending, model.DeliveryQueued:
				if !message.Processing[target].Terminal() {
					recoverable = append(recoverable, scheduledDelivery{message: cloneMessage(message), target: target, forceQueue: true})
				}
			case model.DeliverySubmitting:
				deliveries = append(deliveries, transientDelivery{messageID: message.ID, target: target, state: state})
			}
		}
	}
	var approvals []model.Approval
	for _, approval := range e.snapshot.Approvals {
		if approval.Status == "pending" {
			approvals = append(approvals, approval)
		}
	}
	e.mu.RUnlock()

	for _, pending := range deliveries {
		e.delivery(pending.messageID, pending.target, model.DeliveryFailed, "PairRoom restarted while native submission ownership was unknown; explicit retry is required")
		e.processing(pending.messageID, pending.target, model.ProcessingFailed, "PairRoom restarted while native submission ownership was unknown; explicit retry is required to avoid duplicate execution", "")
	}

	e.mu.RLock()
	type transientProcessing struct {
		messageID string
		target    model.ActorID
	}
	var processing []transientProcessing
	for _, message := range e.snapshot.Messages {
		for target, state := range message.Processing {
			delivery := message.Delivery[target]
			if (state == model.ProcessingWaiting || state == model.ProcessingWorking) && (delivery == model.DeliveryStarted || delivery == model.DeliveryInjected) {
				processing = append(processing, transientProcessing{messageID: message.ID, target: target})
			}
		}
	}
	e.mu.RUnlock()
	for _, item := range processing {
		e.processing(item.messageID, item.target, model.ProcessingCancelled, "PairRoom restarted before the native runtime reported completion", "")
	}
	for _, approval := range approvals {
		now := time.Now().UTC()
		approval.Status = "expired"
		approval.Decision = "runtime_restarted"
		approval.ResolvedAt = &now
		if _, err := e.record(EventApprovalUpdated, model.ActorSystem, approval); err != nil {
			return err
		}
	}
	e.turnMu.Lock()
	e.restoredDeliveries = recoverable
	// Event Log order is the FIFO contract. Map iteration above is deliberately
	// avoided here: a restored message may contain more than one target in an
	// older/corrupt projection, and a deterministic tie-break keeps recovery
	// reproducible instead of letting Go's map order choose the next native turn.
	sort.SliceStable(e.restoredDeliveries, func(i, j int) bool {
		left, right := e.restoredDeliveries[i], e.restoredDeliveries[j]
		if left.message.Seq != right.message.Seq {
			return left.message.Seq < right.message.Seq
		}
		return left.target < right.target
	})
	e.turnMu.Unlock()
	return nil
}

// Start initializes the two runtime adapters. AutoStart controls whether the
// vendor processes are launched immediately; StartTurn always lazy-starts them.
func (e *Engine) Start(parent context.Context) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("room is closed")
	}
	if e.started {
		e.mu.Unlock()
		return nil
	}
	e.ctx, e.cancel = context.WithCancel(parent)
	slot1Participant := e.snapshot.Participants[model.ActorSlot1]
	slot2Participant := e.snapshot.Participants[model.ActorSlot2]
	repo := e.snapshot.Meta.Repo
	roomName := e.snapshot.Meta.Name
	roomID := e.snapshot.Meta.ID
	e.mu.Unlock()

	boundaries := map[model.ActorID]model.WorkspaceBoundary{
		model.ActorSlot1: {Kind: "live", Path: repo},
		model.ActorSlot2: {Kind: "live", Path: repo},
	}

	slot1Cfg := e.cfg.Slot1Config
	slot1Cfg.Actor = model.ActorSlot1
	slot1Cfg.Runtime = slot1Cfg.Runtime.CanonicalForSlot(model.ActorSlot1)
	slot1Cfg.PeerRuntime = e.cfg.Slot2Config.Runtime.CanonicalForSlot(model.ActorSlot2)
	slot1Cfg.Repo = boundaries[model.ActorSlot1].Path
	slot1Cfg.DataDir = e.cfg.Store.Dir()
	slot1Cfg.RoomName = roomName
	slot1Cfg.RoomID = roomID
	if !slot1Cfg.RequireExactSession {
		slot1Cfg.SessionID = slot1Participant.SessionID
	}
	slot2Cfg := e.cfg.Slot2Config
	slot2Cfg.Actor = model.ActorSlot2
	slot2Cfg.Runtime = slot2Cfg.Runtime.CanonicalForSlot(model.ActorSlot2)
	slot2Cfg.PeerRuntime = e.cfg.Slot1Config.Runtime.CanonicalForSlot(model.ActorSlot1)
	slot2Cfg.Repo = boundaries[model.ActorSlot2].Path
	slot2Cfg.DataDir = e.cfg.Store.Dir()
	slot2Cfg.RoomName = roomName
	slot2Cfg.RoomID = roomID
	if !slot2Cfg.RequireExactSession {
		slot2Cfg.SessionID = slot2Participant.SessionID
	}

	slot1Cfg = e.configureParticipant(slot1Cfg, slot1Participant)
	slot2Cfg = e.configureParticipant(slot2Cfg, slot2Participant)
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("room is closed")
	}
	e.started = true
	e.adapters[model.ActorSlot1] = e.cfg.Slot1Factory(slot1Cfg, e.HandleRuntimeEvent)
	e.adapters[model.ActorSlot2] = e.cfg.Slot2Factory(slot2Cfg, e.HandleRuntimeEvent)
	slot1Adapter := e.adapters[model.ActorSlot1]
	slot2Adapter := e.adapters[model.ActorSlot2]
	autoStart := e.cfg.AutoStart
	now := time.Now().UTC()
	e.lastRuntimeActivity[model.ActorSlot1] = now
	e.lastRuntimeActivity[model.ActorSlot2] = now
	e.mu.Unlock()

	for actor, boundary := range boundaries {
		actor, boundary := actor, boundary
		_ = e.mutateParticipant(model.ActorSystem, actor, func(p *model.ParticipantSnapshot) {
			p.Workspace = boundary
			applyPermissionRuntimeProjection(p, actor, e.cfg)
			if p.Workspace.Path == "" {
				p.Workspace.Path = repo
			}
		})
	}
	// Apply the stored permission profile before either native process starts.
	if err := slot1Adapter.SetNativeAccess(parent, nativeAccess(slot1Participant)); err != nil {
		return fmt.Errorf("apply slot1 permissions: %w", err)
	}
	if err := slot2Adapter.SetNativeAccess(parent, nativeAccess(slot2Participant)); err != nil {
		return fmt.Errorf("apply slot2 permissions: %w", err)
	}
	e.resumeRestoredDeliveries(parent)

	go e.monitorStalledTurns()

	if autoStart {
		for _, actor := range []model.ActorID{model.ActorSlot1, model.ActorSlot2} {
			actor := actor
			go func() {
				ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
				defer cancel()
				if err := e.StartAgent(ctx, actor); err != nil {
					e.notice("error", fmt.Sprintf("%s failed to start: %v", e.participantName(actor), err))
				}
			}()
		}
	}
	return nil
}

func (e *Engine) Subscribe() (<-chan model.Event, func()) { return e.cfg.Hub.Subscribe() }

var ErrAttachmentReferenced = errors.New("attachment is already part of the durable room transcript")

func (e *Engine) Close() error {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	if e.cancel != nil {
		e.cancel()
	}
	adapters := make([]agent.Adapter, 0, len(e.adapters))
	for _, adapter := range e.adapters {
		adapters = append(adapters, adapter)
	}
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result error
	for _, adapter := range adapters {
		if err := adapter.Stop(ctx); err != nil {
			result = errors.Join(result, fmt.Errorf("stop %s adapter: %w", adapter.Actor(), err))
		}
	}
	// Persist in-progress summary checkpoints after adapters can no longer
	// report, and before the writer closes.
	if err := e.flushTurnSummaries("", time.Time{}); err != nil {
		result = errors.Join(result, err)
	}

	if err := e.cfg.Store.Close(); err != nil {
		result = errors.Join(result, fmt.Errorf("close Room event store: %w", err))
	}
	return result
}

func (e *Engine) notice(level, text string) {
	_, _ = e.record(EventSystemNotice, model.ActorSystem, model.SystemNotice{Level: level, Text: text})
}

// markStoreFatalLocked records the first durable-store failure and publishes
// one transient (sequence-zero, never persisted) system notice, so the Room UI
// shows why mutations stopped instead of silently losing facts. Callers hold
// e.mu for writing; Hub.Publish is a non-blocking buffered send.
func (e *Engine) markStoreFatalLocked(cause error, roomID string) {
	if e.storeFatal != nil {
		return
	}
	e.storeFatal = cause
	payload, err := json.Marshal(model.SystemNotice{Level: "error", Text: "Room event log writes are failing; new facts cannot be recorded until the Service restarts this Room. Check disk space and permissions."})
	if err != nil {
		return
	}
	e.cfg.Hub.Publish(model.Event{RoomID: roomID, Kind: EventSystemNotice, Actor: model.ActorSystem, Data: payload, CreatedAt: time.Now().UTC()})
}

// Fatal reports the first durable-store failure, if any, so the Service can
// surface this runtime as failed instead of reporting a healthy Active phase.
func (e *Engine) Fatal() error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.storeFatal
}

// defaultRoomName generates the one-time display name for a standalone Room
// from its actual slot runtimes. Room names are display metadata, never
// lookup keys, and a generated name must not claim vendors the pair does not
// run (for example "Claude × Codex" for a Grok pair).
func defaultRoomName(a, b model.RuntimeKind) string {
	label := func(kind model.RuntimeKind) string {
		switch kind.Canonical() {
		case model.RuntimeClaude:
			return "Claude"
		case model.RuntimeCodex:
			return "Codex"
		case model.RuntimeGrok:
			return "Grok"
		default:
			return ""
		}
	}
	first, second := label(a), label(b)
	if first == "" || second == "" {
		return "PairRoom"
	}
	return first + " × " + second
}
