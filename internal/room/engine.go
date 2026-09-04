package room

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/bus"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
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
	EventParticipantsBatch  = "participants.batch.updated"
	EventTurnSummaryUpdated = "turn.summary.updated"

	eventServiceRoomRenamed         = "service.room.renamed"
	eventServiceBindingsCompleted   = "service.room.bindings.completed"
	eventServiceBindingMaterialized = "service.room.binding.materialized"
	recentEventLimit                = 600
)

type Config struct {
	Name                  string
	Repo                  string
	Settings              model.RoomSettings
	Store                 *store.JSONLStore
	Hub                   *bus.Hub
	ClaudeFactory         agent.Factory
	CodexFactory          agent.Factory
	ClaudeConfig          agent.Config
	CodexConfig           agent.Config
	Attachments           AttachmentStore
	Workspaces            WorkspaceManager
	AutoStart             bool
	OnSessionMaterialized func(context.Context, model.ActorID, string) error
}

// AttachmentStore keeps presentation metadata durable while resolving an
// opaque attachment ID to a local path only at the native-agent boundary.
type AttachmentStore interface {
	Resolve(id string) (model.Attachment, string, error)
	DiscoverRepoImages(text, source string) []model.Attachment
}

type WorkspaceManager interface {
	DriverBoundary() model.WorkspaceBoundary
	Refresh(context.Context) (model.WorkspaceBoundary, error)
	Cleanup(context.Context) error
}

type participantBatch struct {
	Reason       string                      `json:"reason"`
	Participants []model.ParticipantSnapshot `json:"participants"`
}

type serviceRoomRenamedProjection struct {
	Name string `json:"name"`
}

type serviceBindingProjection struct {
	Agent     model.ActorID `json:"agent"`
	SessionID string        `json:"session_id"`
	Pending   bool          `json:"pending"`
}

type serviceBindingsCompletedProjection struct {
	Bindings map[model.ActorID]serviceBindingProjection `json:"bindings"`
}

type serviceBindingMaterializedProjection struct {
	Binding serviceBindingProjection `json:"binding"`
}

