package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

type EventSink func(model.RuntimeEvent)

type Config struct {
	Actor                  model.ActorID
	Repo                   string
	DataDir                string
	RoomName               string
	ClientVersion          string
	Command                string
	CommandArgs            []string
	Env                    map[string]string
	Runtime                model.RuntimeKind
	PeerRuntime            model.RuntimeKind
	Provider               string
	Model                  string
	Effort                 string
	PermissionMode         string
	ApprovalPolicy         string
	Sandbox                string
	AdditionalInstructions string
	SessionID              string
	RequireExactSession    bool
	SystemPrompt           string
	MockDelay              time.Duration
}

type Adapter interface {
	Actor() model.ActorID
	Start(context.Context) error
	Submit(context.Context, model.AgentInput) (model.DeliveryState, error)
	Interrupt(context.Context) error
	Stop(context.Context) error
	ResolveApproval(context.Context, string, model.ApprovalResolution) error
	SetRole(context.Context, model.ParticipantRole) error
	SetWorkspace(context.Context, string) error
	State() model.AgentState
	SessionID() string
}

type Factory func(Config, EventSink) Adapter

func factoryActor(cfg Config, fallback model.ActorID) model.ActorID {
	if cfg.Actor.ValidParticipant() {
		return cfg.Actor
	}
	return fallback
}

func ClaudeFactory(cfg Config, sink EventSink) Adapter {
	actor := factoryActor(cfg, model.ActorClaude)
	return newWorkflowAdapter(cfg, actor, sink, func(innerSink EventSink) Adapter { return NewClaude(cfg, innerSink) })
}
func CodexFactory(cfg Config, sink EventSink) Adapter {
	actor := factoryActor(cfg, model.ActorCodex)
	return newWorkflowAdapter(cfg, actor, sink, func(innerSink EventSink) Adapter { return NewCodex(cfg, innerSink) })
}
func GrokFactory(cfg Config, sink EventSink) Adapter {
	actor := factoryActor(cfg, model.ActorClaude)
	return newWorkflowAdapter(cfg, actor, sink, func(innerSink EventSink) Adapter { return NewGrok(cfg, innerSink) })
}
func MockFactory(cfg Config, sink EventSink) Adapter {
	return newWorkflowAdapter(cfg, cfg.Actor, sink, func(innerSink EventSink) Adapter { return NewMock(cfg, innerSink) })
}

func FactoryFor(kind model.RuntimeKind) Factory {
	switch kind.Canonical() {
	case model.RuntimeCodex:
		return CodexFactory
	case model.RuntimeGrok:
		return GrokFactory
	default:
		return ClaudeFactory
	}
}

func SlotFactory(mock bool, kind model.RuntimeKind) Factory {
	if mock {
		return MockFactory
	}
	return FactoryFor(kind)
}

func collaborationPrompt(cfg Config) string {
	if strings.TrimSpace(cfg.SystemPrompt) != "" {
		return appendInstructions(cfg.SystemPrompt, cfg.AdditionalInstructions)
	}
	return appendInstructions(prompt.BootstrapPromptWithRuntime(cfg.Actor, cfg.Runtime, cfg.PeerRuntime), cfg.AdditionalInstructions)
}

func appendInstructions(base, extra string) string {
	base = strings.TrimSpace(base)
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return base
	}
	if base == "" {
		return extra
	}
	return base + "\n\n" + extra
}

var ErrApprovalUnsupported = errors.New("runtime does not expose interactive approvals through this adapter")

func runtimeEvent(actor model.ActorID, kind string) model.RuntimeEvent {
	return model.RuntimeEvent{Agent: actor, Kind: kind, CreatedAt: time.Now().UTC()}
}
