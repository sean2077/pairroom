package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

	recentEventLimit = 600
)

type Config struct {
	Name          string
	Repo          string
	Settings      model.RoomSettings
	Store         *store.JSONLStore
	Hub           *bus.Hub
	ClaudeFactory agent.Factory
	CodexFactory  agent.Factory
	ClaudeConfig  agent.Config
	CodexConfig   agent.Config
	AutoStart     bool
}

type SendRequest struct {
	Text    string          `json:"text"`
	To      []model.ActorID `json:"to,omitempty"`
	ReplyTo string          `json:"reply_to,omitempty"`
}

type RetryRequest struct {
	To []model.ActorID `json:"to,omitempty"`
}

type Engine struct {
	mu sync.RWMutex

	cfg      Config
	snapshot model.RoomSnapshot
	adapters map[model.ActorID]agent.Adapter
	ctx      context.Context
	cancel   context.CancelFunc
	started  bool
	closed   bool

	lastRuntimeActivity map[model.ActorID]time.Time
	stallWarnedTurn     map[model.ActorID]string
}

func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil {
		return nil, errors.New("room store is required")
	}
	if cfg.Hub == nil {
		cfg.Hub = bus.New(256)
	}
	if cfg.Settings.RoutingMode == "" {
		cfg.Settings = model.DefaultRoomSettings()
	}
	if !cfg.Settings.RoutingMode.Valid() {
		return nil, fmt.Errorf("invalid routing mode %q", cfg.Settings.RoutingMode)
	}
	if cfg.Settings.MaxHops < 1 {
		cfg.Settings.MaxHops = model.DefaultRoomSettings().MaxHops
	}
	if cfg.Settings.StallWarningSeconds == 0 {
		cfg.Settings.StallWarningSeconds = model.DefaultRoomSettings().StallWarningSeconds
	}
	if cfg.Settings.StallWarningSeconds != -1 && (cfg.Settings.StallWarningSeconds < 30 || cfg.Settings.StallWarningSeconds > 86400) {
		return nil, errors.New("stall_warning_seconds must be -1 (disabled) or between 30 and 86400")
	}
	if cfg.ClaudeFactory == nil {
		cfg.ClaudeFactory = agent.ClaudeFactory
	}
	if cfg.CodexFactory == nil {
		cfg.CodexFactory = agent.CodexFactory
	}

	e := &Engine{
		cfg:                 cfg,
		adapters:            make(map[model.ActorID]agent.Adapter, 2),
		lastRuntimeActivity: make(map[model.ActorID]time.Time, 2),
		stallWarnedTurn:     make(map[model.ActorID]string, 2),
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
		e.ensureSnapshotDefaults()
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
	participants := []model.ParticipantSnapshot{
		{ID: model.ActorClaude, DisplayName: model.ActorClaude.DisplayName(), Role: model.RoleDriver, State: model.StateStopped, Model: e.cfg.ClaudeConfig.Model},
		{ID: model.ActorCodex, DisplayName: model.ActorCodex.DisplayName(), Role: model.RoleReviewer, State: model.StateStopped, Model: e.cfg.CodexConfig.Model},
	}
	for _, participant := range participants {
		if _, err := e.record(EventParticipantUpdated, participant.ID, participant); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) ensureSnapshotDefaults() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snapshot.Settings.RoutingMode == "" {
		e.snapshot.Settings = model.DefaultRoomSettings()
	}
	if e.snapshot.Settings.MaxHops < 1 {
		e.snapshot.Settings.MaxHops = model.DefaultRoomSettings().MaxHops
	}
	// Schema v1 had no stall-warning field. Zero therefore means "use the
	// current default"; -1 explicitly disables warnings.
	if e.snapshot.Settings.StallWarningSeconds == 0 {
		e.snapshot.Settings.StallWarningSeconds = model.DefaultRoomSettings().StallWarningSeconds
	}
	if e.snapshot.Participants == nil {
		e.snapshot.Participants = make(map[model.ActorID]model.ParticipantSnapshot, 2)
	}
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		participant, ok := e.snapshot.Participants[actor]
		if !ok {
			role := model.RolePeer
			if actor == model.ActorClaude {
				role = model.RoleDriver
			} else {
				role = model.RoleReviewer
			}
			participant = model.ParticipantSnapshot{ID: actor, DisplayName: actor.DisplayName(), Role: role, State: model.StateStopped}
		}
		if participant.DisplayName == "" {
			participant.DisplayName = actor.DisplayName()
		}
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
}