type SendRequest struct {
	Text        string                `json:"text"`
	To          []model.ActorID       `json:"to,omitempty"`
	TargetRole  model.ParticipantRole `json:"target_role,omitempty"`
	ReplyTo     string                `json:"reply_to,omitempty"`
	Attachments []model.Attachment    `json:"attachments,omitempty"`
	Intent      model.MessageIntent   `json:"intent,omitempty"`
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
	mu        sync.RWMutex
	routingMu sync.Mutex
	turnMu    sync.Mutex

	cfg      Config
	snapshot model.RoomSnapshot
	adapters map[model.ActorID]agent.Adapter
	ctx      context.Context
	cancel   context.CancelFunc
	started  bool
	closed   bool

	lastRuntimeActivity map[model.ActorID]time.Time
	stallWarnedTurn     map[model.ActorID]string
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
	if cfg.ClaudeFactory == nil {
		cfg.ClaudeFactory = agent.SlotFactory(false, cfg.ClaudeConfig.Runtime.CanonicalForSlot(model.ActorClaude))
	}
	if cfg.CodexFactory == nil {
		cfg.CodexFactory = agent.SlotFactory(false, cfg.CodexConfig.Runtime.CanonicalForSlot(model.ActorCodex))
	}

	e := &Engine{
		cfg:                 cfg,
		adapters:            make(map[model.ActorID]agent.Adapter, 2),
		lastRuntimeActivity: make(map[model.ActorID]time.Time, 2),
		stallWarnedTurn:     make(map[model.ActorID]string, 2),
		deliveryMu: map[model.ActorID]chan struct{}{
			model.ActorClaude: make(chan struct{}, 1),
			model.ActorCodex:  make(chan struct{}, 1),
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
		if err := e.ensureSnapshotDefaults(); err != nil {
			return err
		}
		return e.expireRestoredTransientState()
	}

	name := strings.TrimSpace(e.cfg.Name)
	if name == "" {
		name = "Claude × Codex"
	}
	meta := model.RoomMeta{
		ID:        model.NewID("room"),
		Name:      name,
		Repo:      e.cfg.Repo,
		CreatedAt: time.Now().UTC(),
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
			ID: model.ActorClaude, DisplayName: identities[model.ActorClaude].DisplayName, MentionHandle: identities[model.ActorClaude].MentionHandle,
			Role: model.RoleDriver, State: model.StateStopped, Model: e.cfg.ClaudeConfig.Model,
			RuntimeKind: e.cfg.ClaudeConfig.Runtime.CanonicalForSlot(model.ActorClaude),
		},
		{
			ID: model.ActorCodex, DisplayName: identities[model.ActorCodex].DisplayName, MentionHandle: identities[model.ActorCodex].MentionHandle,
			Role: model.RoleReviewer, State: model.StateStopped, Model: e.cfg.CodexConfig.Model,
			RuntimeKind: e.cfg.CodexConfig.Runtime.CanonicalForSlot(model.ActorCodex),
		},
	}
	for _, participant := range participants {
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
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		participant, ok := e.snapshot.Participants[actor]
		if !ok {
			role := model.RolePeer
			if actor == model.ActorClaude {
				role = model.RoleDriver
			} else {
				role = model.RoleReviewer
			}
			participant = model.ParticipantSnapshot{ID: actor, Role: role, State: model.StateStopped}
		}
		participant.DisplayName = identities[actor].DisplayName
		participant.MentionHandle = identities[actor].MentionHandle
		if !participant.Role.Valid() {
			participant.Role = model.RolePeer
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
	claudeParticipant := e.snapshot.Participants[model.ActorClaude]
	codexParticipant := e.snapshot.Participants[model.ActorCodex]
	repo := e.snapshot.Meta.Repo
	roomName := e.snapshot.Meta.Name
	e.mu.Unlock()

	boundaries, err := e.prepareWorkspaceBoundaries(parent, claudeParticipant.Role, codexParticipant.Role)
	if err != nil {
		e.mu.Lock()
		if e.cancel != nil {
			e.cancel()
		}
		e.ctx, e.cancel = nil, nil
		e.mu.Unlock()
		return err
	}

	claudeCfg := e.cfg.ClaudeConfig
	claudeCfg.Actor = model.ActorClaude
	claudeCfg.Runtime = claudeCfg.Runtime.CanonicalForSlot(model.ActorClaude)
	claudeCfg.PeerRuntime = e.cfg.CodexConfig.Runtime.CanonicalForSlot(model.ActorCodex)
	claudeCfg.Repo = boundaries[model.ActorClaude].Path
	claudeCfg.DataDir = e.cfg.Store.Dir()
	claudeCfg.RoomName = roomName
	if !claudeCfg.RequireExactSession {
		claudeCfg.SessionID = claudeParticipant.SessionID
	}
	codexCfg := e.cfg.CodexConfig
	codexCfg.Actor = model.ActorCodex
	codexCfg.Runtime = codexCfg.Runtime.CanonicalForSlot(model.ActorCodex)
	codexCfg.PeerRuntime = e.cfg.ClaudeConfig.Runtime.CanonicalForSlot(model.ActorClaude)
	codexCfg.Repo = boundaries[model.ActorCodex].Path
	codexCfg.DataDir = e.cfg.Store.Dir()
	codexCfg.RoomName = roomName
	if !codexCfg.RequireExactSession {
		codexCfg.SessionID = codexParticipant.SessionID
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("room is closed")
	}
	e.started = true
	e.adapters[model.ActorClaude] = e.cfg.ClaudeFactory(claudeCfg, e.HandleRuntimeEvent)
	e.adapters[model.ActorCodex] = e.cfg.CodexFactory(codexCfg, e.HandleRuntimeEvent)
	claudeAdapter := e.adapters[model.ActorClaude]
	codexAdapter := e.adapters[model.ActorCodex]
	autoStart := e.cfg.AutoStart
	now := time.Now().UTC()
	e.lastRuntimeActivity[model.ActorClaude] = now
	e.lastRuntimeActivity[model.ActorCodex] = now
	e.mu.Unlock()

	for actor, boundary := range boundaries {
		actor, boundary := actor, boundary
		_ = e.mutateParticipant(model.ActorSystem, actor, func(p *model.ParticipantSnapshot) {
			p.Workspace = boundary
			if p.Workspace.Path == "" {
				p.Workspace.Path = repo
			}
		})
	}
	// Apply the restored room roles before either native process starts. Codex
	// enforces reviewer policy per turn; Claude maps reviewer to native plan mode.
	if err := claudeAdapter.SetRole(parent, claudeParticipant.Role); err != nil {
		return fmt.Errorf("apply Claude role: %w", err)
	}
	if err := codexAdapter.SetRole(parent, codexParticipant.Role); err != nil {
		return fmt.Errorf("apply Codex role: %w", err)
	}
	e.resumeRestoredDeliveries(parent)

	go e.monitorStalledTurns()

	if autoStart {
		for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
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

func (e *Engine) prepareWorkspaceBoundaries(ctx context.Context, claudeRole, codexRole model.ParticipantRole) (map[model.ActorID]model.WorkspaceBoundary, error) {
	e.mu.RLock()
	repo := e.snapshot.Meta.Repo
	e.mu.RUnlock()
	driver := model.WorkspaceBoundary{Kind: "driver-live", Path: repo}
	if e.cfg.Workspaces != nil {
		driver = e.cfg.Workspaces.DriverBoundary()
	}
	boundaries := map[model.ActorID]model.WorkspaceBoundary{
		model.ActorClaude: driver,
		model.ActorCodex:  driver,
	}
	if claudeRole != model.RoleReviewer && codexRole != model.RoleReviewer {
		return boundaries, nil
	}
	if e.cfg.Workspaces == nil {
		fallback := driver
		fallback.Kind = "reviewer-live-fallback"
		fallback.ReadOnly = false
		fallback.Warnings = []string{"reviewer snapshot manager is unavailable; reviewer sees the live working tree"}
		if claudeRole == model.RoleReviewer {
			boundaries[model.ActorClaude] = fallback
		}
		if codexRole == model.RoleReviewer {
			boundaries[model.ActorCodex] = fallback
		}
		return boundaries, nil
	}
	reviewer, err := e.cfg.Workspaces.Refresh(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare reviewer workspace: %w", err)
	}
	if claudeRole == model.RoleReviewer {
		boundaries[model.ActorClaude] = reviewer
	}
	if codexRole == model.RoleReviewer {
		boundaries[model.ActorCodex] = reviewer
	}
	return boundaries, nil
}

func (e *Engine) Snapshot() model.RoomSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneSnapshot(e.snapshot)
}

// WindowedSnapshot returns the newest messages while retaining full room and
// runtime state. The authoritative in-memory/event-sourced transcript remains
// complete; this is only a transport optimization for long-lived rooms.
func (e *Engine) WindowedSnapshot(limit int) model.RoomSnapshot {
	snapshot := e.Snapshot()
	total := len(snapshot.Messages)
	if limit <= 0 || limit > 1000 {
		limit = 250
	}
	if total > limit {
		snapshot.Messages = append([]model.Message(nil), snapshot.Messages[total-limit:]...)
	}
	window := &model.MessageWindow{Total: total, Loaded: len(snapshot.Messages), HasMore: total > len(snapshot.Messages)}
	if len(snapshot.Messages) > 0 {
		window.OldestSeq = snapshot.Messages[0].Seq
	}
	snapshot.MessageWindow = window
	return snapshot
}

// MessagesPage returns messages immediately before beforeSeq. A zero cursor
// addresses the newest page. Results remain chronological for direct merging.
func (e *Engine) MessagesPage(beforeSeq uint64, limit int) model.MessagePage {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	end := len(e.snapshot.Messages)
	if beforeSeq > 0 {
		end = 0
		for i := range e.snapshot.Messages {
			if e.snapshot.Messages[i].Seq >= beforeSeq {
				break
			}
			end = i + 1
		}
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	messages := make([]model.Message, end-start)
	for i := start; i < end; i++ {
		messages[i-start] = cloneMessage(e.snapshot.Messages[i])
	}
	page := model.MessagePage{Messages: messages, Total: len(e.snapshot.Messages), HasMore: start > 0}
	if len(messages) > 0 {
		page.OldestSeq = messages[0].Seq
	}
	return page
}

func (e *Engine) Subscribe() (<-chan model.Event, func()) { return e.cfg.Hub.Subscribe() }

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
	attachments, err := e.canonicalAttachments(req.Attachments)
	if err != nil {
		return model.Message{}, err
	}
	if text == "" && len(attachments) == 0 {
		return model.Message{}, errors.New("message text or image is required")
	}

	e.routingMu.Lock()
	defer e.routingMu.Unlock()
	targets, err := e.resolveUserTargets(text, req.To, req.TargetRole, req.ReplyTo)
	if err != nil {
		return model.Message{}, err
	}
	if len(targets) == 0 {
		return model.Message{}, errors.New("message has no target; use an exact Agent handle, Driver, or Reviewer")
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
		return errors.New("cancel target must be claude or codex")
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
	unlock, err := e.lockDeliveryScope(ctx, target)
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
	e.mu.RLock()
	var current *model.Approval
	for i := range e.snapshot.Approvals {
		if e.snapshot.Approvals[i].ID == approvalID {
			copy := e.snapshot.Approvals[i]
			current = &copy
			break
		}
	}
	e.mu.RUnlock()
	if current == nil {
		return fmt.Errorf("unknown approval %q", approvalID)
	}
	if current.Status != "pending" {
		return fmt.Errorf("approval %q is already %s", approvalID, current.Status)
	}
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

func (e *Engine) SetRole(ctx context.Context, actor model.ActorID, role model.ParticipantRole) error {
	if !actor.ValidParticipant() {
		return errors.New("participant must be claude or codex")
	}
	if !role.Valid() {
		return fmt.Errorf("invalid role %q", role)
	}
	// The reviewer snapshot manager is shared by both participants. Serialize a
	// role transition with both submission paths so a concurrent delivery cannot
	// mutate the live tree while a new reviewer snapshot is captured.
	unlock, err := e.lockAllDeliveries(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	e.routingMu.Lock()
	defer e.routingMu.Unlock()
	other := model.OtherParticipant(actor)
	e.mu.RLock()
	actorSnapshot := e.snapshot.Participants[actor]
	otherSnapshot := e.snapshot.Participants[other]
	e.mu.RUnlock()
	if err := roleChangeSafe(actorSnapshot); err != nil {
		return err
	}
	if actorSnapshot.Role == role {
		return nil
	}

	boundary := model.WorkspaceBoundary{Kind: "driver-live", Path: e.snapshot.Meta.Repo}
	if e.cfg.Workspaces != nil {
		boundary = e.cfg.Workspaces.DriverBoundary()
	}
	if role == model.RoleReviewer {
		if e.cfg.Workspaces != nil {
			if otherSnapshot.Role == model.RoleReviewer {
				return errors.New("reviewer snapshot requires a single Reviewer; assign the other participant as Driver or Peer first")
			}
			if err := roleChangeSafe(otherSnapshot); err != nil {
				return fmt.Errorf("cannot capture reviewer snapshot while the peer may be changing the live workspace: %w", err)
			}
		}
		boundaries, err := e.prepareWorkspaceBoundaries(ctx,
			roleFor(actor, model.ActorClaude, role, otherSnapshot.Role),
			roleFor(actor, model.ActorCodex, role, otherSnapshot.Role),
		)
		if err != nil {
			return err
		}
		boundary = boundaries[actor]
	}
	adapter, err := e.adapter(actor)
	if err != nil {
		return err
	}
	wasRunning := adapter.State() != model.StateStopped
	oldBoundary := actorSnapshot.Workspace
	if oldBoundary.Path == "" {
		oldBoundary = model.WorkspaceBoundary{Kind: "driver-live", Path: e.snapshot.Meta.Repo}
	}
	if wasRunning {
		if err := adapter.Stop(ctx); err != nil {
			return fmt.Errorf("stop %s before role change: %w", e.participantName(actor), err)
		}
	}
	rollback := func() {
		_ = adapter.SetWorkspace(context.Background(), oldBoundary.Path)
		_ = adapter.SetRole(context.Background(), actorSnapshot.Role)
		if wasRunning {
			_ = adapter.Start(context.Background())
		}
	}
	if err := adapter.SetWorkspace(ctx, boundary.Path); err != nil {
		rollback()
		return err
	}
	if err := adapter.SetRole(ctx, role); err != nil {
		rollback()
		return err
	}
	if wasRunning {
		if err := adapter.Start(ctx); err != nil {
			rollback()
			return fmt.Errorf("restart %s after role change: %w", e.participantName(actor), err)
		}
	}
	return e.mutateParticipant(model.ActorUser, actor, func(participant *model.ParticipantSnapshot) {
		participant.Role = role
		participant.Workspace = boundary
		applyRoleRuntimeProjection(participant, actor, role, e.cfg)
	})
}

func (e *Engine) SwitchDriver(ctx context.Context, driver model.ActorID) error {
	if !driver.ValidParticipant() {
		return errors.New("driver must be claude or codex")
	}
	unlock, err := e.lockAllDeliveries(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	e.routingMu.Lock()
	defer e.routingMu.Unlock()
	reviewer := model.OtherParticipant(driver)
	e.mu.RLock()
	old := map[model.ActorID]model.ParticipantSnapshot{
		model.ActorClaude: e.snapshot.Participants[model.ActorClaude],
		model.ActorCodex:  e.snapshot.Participants[model.ActorCodex],
	}
	e.mu.RUnlock()
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		if err := roleChangeSafe(old[actor]); err != nil {
			return err
		}
	}

	roles := map[model.ActorID]model.ParticipantRole{
		driver:   model.RoleDriver,
		reviewer: model.RoleReviewer,
	}
	boundaries, err := e.prepareWorkspaceBoundaries(ctx, roles[model.ActorClaude], roles[model.ActorCodex])
	if err != nil {
		return err
	}
	adapters := map[model.ActorID]agent.Adapter{}
	running := map[model.ActorID]bool{}
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		adapter, err := e.adapter(actor)
		if err != nil {
			return err
		}
		adapters[actor] = adapter
		running[actor] = adapter.State() != model.StateStopped
	}
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		if running[actor] {
			if err := adapters[actor].Stop(ctx); err != nil {
				for _, stopped := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
					if stopped == actor {
						break
					}
					if running[stopped] {
						_ = adapters[stopped].Start(context.Background())
					}
				}
				return fmt.Errorf("stop %s before driver switch: %w", e.participantName(actor), err)
			}
		}
	}
	rollback := func() {
		for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
			_ = adapters[actor].Stop(context.Background())
			path := old[actor].Workspace.Path
			if path == "" {
				path = e.snapshot.Meta.Repo
			}
			_ = adapters[actor].SetWorkspace(context.Background(), path)
			_ = adapters[actor].SetRole(context.Background(), old[actor].Role)
		}
		for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
			if running[actor] {
				_ = adapters[actor].Start(context.Background())
			}
		}
	}
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		if err := adapters[actor].SetWorkspace(ctx, boundaries[actor].Path); err != nil {
			rollback()
			return fmt.Errorf("set %s workspace: %w", e.participantName(actor), err)
		}
		if err := adapters[actor].SetRole(ctx, roles[actor]); err != nil {
			rollback()
			return fmt.Errorf("set %s role: %w", e.participantName(actor), err)
		}
	}
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		if running[actor] {
			if err := adapters[actor].Start(ctx); err != nil {
				rollback()
				return fmt.Errorf("restart %s after driver switch: %w", e.participantName(actor), err)
			}
		}
	}
	participants := make([]model.ParticipantSnapshot, 0, 2)
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		participant := old[actor]
		participant.Role = roles[actor]
		participant.Workspace = boundaries[actor]
		applyRoleRuntimeProjection(&participant, actor, roles[actor], e.cfg)
		participants = append(participants, participant)
	}
	_, err = e.record(EventParticipantsBatch, model.ActorUser, participantBatch{
		Reason: "driver_switched", Participants: participants,
	})
	return err
}

