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

type Engine struct {
	mu sync.RWMutex

	cfg      Config
	snapshot model.RoomSnapshot
	adapters map[model.ActorID]agent.Adapter
	ctx      context.Context
	cancel   context.CancelFunc
	started  bool
	closed   bool
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
		cfg.Settings.MaxHops = 6
	}
	if cfg.ClaudeFactory == nil {
		cfg.ClaudeFactory = agent.ClaudeFactory
	}
	if cfg.CodexFactory == nil {
		cfg.CodexFactory = agent.CodexFactory
	}

	e := &Engine{cfg: cfg, adapters: make(map[model.ActorID]agent.Adapter, 2)}
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
		return nil
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
	if e.snapshot.Approvals == nil {
		e.snapshot.Approvals = make([]model.Approval, 0)
	}
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
	e.mu.Unlock()

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
		ID:             model.NewID("msg"),
		From:           model.ActorUser,
		To:             targets,
		Text:           text,
		ReplyTo:        req.ReplyTo,
		ThreadID:       threadID,
		CreatedAt:      time.Now().UTC(),
		Delivery:       make(map[model.ActorID]model.DeliveryState, len(targets)),
		DeliveryDetail: make(map[model.ActorID]string, len(targets)),
	}
	for _, target := range targets {
		message.Delivery[target] = model.DeliveryPending
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
	e.updateParticipant(actor, func(p *model.ParticipantSnapshot) {
		p.State = model.StateStopped
		p.CurrentTurn = ""
		p.LastActivity = time.Now().UTC()
	})
	return nil
}

func (e *Engine) RestartAgent(ctx context.Context, actor model.ActorID) error {
	adapter, err := e.adapter(actor)
	if err != nil {
		return err
	}
	_ = adapter.Stop(ctx)
	return e.StartAgent(ctx, actor)
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
		e.updateParticipant(target, func(p *model.ParticipantSnapshot) {
			p.State = model.StateError
			p.LastError = err.Error()
			p.LastActivity = time.Now().UTC()
		})
		return
	}
	e.delivery(message.ID, target, state, "")
}

// HandleRuntimeEvent is the single ingress from both vendor adapters. It
// records the canonical event before projecting state and chat messages.
func (e *Engine) HandleRuntimeEvent(runtimeEvent model.RuntimeEvent) {
	if runtimeEvent.CreatedAt.IsZero() {
		runtimeEvent.CreatedAt = time.Now().UTC()
	}
	_, _ = e.record(EventRuntime, runtimeEvent.Agent, runtimeEvent)

	switch runtimeEvent.Kind {
	case model.RuntimeSession:
		e.updateParticipant(runtimeEvent.Agent, func(p *model.ParticipantSnapshot) {
			if runtimeEvent.SessionID != "" {
				p.SessionID = runtimeEvent.SessionID
			}
			p.LastActivity = runtimeEvent.CreatedAt
		})
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
		e.updateParticipant(runtimeEvent.Agent, func(p *model.ParticipantSnapshot) {
			p.State = model.StateError
			p.LastError = runtimeEvent.Text
			p.LastActivity = runtimeEvent.CreatedAt
		})
	case model.RuntimeFinal:
		e.onFinal(runtimeEvent)
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
		ID:             model.NewID("msg"),
		From:           runtimeEvent.Agent,
		To:             to,
		Text:           cleanText,
		ReplyTo:        incoming.ID,
		ThreadID:       incoming.ThreadID,
		Hop:            hop,
		TurnID:         runtimeEvent.TurnID,
		CreatedAt:      time.Now().UTC(),
		Delivery:       make(map[model.ActorID]model.DeliveryState, len(targets)),
		DeliveryDetail: make(map[model.ActorID]string, len(targets)),
	}
	if message.ThreadID == "" {
		message.ThreadID = model.NewID("thread")
	}
	for _, target := range targets {
		message.Delivery[target] = model.DeliveryPending
	}
	if _, err := e.record(EventMessageCreated, runtimeEvent.Agent, message); err != nil {
		return
	}
	for _, target := range targets {
		target := target
		go e.deliver(e.runtimeContext(context.Background()), message, target)
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
	_, _ = e.record(EventDeliveryUpdated, target, model.DeliveryUpdate{
		MessageID: messageID,
		Target:    target,
		State:     state,
		Detail:    detail,
	})
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
			e.snapshot.Messages[i].Delivery[update.Target] = update.State
			e.snapshot.Messages[i].DeliveryDetail[update.Target] = update.Detail
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

func cloneSnapshot(in model.RoomSnapshot) model.RoomSnapshot {
	out := in
	out.Messages = make([]model.Message, len(in.Messages))
	for i, message := range in.Messages {
		out.Messages[i] = message
		out.Messages[i].To = append([]model.ActorID(nil), message.To...)
		out.Messages[i].Delivery = cloneDelivery(message.Delivery)
		out.Messages[i].DeliveryDetail = cloneDetails(message.DeliveryDetail)
	}
	out.Approvals = append([]model.Approval(nil), in.Approvals...)
	out.Participants = make(map[model.ActorID]model.ParticipantSnapshot, len(in.Participants))
	for key, value := range in.Participants {
		out.Participants[key] = value
	}
	out.Events = make([]model.Event, len(in.Events))
	copy(out.Events, in.Events)
	return out
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