// expireRestoredTransientState closes records that were durable but never
// handed to a runtime before the previous PairRoom process stopped. Vendor
// server-request IDs are connection-local, so pending approvals cannot safely
// survive a daemon restart.
func (e *Engine) expireRestoredTransientState() error {
	e.mu.RLock()
	type pendingDelivery struct {
		messageID string
		target    model.ActorID
	}
	var deliveries []pendingDelivery
	for _, message := range e.snapshot.Messages {
		for target, state := range message.Delivery {
			if state == model.DeliveryPending {
				deliveries = append(deliveries, pendingDelivery{messageID: message.ID, target: target})
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
		e.delivery(pending.messageID, pending.target, model.DeliverySkipped, "PairRoom restarted before runtime submission completed")
	}

	e.mu.RLock()
	type transientProcessing struct {
		messageID string
		target    model.ActorID
	}
	var processing []transientProcessing
	for _, message := range e.snapshot.Messages {
		for target, state := range message.Processing {
			if state == model.ProcessingWaiting || state == model.ProcessingWorking {
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
	return nil
}

// Start initializes the two runtime adapters. AutoStart controls whether the
// vendor processes are launched immediately; Submit always lazy-starts them.
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
	e.started = true

	claudeParticipant := e.snapshot.Participants[model.ActorClaude]
	codexParticipant := e.snapshot.Participants[model.ActorCodex]
	claudeCfg := e.cfg.ClaudeConfig
	claudeCfg.Actor = model.ActorClaude
	claudeCfg.Repo = e.snapshot.Meta.Repo
	claudeCfg.DataDir = e.cfg.Store.Dir()
	claudeCfg.RoomName = e.snapshot.Meta.Name
	claudeCfg.SessionID = claudeParticipant.SessionID
	codexCfg := e.cfg.CodexConfig
	codexCfg.Actor = model.ActorCodex
	codexCfg.Repo = e.snapshot.Meta.Repo
	codexCfg.DataDir = e.cfg.Store.Dir()
	codexCfg.RoomName = e.snapshot.Meta.Name
	codexCfg.SessionID = codexParticipant.SessionID

	e.adapters[model.ActorClaude] = e.cfg.ClaudeFactory(claudeCfg, e.HandleRuntimeEvent)
	e.adapters[model.ActorCodex] = e.cfg.CodexFactory(codexCfg, e.HandleRuntimeEvent)
	autoStart := e.cfg.AutoStart
	now := time.Now().UTC()
	e.lastRuntimeActivity[model.ActorClaude] = now
	e.lastRuntimeActivity[model.ActorCodex] = now
	e.mu.Unlock()

	go e.monitorStalledTurns()

	if autoStart {
		for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
			actor := actor
			go func() {
				ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
				defer cancel()
				if err := e.StartAgent(ctx, actor); err != nil {
					e.notice("error", fmt.Sprintf("%s failed to start: %v", actor.DisplayName(), err))
				}
			}()
		}
	}
	return nil
}

func (e *Engine) Snapshot() model.RoomSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneSnapshot(e.snapshot)
}

func (e *Engine) Subscribe() (<-chan model.Event, func()) { return e.cfg.Hub.Subscribe() }

func (e *Engine) Send(ctx context.Context, req SendRequest) (model.Message, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return model.Message{}, errors.New("message text is required")
	}
	targets := e.resolveUserTargets(text, req.To)
	if len(targets) == 0 {
		return model.Message{}, errors.New("message has no target; use @claude, @codex, or @all")
	}
	threadID := e.threadForReply(req.ReplyTo)
	message := model.Message{
		ID:                      model.NewID("msg"),
		From:                    model.ActorUser,
		To:                      targets,
		Text:                    text,
		ReplyTo:                 req.ReplyTo,
		ThreadID:                threadID,
		CreatedAt:               time.Now().UTC(),
		Delivery:                make(map[model.ActorID]model.DeliveryState, len(targets)),
		DeliveryDetail:          make(map[model.ActorID]string, len(targets)),
		Processing:              make(map[model.ActorID]model.ProcessingState, len(targets)),
		ProcessingDetail:        make(map[model.ActorID]string, len(targets)),
		ProcessingTurn:          make(map[model.ActorID]string, len(targets)),
		ProcessingLastUpdatedAt: make(map[model.ActorID]time.Time, len(targets)),
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
	// Return the persisted message immediately; vendor startup and delivery are
	// intentionally asynchronous so an IM send never blocks on login/network.
	for _, target := range targets {
		target := target
		go e.deliver(e.runtimeContext(ctx), message, target)
	}
	return message, nil
}

// Retry creates a new auditable message rather than mutating a past message.
// Reusing the original ID would make a late vendor acknowledgment ambiguous
// and could hide duplicate execution. The caller can retry only targets whose
// previous delivery or processing state is terminal and unsuccessful.
func (e *Engine) Retry(ctx context.Context, messageID string, req RetryRequest) (model.Message, error) {
	e.mu.RLock()
	original, found := e.findMessageLocked(messageID)
	e.mu.RUnlock()
	if !found {
		return model.Message{}, fmt.Errorf("unknown message %q", messageID)
	}

	targets := model.NormalizeActors(req.To)
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
		return model.Message{}, errors.New("message has no failed, cancelled, skipped, or superseded target to retry")
	}
	for _, target := range targets {
		if !retryableTarget(original, target) {
			return model.Message{}, fmt.Errorf("delivery to %s is not retryable", target.DisplayName())
		}
	}

	now := time.Now().UTC()
	retry := model.Message{
		ID:                      model.NewID("msg"),
		From:                    original.From,
		To:                      targets,
		Text:                    original.Text,
		ReplyTo:                 original.ReplyTo,
		RetryOf:                 original.ID,
		ThreadID:                original.ThreadID,
		Hop:                     original.Hop,
		CreatedAt:               now,
		Delivery:                make(map[model.ActorID]model.DeliveryState, len(targets)),
		DeliveryDetail:          make(map[model.ActorID]string, len(targets)),
		Processing:              make(map[model.ActorID]model.ProcessingState, len(targets)),
		ProcessingDetail:        make(map[model.ActorID]string, len(targets)),
		ProcessingTurn:          make(map[model.ActorID]string, len(targets)),
		ProcessingLastUpdatedAt: make(map[model.ActorID]time.Time, len(targets)),
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
		target := target
		go e.deliver(e.runtimeContext(ctx), retry, target)
	}
	return retry, nil
}

func (e *Engine) StartAgent(ctx context.Context, actor model.ActorID) error {
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
	if err := e.StopAgent(ctx, actor); err != nil {
		return err
	}
	return e.StartAgent(ctx, actor)
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
	return adapter.Interrupt(ctx)
}

func (e *Engine) ResolveApproval(ctx context.Context, approvalID, decision string) error {
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
	if err := adapter.ResolveApproval(ctx, approvalID, decision); err != nil {
		return err
	}
	now := time.Now().UTC()
	current.Status = "resolved"
	current.Decision = decision
	current.ResolvedAt = &now
	_, err = e.record(EventApprovalUpdated, model.ActorUser, *current)
	return err
}

func (e *Engine) UpdateSettings(settings model.RoomSettings) error {
	if !settings.RoutingMode.Valid() {
		return fmt.Errorf("invalid routing mode %q", settings.RoutingMode)
	}
	if settings.MaxHops < 1 || settings.MaxHops > 30 {
		return errors.New("max_agent_hops must be between 1 and 30")
	}
	if settings.StallWarningSeconds == 0 {
		settings.StallWarningSeconds = model.DefaultRoomSettings().StallWarningSeconds
	}
	if settings.StallWarningSeconds < -1 || settings.StallWarningSeconds > 86400 || (settings.StallWarningSeconds > 0 && settings.StallWarningSeconds < 30) {
		return errors.New("stall_warning_seconds must be -1 (disabled) or between 30 and 86400")
	}
	_, err := e.record(EventSettingsUpdated, model.ActorUser, settings)
	return err
}

func (e *Engine) SetRole(actor model.ActorID, role model.ParticipantRole) error {
	if !actor.ValidParticipant() {
		return errors.New("participant must be claude or codex")
	}
	if !role.Valid() {
		return fmt.Errorf("invalid role %q", role)
	}
	return e.mutateParticipant(model.ActorUser, actor, func(participant *model.ParticipantSnapshot) {
		participant.Role = role
	})
}

func (e *Engine) SwitchDriver(driver model.ActorID) error {
	if !driver.ValidParticipant() {
		return errors.New("driver must be claude or codex")
	}
	reviewer := model.OtherParticipant(driver)
	if err := e.SetRole(driver, model.RoleDriver); err != nil {
		return err
	}
	return e.SetRole(reviewer, model.RoleReviewer)
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
	for _, adapter := range adapters {
		_ = adapter.Stop(ctx)
	}
	return e.cfg.Store.Close()
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

func (e *Engine) deliver(ctx context.Context, message model.Message, target model.ActorID) {
	adapter, err := e.adapter(target)
	if err != nil {
		e.delivery(message.ID, target, model.DeliveryFailed, err.Error())
		e.processing(message.ID, target, model.ProcessingFailed, "input was not submitted: "+err.Error(), "")
		return
	}
	e.mu.RLock()
	participant := e.snapshot.Participants[target]
	settings := e.snapshot.Settings
	e.mu.RUnlock()
	input := model.AgentInput{
		MessageID:   message.ID,
		ThreadID:    message.ThreadID,
		Hop:         message.Hop,
		From:        message.From,
		To:          target,
		Text:        message.Text,
		ReplyTo:     message.ReplyTo,
		Role:        participant.Role,
		RoutingMode: settings.RoutingMode,
		MaxHops:     settings.MaxHops,
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	state, err := adapter.Submit(deliveryCtx, input)
	if err != nil {
		e.delivery(message.ID, target, model.DeliveryFailed, err.Error())
		e.processing(message.ID, target, model.ProcessingFailed, "runtime did not accept input: "+err.Error(), "")
		e.updateParticipant(target, func(p *model.ParticipantSnapshot) {
			p.State = model.StateError
			p.LastError = err.Error()
			p.LastActivity = time.Now().UTC()
		})
		return
	}
	e.delivery(message.ID, target, state, "")
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
			p.LastActivity = runtimeEvent.CreatedAt
		})
	case model.RuntimeInputProcessing:
		if runtimeEvent.CorrelationID != "" {
			state := model.ProcessingWorking
			if runtimeEvent.Name == string(model.ProcessingWaiting) {
				state = model.ProcessingWaiting
			}
			e.processing(runtimeEvent.CorrelationID, runtimeEvent.Agent, state, runtimeEvent.Text, runtimeEvent.TurnID)
		}
	case model.RuntimeInputCompleted:
		if runtimeEvent.CorrelationID != "" {
			e.processing(runtimeEvent.CorrelationID, runtimeEvent.Agent, model.ProcessingCompleted, runtimeEvent.Text, runtimeEvent.TurnID)
		}
	case model.RuntimeInputCancelled:
		if runtimeEvent.CorrelationID != "" {
			e.processing(runtimeEvent.CorrelationID, runtimeEvent.Agent, model.ProcessingCancelled, runtimeEvent.Text, runtimeEvent.TurnID)
		}
	case model.RuntimeInputFailed:
		if runtimeEvent.CorrelationID != "" {
			e.processing(runtimeEvent.CorrelationID, runtimeEvent.Agent, model.ProcessingFailed, runtimeEvent.Text, runtimeEvent.TurnID)
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
		if runtimeEvent.CorrelationID != "" {
			e.processing(runtimeEvent.CorrelationID, runtimeEvent.Agent, model.ProcessingWorking, "native turn started", runtimeEvent.TurnID)
		}
		e.updateParticipant(runtimeEvent.Agent, func(p *model.ParticipantSnapshot) {
			p.State = model.StateWorking
			p.CurrentTurn = runtimeEvent.TurnID
			p.LastActivity = runtimeEvent.CreatedAt
		})
	case model.RuntimeTurnCompleted:
		e.updateParticipant(runtimeEvent.Agent, func(p *model.ParticipantSnapshot) {
			if p.State != model.StateWaiting {
				p.State = model.StateIdle
			}
			if p.CurrentTurn == runtimeEvent.TurnID || runtimeEvent.TurnID == "" {
				p.CurrentTurn = ""
			}
			p.LastActivity = runtimeEvent.CreatedAt
		})
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
		// Runtime errors happen after a harness has accepted the input. Keep the
		// transport-level delivery result intact and project only execution failure.
		// Submit() errors are the sole source of DeliveryFailed.
		if runtimeEvent.CorrelationID != "" {
			e.processing(runtimeEvent.CorrelationID, runtimeEvent.Agent, model.ProcessingFailed, runtimeEvent.Text, runtimeEvent.TurnID)
		}
		e.updateParticipant(runtimeEvent.Agent, func(p *model.ParticipantSnapshot) {
			p.State = model.StateError
			p.LastError = runtimeEvent.Text
			p.LastActivity = runtimeEvent.CreatedAt
		})
	case model.RuntimeFinal:
		e.onFinal(runtimeEvent)
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
	cleanText, control := stripControl(runtimeEvent.Text)
	if strings.TrimSpace(cleanText) == "" {
		return
	}

	e.mu.RLock()
	incoming, found := e.findMessageLocked(runtimeEvent.CorrelationID)
	settings := e.snapshot.Settings
	latestHumanSeq := e.latestHumanSeqLocked()
	e.mu.RUnlock()
	if !found {
		incoming = model.Message{ID: runtimeEvent.CorrelationID, ThreadID: model.NewID("thread"), Hop: 0}
	}

	hop := incoming.Hop + 1
	targets := e.agentTargets(runtimeEvent.Agent, cleanText, control, hop, incoming.Seq, latestHumanSeq, settings)
	to := []model.ActorID{model.ActorUser}
	to = append(to, targets...)
	message := model.Message{
		ID:                      model.NewID("msg"),
		From:                    runtimeEvent.Agent,
		To:                      to,
		Text:                    cleanText,
		ReplyTo:                 incoming.ID,
		ThreadID:                incoming.ThreadID,
		Hop:                     hop,
		TurnID:                  runtimeEvent.TurnID,
		CreatedAt:               time.Now().UTC(),
		Delivery:                make(map[model.ActorID]model.DeliveryState, len(targets)),
		DeliveryDetail:          make(map[model.ActorID]string, len(targets)),
		Processing:              make(map[model.ActorID]model.ProcessingState, len(targets)),
		ProcessingDetail:        make(map[model.ActorID]string, len(targets)),
		ProcessingTurn:          make(map[model.ActorID]string, len(targets)),
		ProcessingLastUpdatedAt: make(map[model.ActorID]time.Time, len(targets)),
	}
	if message.ThreadID == "" {
		message.ThreadID = model.NewID("thread")
	}
	for _, target := range targets {
		message.Delivery[target] = model.DeliveryPending
		message.Processing[target] = model.ProcessingWaiting
		message.ProcessingLastUpdatedAt[target] = message.CreatedAt
	}
	if _, err := e.record(EventMessageCreated, runtimeEvent.Agent, message); err != nil {
		return
	}
	for _, target := range targets {
		target := target
		go e.deliver(e.runtimeContext(context.Background()), message, target)
	}
}

// processingFallback gives adapters that only return a DeliveryState a minimal
// processing lifecycle without overwriting richer runtime events emitted during
// Submit. Native adapters can report a turn ID and vendor-specific detail before
// Submit returns; those events remain authoritative.
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

func (e *Engine) agentTargets(actor model.ActorID, text, control string, hop int, sourceSeq, latestHumanSeq uint64, settings model.RoomSettings) []model.ActorID {
	if settings.RoutingMode == model.RoutingManual {
		return nil
	}
	if sourceSeq > 0 && latestHumanSeq > sourceSeq {
		e.notice("info", fmt.Sprintf("A newer user message superseded %s's automatic handoff; the response remains visible in the room.", actor.DisplayName()))
		return nil
	}
	if hop >= settings.MaxHops {
		e.notice("info", "Automatic agent handoff paused at the configured hop limit.")
		return nil
	}
	if stopsConversation(control) || prompt.MentionsHuman(text) {
		return nil
	}
	explicit := prompt.Mentions(text, actor)
	out := explicit[:0]
	for _, target := range explicit {
		if target != actor {
			out = append(out, target)
		}
	}
	if len(out) > 0 {
		return model.NormalizeActors(out)
	}
	if settings.RoutingMode == model.RoutingRoundtable {
		return []model.ActorID{model.OtherParticipant(actor)}
	}
	return nil
}

func (e *Engine) delivery(messageID string, target model.ActorID, state model.DeliveryState, detail string) {
	update := model.DeliveryUpdate{
		MessageID: messageID,
		Target:    target,
		State:     state,
		Detail:    detail,
	}

	// Validate and persist a delivery transition under the same room lock. This
	// prevents a fast runtime error from being followed by a late Submit return
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
	if !found || !deliveryTransitionAllowed(current, state) {
		e.mu.Unlock()
		return
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
		participant.DisplayName = participantID.DisplayName()
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

func (e *Engine) resolveUserTargets(text string, explicit []model.ActorID) []model.ActorID {
	if targets := model.NormalizeActors(explicit); len(targets) > 0 {
		return targets
	}
	if targets := prompt.Mentions(text, model.ActorUser); len(targets) > 0 {
		return targets
	}
	return []model.ActorID{model.ActorClaude, model.ActorCodex}
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

func (e *Engine) latestHumanSeqLocked() uint64 {
	for i := len(e.snapshot.Messages) - 1; i >= 0; i-- {
		if e.snapshot.Messages[i].From == model.ActorUser {
			return e.snapshot.Messages[i].Seq
		}
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

func (e *Engine) apply(event model.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applyLocked(event)
}

func (e *Engine) applyLocked(event model.Event) error {
	e.snapshot.LatestSeq = event.Seq
	switch event.Kind {
	case EventRoomCreated:
		if err := json.Unmarshal(event.Data, &e.snapshot.Meta); err != nil {
			return err
		}
	case EventSettingsUpdated:
		if err := json.Unmarshal(event.Data, &e.snapshot.Settings); err != nil {
			return err
		}
	case EventParticipantUpdated:
		var participant model.ParticipantSnapshot
		if err := json.Unmarshal(event.Data, &participant); err != nil {
			return err
		}
		if e.snapshot.Participants == nil {
			e.snapshot.Participants = make(map[model.ActorID]model.ParticipantSnapshot)
		}
		e.snapshot.Participants[participant.ID] = participant
	case EventMessageCreated:
		var message model.Message
		if err := json.Unmarshal(event.Data, &message); err != nil {
			return err
		}
		message.Seq = event.Seq
		e.snapshot.Messages = append(e.snapshot.Messages, message)
	case EventDeliveryUpdated:
		var update model.DeliveryUpdate
		if err := json.Unmarshal(event.Data, &update); err != nil {
			return err
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
		if err := json.Unmarshal(event.Data, &update); err != nil {
			return err
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
	}

	e.snapshot.Events = append(e.snapshot.Events, event)
	if len(e.snapshot.Events) > recentEventLimit {
		e.snapshot.Events = append([]model.Event(nil), e.snapshot.Events[len(e.snapshot.Events)-recentEventLimit:]...)
	}
	return nil
}

func deliveryTransitionAllowed(current, next model.DeliveryState) bool {
	if current == "" || current == model.DeliveryPending {
		return true
	}
	// Failure and explicit policy skips are terminal. This matters when a very
	// fast runtime emits an error before Submit returns its initial state.
	if current == model.DeliveryFailed || current == model.DeliverySkipped {
		return false
	}
	if next == model.DeliveryFailed || next == model.DeliverySkipped {
		return true
	}
	// started/injected/queued describe how the input entered the native harness,
	// not a processing lifecycle; don't let a late initial update rewrite them.
	return current == next
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
		out.Messages[i] = message
		out.Messages[i].To = append([]model.ActorID(nil), message.To...)
		out.Messages[i].Delivery = cloneDelivery(message.Delivery)
		out.Messages[i].DeliveryDetail = cloneDetails(message.DeliveryDetail)
		out.Messages[i].Processing = cloneProcessing(message.Processing)
		out.Messages[i].ProcessingDetail = cloneDetails(message.ProcessingDetail)
		out.Messages[i].ProcessingTurn = cloneDetails(message.ProcessingTurn)
		out.Messages[i].ProcessingLastUpdatedAt = cloneTimes(message.ProcessingLastUpdatedAt)
	}
	out.Approvals = make([]model.Approval, len(in.Approvals))
	for i, approval := range in.Approvals {
		out.Approvals[i] = approval
		out.Approvals[i].Detail = append(json.RawMessage(nil), approval.Detail...)
	}
	out.Participants = make(map[model.ActorID]model.ParticipantSnapshot, len(in.Participants))
	for key, value := range in.Participants {
		value.Runtime = cloneRuntimeInfo(value.Runtime)
		out.Participants[key] = value
	}
	out.Events = make([]model.Event, len(in.Events))
	for i, event := range in.Events {
		out.Events[i] = event
		out.Events[i].Data = append(json.RawMessage(nil), event.Data...)
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
	if processing == model.ProcessingFailed || processing == model.ProcessingCancelled || processing == model.ProcessingSuperseded {
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
				detail := fmt.Sprintf("%s has produced no runtime event for %s", item.actor.DisplayName(), item.age.Round(time.Second))
				if item.turn != "" {
					detail += " during turn " + item.turn
				}
				e.notice("warning", detail+". It may be running a long command, waiting on an unexposed prompt, or stalled.")
			}
		}
	}
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

var controlPattern = regexp.MustCompile(`(?mi)^\s*\[PAIRROOM:(CONTINUE|CONSENSUS|WAIT|BLOCKED|DONE|IMPLEMENTED|REVIEW_APPROVED|REVIEW_CHANGES)\]\s*$`)

func stripControl(text string) (string, string) {
	matches := controlPattern.FindAllStringSubmatch(text, -1)
	control := ""
	if len(matches) > 0 {
		control = strings.ToUpper(matches[len(matches)-1][1])
	}
	return strings.TrimSpace(controlPattern.ReplaceAllString(text, "")), control
}

func stopsConversation(control string) bool {
	switch control {
	case "CONSENSUS", "WAIT", "BLOCKED", "DONE", "IMPLEMENTED", "REVIEW_APPROVED":
		return true
	default:
		return false
	}
}