func roleFor(changed, target model.ActorID, requested, other model.ParticipantRole) model.ParticipantRole {
	if changed == target {
		return requested
	}
	return other
}

func roleChangeSafe(participant model.ParticipantSnapshot) error {
	switch participant.State {
	case model.StateStopped, model.StateIdle, model.StateError:
		return nil
	default:
		return fmt.Errorf("interrupt or stop %s before changing roles or workspaces", participant.DisplayName)
	}
}

func (e *Engine) runtimeKinds() map[model.ActorID]model.RuntimeKind {
	return runtimeKindsForConfig(e.cfg)
}

func runtimeKindsForConfig(cfg Config) map[model.ActorID]model.RuntimeKind {
	return map[model.ActorID]model.RuntimeKind{
		model.ActorClaude: cfg.ClaudeConfig.Runtime.CanonicalForSlot(model.ActorClaude),
		model.ActorCodex:  cfg.CodexConfig.Runtime.CanonicalForSlot(model.ActorCodex),
	}
}

func (e *Engine) participantName(actor model.ActorID) string {
	return model.ParticipantIdentityFor(actor, e.runtimeKinds()).DisplayName
}

func slotAgentConfig(cfg Config, actor model.ActorID) agent.Config {
	if actor == model.ActorCodex {
		return cfg.CodexConfig
	}
	return cfg.ClaudeConfig
}

func applyRoleRuntimeProjection(participant *model.ParticipantSnapshot, actor model.ActorID, role model.ParticipantRole, cfg Config) {
	slot := slotAgentConfig(cfg, actor)
	kind := slot.Runtime.CanonicalForSlot(actor)
	identity := model.ParticipantIdentityFor(actor, runtimeKindsForConfig(cfg))
	participant.RuntimeKind = kind
	participant.DisplayName = identity.DisplayName
	participant.MentionHandle = identity.MentionHandle
	// Rebuild the policy projection from the immutable slot selection on every
	// role transition. A process can move from a read-only Reviewer stage back
	// to a Driver turn; leaving fields from the previous role in the snapshot
	// would make the UI/API claim a policy that is no longer active.
	participant.Runtime.PermissionMode = ""
	participant.Runtime.ApprovalPolicy = ""
	participant.Runtime.Sandbox = ""
	nativeRole := role
	if role == model.RoleReviewer && slot.OrdinaryReviewerPolicy == model.ReviewerExplicit {
		// The Reviewer workspace boundary remains owned by the Room, but the
		// explicit policy opts the native harness into the selected slot policy.
		nativeRole = model.RoleDriver
	}
	switch kind {
	case model.RuntimeClaude:
		if nativeRole == model.RoleReviewer {
			participant.Runtime.PermissionMode = "plan"
		} else {
			participant.Runtime.PermissionMode = slot.PermissionMode
		}
	case model.RuntimeCodex:
		if nativeRole == model.RoleReviewer {
			participant.Runtime.Sandbox = "readOnly"
		} else {
			participant.Runtime.ApprovalPolicy = slot.ApprovalPolicy
			participant.Runtime.Sandbox = slot.Sandbox
		}
	case model.RuntimeGrok:
		if nativeRole == model.RoleReviewer {
			participant.Runtime.PermissionMode = "plan"
			participant.Runtime.Sandbox = "read-only"
		} else {
			participant.Runtime.PermissionMode = slot.PermissionMode
			participant.Runtime.Sandbox = slot.Sandbox
		}
	}
}

func (e *Engine) Close() error {
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
	if e.cfg.Workspaces != nil {
		if err := e.cfg.Workspaces.Cleanup(ctx); err != nil {
			result = errors.Join(result, fmt.Errorf("clean up Room workspaces: %w", err))
		}
	}
	if err := e.cfg.Store.Close(); err != nil {
		result = errors.Join(result, fmt.Errorf("close Room event store: %w", err))
	}
	return result
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
	claudeUnlock, err := e.lockDelivery(ctx, model.ActorClaude)
	if err != nil {
		return nil, err
	}
	codexUnlock, err := e.lockDelivery(ctx, model.ActorCodex)
	if err != nil {
		claudeUnlock()
		return nil, err
	}
	return func() {
		codexUnlock()
		claudeUnlock()
	}, nil
}

func (e *Engine) targetUsesReviewerSnapshot(target model.ActorID) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg.Workspaces != nil && e.snapshot.Participants[target].Role == model.RoleReviewer
}

// lockDeliveryScope keeps ordinary peer/Driver submissions independent while
// serializing a Reviewer refresh with both submissions. Role transitions take
// the same pair of locks, and the role is rechecked after lock acquisition.
func (e *Engine) lockDeliveryScope(ctx context.Context, target model.ActorID) (func(), error) {
	for {
		reviewer := e.targetUsesReviewerSnapshot(target)
		var (
			unlock func()
			err    error
		)
		if reviewer {
			unlock, err = e.lockAllDeliveries(ctx)
		} else {
			unlock, err = e.lockDelivery(ctx, target)
		}
		if err != nil {
			return nil, err
		}
		if reviewer == e.targetUsesReviewerSnapshot(target) {
			return unlock, nil
		}
		unlock()
	}
}

// refreshReviewerWorkspace recreates the reviewer snapshot immediately before
// a safe new reviewer turn. The startup snapshot is only an initial boundary;
// without this refresh a reviewer could inspect files from before the Driver's
// latest implementation. Active reviewer turns keep their existing snapshot so
// steering remains correlated to one stable filesystem view.
func (e *Engine) refreshReviewerWorkspace(ctx context.Context, target model.ActorID, adapter agent.Adapter) error {
	e.mu.RLock()
	participant := e.snapshot.Participants[target]
	peer := e.snapshot.Participants[model.OtherParticipant(target)]
	e.mu.RUnlock()
	if participant.Role != model.RoleReviewer || e.cfg.Workspaces == nil {
		return nil
	}
	if peer.Role == model.RoleReviewer {
		return errors.New("reviewer snapshot refresh requires a single Reviewer; assign a Driver or Peer before review")
	}
	state := adapter.State()
	switch state {
	case model.StateStopped, model.StateIdle, model.StateError:
	default:
		return nil
	}
	if state != model.StateStopped {
		if err := adapter.Stop(ctx); err != nil {
			return fmt.Errorf("stop idle reviewer before snapshot refresh: %w", err)
		}
	}
	boundary, err := e.cfg.Workspaces.Refresh(ctx)
	if err != nil {
		return fmt.Errorf("refresh reviewer snapshot: %w", err)
	}
	if err := adapter.SetWorkspace(ctx, boundary.Path); err != nil {
		return fmt.Errorf("apply refreshed reviewer workspace: %w", err)
	}
	if err := adapter.SetRole(ctx, model.RoleReviewer); err != nil {
		return fmt.Errorf("restore reviewer policy after refresh: %w", err)
	}
	return e.mutateParticipant(model.ActorSystem, target, func(p *model.ParticipantSnapshot) {
		p.Workspace = boundary
		p.LastError = ""
	})
}

func (e *Engine) deliver(ctx context.Context, message model.Message, target model.ActorID, steer bool) string {
	unlock, err := e.lockDeliveryScope(ctx, target)
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
	if !steer {
		if err := e.refreshReviewerWorkspace(ctx, target, adapter); err != nil {
			detail := "prepare reviewer workspace: " + err.Error()
			e.delivery(message.ID, target, model.DeliveryFailed, detail)
			e.processing(message.ID, target, model.ProcessingFailed, detail, "")
			e.updateParticipant(target, func(p *model.ParticipantSnapshot) {
				p.State = model.StateError
				p.LastError = detail
				p.LastActivity = time.Now().UTC()
			})
			return ""
		}
	}
	e.mu.RLock()
	participant := e.snapshot.Participants[target]
	e.mu.RUnlock()
	attachments, err := e.agentAttachments(message.Attachments)
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
		Role:        participant.Role,
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
	if isTransientRuntimeKind(runtimeEvent.Kind) {
		e.publishTransientRuntime(runtimeEvent)
	} else {
		_, _ = e.record(EventRuntime, runtimeEvent.Agent, runtimeEvent)
	}
	e.projectTurnSummary(runtimeEvent)

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
			applyRoleRuntimeProjection(p, runtimeEvent.Agent, p.Role, e.cfg)
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

func (e *Engine) projectTurnSummary(runtimeEvent model.RuntimeEvent) {
	if !runtimeEvent.Agent.ValidParticipant() || strings.TrimSpace(runtimeEvent.TurnID) == "" {
		return
	}
	id := string(runtimeEvent.Agent) + ":" + runtimeEvent.TurnID
	e.mu.RLock()
	var summary model.TurnSummary
	for _, existing := range e.snapshot.Turns {
		if existing.ID == id {
			summary = cloneTurnSummary(existing)
			break
		}
	}
	e.mu.RUnlock()
	if summary.ID == "" {
		summary = model.TurnSummary{
			ID: id, Agent: runtimeEvent.Agent, TurnID: runtimeEvent.TurnID,
			Status: "working", StartedAt: runtimeEvent.CreatedAt,
		}
	}
	if summary.StartedAt.IsZero() {
		summary.StartedAt = runtimeEvent.CreatedAt
	}
	summary.UpdatedAt = runtimeEvent.CreatedAt
	if runtimeEvent.SessionID != "" {
		summary.SessionID = runtimeEvent.SessionID
	}
	if runtimeEvent.CorrelationID != "" && !containsString(summary.MessageIDs, runtimeEvent.CorrelationID) {
		summary.MessageIDs = append(summary.MessageIDs, runtimeEvent.CorrelationID)
	}

	persist := true
	switch runtimeEvent.Kind {
	case model.RuntimeTurnStarted:
		summary.Status = "working"
	case model.RuntimeToolStarted:
		upsertTurnItem(&summary, runtimeEvent, "tool", "working")
	case model.RuntimeToolCompleted:
		upsertTurnItem(&summary, runtimeEvent, "tool", "completed")
	case model.RuntimeCommandOutput:
		item := findOrCreateTurnItem(&summary, runtimeEvent.ItemID, "command")
		item.Status = "working"
		item.Detail = boundedTail(item.Detail+runtimeEvent.Text, 12<<10)
		persist = false
	case model.RuntimePlanUpdated:
		if runtimeEvent.Text != "" {
			summary.Plan = boundedTail(summary.Plan+runtimeEvent.Text, 24<<10)
		} else if len(runtimeEvent.Data) > 0 {
			summary.Plan = boundedTail(string(runtimeEvent.Data), 24<<10)
		}
	case model.RuntimeDiffUpdated:
		if runtimeEvent.Text != "" {
			summary.Diff = boundedTail(runtimeEvent.Text, 48<<10)
		} else if len(runtimeEvent.Data) > 0 {
			summary.Diff = boundedTail(string(runtimeEvent.Data), 48<<10)
		}
	case model.RuntimeUsageUpdated:
		summary.Usage = boundedRaw(runtimeEvent.Data, 16<<10)
	case model.RuntimeFinal:
		summary.FinalText = boundedTail(runtimeEvent.Text, 48<<10)
	case model.RuntimeInputCancelled:
		summary.Status = "cancelled"
		summary.Error = boundedTail(runtimeEvent.Text, 8<<10)
	case model.RuntimeInputFailed:
		summary.Status = "failed"
		summary.Error = boundedTail(runtimeEvent.Text, 8<<10)
	case model.RuntimeError:
		// Keep diagnostic errors visible without declaring the native Turn
		// terminal. A later input.failed or turn.completed owns final status.
		summary.Error = boundedTail(runtimeEvent.Text, 8<<10)
	case model.RuntimeTurnCompleted:
		for i := range summary.Items {
			if summary.Items[i].CompletedAt == nil && summary.Items[i].Status == "working" {
				summary.Items[i].Status = "completed"
				completed := runtimeEvent.CreatedAt
				summary.Items[i].CompletedAt = &completed
			}
		}
		status := strings.TrimSpace(runtimeEvent.Name)
		if status == "" || status == "completed" || status == "success" {
			status = "completed"
		}
		if summary.Status != "failed" && summary.Status != "cancelled" {
			summary.Status = status
		}
		completed := runtimeEvent.CreatedAt
		summary.CompletedAt = &completed
		summary.DurationMillis = max(int64(0), completed.Sub(summary.StartedAt).Milliseconds())
	case model.RuntimeTextDelta, model.RuntimeState, model.RuntimeSession, model.RuntimeInfoUpdated:
		return
	default:
		// Retain a summary heartbeat for durable turn-scoped events, but avoid
		// creating extra events for unrelated adapter status notifications.
		if runtimeEvent.Kind == model.RuntimeLog || runtimeEvent.Kind == model.RuntimeApprovalRequested || runtimeEvent.Kind == model.RuntimeApprovalResolved {
			persist = false
		}
	}
	if len(summary.Items) > 128 {
		summary.Items = append([]model.TurnWorkItem(nil), summary.Items[len(summary.Items)-128:]...)
	}
	if persist {
		_, _ = e.record(EventTurnSummaryUpdated, runtimeEvent.Agent, summary)
		return
	}
	e.mu.Lock()
	replaceTurnSummaryLocked(&e.snapshot, summary)
	e.mu.Unlock()
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

func upsertTurnItem(summary *model.TurnSummary, event model.RuntimeEvent, kind, status string) {
	item := findOrCreateTurnItem(summary, event.ItemID, kind)
	if event.Name != "" {
		item.Name = event.Name
	}
	item.Status = status
	if item.StartedAt.IsZero() {
		item.StartedAt = event.CreatedAt
	}
	if event.Text != "" {
		item.Detail = boundedTail(event.Text, 12<<10)
	}
	if len(event.Data) > 0 {
		item.Data = boundedRaw(event.Data, 16<<10)
	}
	if status == "completed" || status == "failed" || status == "cancelled" {
		completed := event.CreatedAt
		item.CompletedAt = &completed
	}
}

func findOrCreateTurnItem(summary *model.TurnSummary, itemID, kind string) *model.TurnWorkItem {
	if itemID == "" {
		itemID = kind + "-" + fmt.Sprint(len(summary.Items)+1)
	}
	for i := range summary.Items {
		if summary.Items[i].ID == itemID {
			if summary.Items[i].Kind == "" {
				summary.Items[i].Kind = kind
			}
			return &summary.Items[i]
		}
	}
	summary.Items = append(summary.Items, model.TurnWorkItem{ID: itemID, Kind: kind, Status: "working"})
	return &summary.Items[len(summary.Items)-1]
}

func boundedTail(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return "…" + value[len(value)-limit+1:]
}

func boundedRaw(value json.RawMessage, limit int) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	if len(value) <= limit {
		return append(json.RawMessage(nil), value...)
	}
	wrapped, _ := json.Marshal(map[string]any{
		"truncated": true,
		"tail":      boundedTail(string(value), limit-64),
	})
	return wrapped
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func replaceTurnSummaryLocked(snapshot *model.RoomSnapshot, summary model.TurnSummary) {
	for i := range snapshot.Turns {
		if snapshot.Turns[i].ID == summary.ID {
			snapshot.Turns[i] = summary
			return
		}
	}
	snapshot.Turns = append(snapshot.Turns, summary)
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
		err = e.cfg.Store.Append(&event)
	}
	if err == nil {
		err = e.applyLocked(event)
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
		err = e.cfg.Store.Append(&event)
	}
	if err == nil {
		err = e.applyLocked(event)
	}
	e.mu.Unlock()
	if err == nil {
		e.cfg.Hub.Publish(event)
	}
}

func (e *Engine) agentTargets(actor model.ActorID, text string, sourceSeq, latestHumanSeq uint64) []model.ActorID {
	mentions := prompt.ParseMentions(text, actor, e.runtimeKinds())
	if mentions.Human {
		return nil
	}
	// A newer human instruction takes precedence over an older Agent result, including an
	// explicit peer address in that stale result.
	if sourceSeq > 0 && latestHumanSeq > sourceSeq {
		e.notice("info", fmt.Sprintf("A newer user message cancelled %s's pending Agent relay; the response remains visible in the Room.", e.participantName(actor)))
		return nil
	}
	if len(mentions.Ambiguous) > 0 {
		identities := model.ParticipantIdentities(e.runtimeKinds())
		e.notice("warning", fmt.Sprintf("%s used ambiguous handle %s; use %s or %s. No Agent relay was started.", e.participantName(actor), strings.Join(mentions.Ambiguous, ", "), identities[model.ActorClaude].MentionHandle, identities[model.ActorCodex].MentionHandle))
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
		err = e.cfg.Store.Append(&event)
	}
	if err == nil {
		err = e.applyLocked(event)
	}
	e.mu.Unlock()
	if err == nil {
		e.cfg.Hub.Publish(event)
	}
	return err == nil
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
		err = e.cfg.Store.Append(&event)
	}
	if err == nil {
		err = e.applyLocked(event)
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}
	e.cfg.Hub.Publish(event)
	return nil
}

func (e *Engine) notice(level, text string) {
	_, _ = e.record(EventSystemNotice, model.ActorSystem, model.SystemNotice{Level: level, Text: text})
}

func (e *Engine) resolveUserTargets(text string, explicit []model.ActorID, targetRole model.ParticipantRole, replyTo string) ([]model.ActorID, error) {
	targets, err := normalizeExplicitActors(explicit)
	if err != nil {
		return nil, err
	}
	if targetRole != "" {
		if len(targets) > 0 {
			return nil, errors.New("choose either explicit recipients or target_role, not both")
		}
		if targetRole != model.RoleDriver && targetRole != model.RoleReviewer {
			return nil, errors.New("target_role must be driver or reviewer")
		}
		e.mu.RLock()
		for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
			if e.snapshot.Participants[actor].Role == targetRole {
				targets = append(targets, actor)
			}
		}
		e.mu.RUnlock()
		if len(targets) != 1 {
			return nil, fmt.Errorf("target_role %q requires exactly one participant, found %d", targetRole, len(targets))
		}
		return targets, nil
	}
	if len(targets) > 0 {
		return targets, nil
	}
	mentions := prompt.ParseMentions(text, model.ActorUser, e.runtimeKinds())
	if len(mentions.Ambiguous) > 0 {
		identities := model.ParticipantIdentities(e.runtimeKinds())
		return nil, fmt.Errorf("ambiguous Agent handle %s; use %s or %s", strings.Join(mentions.Ambiguous, ", "), identities[model.ActorClaude].MentionHandle, identities[model.ActorCodex].MentionHandle)
	}
	// Removed aliases are ordinary prose once a valid current handle is also
	// present. Reject only an otherwise unaddressed message that still relies on
	// a retired alias, so legacy text cannot silently fall back to the Driver.
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
	e.mu.RLock()
	drivers := make([]model.ActorID, 0, 2)
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		if e.snapshot.Participants[actor].Role == model.RoleDriver {
			drivers = append(drivers, actor)
		}
	}
	e.mu.RUnlock()
	if len(drivers) == 1 {
		return drivers, nil
	}
	if len(drivers) == 0 {
		return nil, errors.New("message has no current Driver; choose an exact Agent handle or assign a Driver")
	}
	return nil, errors.New("message has multiple Drivers; choose an exact Agent handle or assign one Driver")
}

func normalizeExplicitActors(values []model.ActorID) ([]model.ActorID, error) {
	canonical := make([]model.ActorID, 0, len(values))
	for _, value := range values {
		actor := model.ActorID(strings.ToLower(strings.TrimSpace(string(value))))
		if !actor.ValidParticipant() {
			return nil, fmt.Errorf("invalid Agent recipient %q; use claude or codex", value)
		}
		canonical = append(canonical, actor)
	}
	return model.NormalizeActors(canonical), nil
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
		e.mu.Unlock()
		return model.Event{}, err
	}
	if err := e.applyLocked(event); err != nil {
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
// decoding. Store schema 9 has no migration or legacy-field compatibility;
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
	case EventRoomCreated:
		if err := json.Unmarshal(event.Data, &e.snapshot.Meta); err != nil {
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
	case eventServiceBindingsCompleted:
		var update serviceBindingsCompletedProjection
		if err := json.Unmarshal(event.Data, &update); err != nil {
			return err
		}
		if e.snapshot.Participants == nil {
			e.snapshot.Participants = make(map[model.ActorID]model.ParticipantSnapshot, 2)
		}
		for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
			binding, ok := update.Bindings[actor]
			if !ok || binding.Pending || binding.Agent != actor || strings.TrimSpace(binding.SessionID) == "" {
				return fmt.Errorf("service binding completion has an invalid %s binding", actor)
			}
			participant := e.snapshot.Participants[actor]
			participant.ID = actor
			identity := model.ParticipantIdentityFor(actor, e.runtimeKinds())
			participant.DisplayName = identity.DisplayName
			participant.MentionHandle = identity.MentionHandle
			participant.SessionID = strings.TrimSpace(binding.SessionID)
			e.snapshot.Participants[actor] = participant
		}
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
	case EventParticipantsBatch:
		var update participantBatch
		if err := decodeCurrentEventData(event.Data, &update); err != nil {
			return err
		}
		if e.snapshot.Participants == nil {
			e.snapshot.Participants = make(map[model.ActorID]model.ParticipantSnapshot)
		}
		for _, participant := range update.Participants {
			if !participant.ID.ValidParticipant() || strings.TrimSpace(participant.MentionHandle) == "" {
				return fmt.Errorf("invalid participant in batch update: %q", participant.ID)
			}
			e.snapshot.Participants[participant.ID] = participant
		}
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
	if len(e.snapshot.Events) > recentEventLimit {
		e.snapshot.Events = append([]model.Event(nil), e.snapshot.Events[len(e.snapshot.Events)-recentEventLimit:]...)
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

func cloneSnapshot(in model.RoomSnapshot) model.RoomSnapshot {
	out := in
	out.Messages = make([]model.Message, len(in.Messages))
	for i, message := range in.Messages {
		out.Messages[i] = cloneMessage(message)
	}
	if in.MessageWindow != nil {
		window := *in.MessageWindow
		out.MessageWindow = &window
	}
	out.Approvals = make([]model.Approval, len(in.Approvals))
	for i, approval := range in.Approvals {
		out.Approvals[i] = approval
		out.Approvals[i].Detail = append(json.RawMessage(nil), approval.Detail...)
	}
	out.Turns = make([]model.TurnSummary, len(in.Turns))
	for i, summary := range in.Turns {
		out.Turns[i] = cloneTurnSummary(summary)
	}
	out.Participants = make(map[model.ActorID]model.ParticipantSnapshot, len(in.Participants))
	for key, value := range in.Participants {
		value.Runtime = cloneRuntimeInfo(value.Runtime)
		value.Workspace.Warnings = append([]string(nil), value.Workspace.Warnings...)
		out.Participants[key] = value
	}
	out.Events = make([]model.Event, len(in.Events))
	for i, event := range in.Events {
		out.Events[i] = event
		out.Events[i].Data = append(json.RawMessage(nil), event.Data...)
	}
	return out
}

func cloneMessage(message model.Message) model.Message {
	out := message
	out.To = append([]model.ActorID(nil), message.To...)
	out.Attachments = append([]model.Attachment(nil), message.Attachments...)
	out.Delivery = cloneDelivery(message.Delivery)
	out.DeliveryDetail = cloneDetails(message.DeliveryDetail)
	out.Processing = cloneProcessing(message.Processing)
	out.ProcessingDetail = cloneDetails(message.ProcessingDetail)
	out.ProcessingTurn = cloneDetails(message.ProcessingTurn)
	out.ProcessingLastUpdatedAt = cloneTimes(message.ProcessingLastUpdatedAt)
	return out
}

func cloneTurnSummary(in model.TurnSummary) model.TurnSummary {
	out := in
	out.MessageIDs = append([]string(nil), in.MessageIDs...)
	out.Usage = append(json.RawMessage(nil), in.Usage...)
	out.Items = make([]model.TurnWorkItem, len(in.Items))
	for i, item := range in.Items {
		out.Items[i] = item
		out.Items[i].Data = append(json.RawMessage(nil), item.Data...)
	}
	return out
}

func cloneRuntimeInfo(in model.RuntimeInfo) model.RuntimeInfo {
	out := in
	out.Capabilities = append([]string(nil), in.Capabilities...)
	out.Warnings = append([]string(nil), in.Warnings...)
	out.Data = append(json.RawMessage(nil), in.Data...)
	return out
}

func cloneProcessing(in map[model.ActorID]model.ProcessingState) map[model.ActorID]model.ProcessingState {
	if in == nil {
		return nil
	}
	out := make(map[model.ActorID]model.ProcessingState, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneTimes(in map[model.ActorID]time.Time) map[model.ActorID]time.Time {
	if in == nil {
		return nil
	}
	out := make(map[model.ActorID]time.Time, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
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

func retryableTarget(message model.Message, target model.ActorID) bool {
	processing := message.Processing[target]
	if processing == model.ProcessingFailed || processing == model.ProcessingCancelled {
		return true
	}
	delivery := message.Delivery[target]
	return delivery == model.DeliveryFailed || delivery == model.DeliverySkipped
}

func (e *Engine) monitorStalledTurns() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-ticker.C:
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
			for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
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

func (e *Engine) actorHasPendingApprovalLocked(actor model.ActorID) bool {
	for _, approval := range e.snapshot.Approvals {
		if approval.Agent == actor && approval.Status == "pending" {
			return true
		}
	}
	return false
}

func cloneDelivery(in map[model.ActorID]model.DeliveryState) map[model.ActorID]model.DeliveryState {
	if in == nil {
		return nil
	}
	out := make(map[model.ActorID]model.DeliveryState, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneDetails(in map[model.ActorID]string) map[model.ActorID]string {
	if in == nil {
		return nil
	}
	out := make(map[model.ActorID]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
