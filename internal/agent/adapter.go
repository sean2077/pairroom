package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
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
	Env                    map[string]string `json:"-"`
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
	OrdinaryReviewerPolicy model.OrdinaryReviewerPolicy
	MockDelay              time.Duration
}

type Adapter interface {
	Actor() model.ActorID
	Start(context.Context) error
	StartTurn(context.Context, model.AgentInput) error
	Steer(context.Context, model.AgentInput) SteerOutcome
	Interrupt(context.Context) error
	Stop(context.Context) error
	ResolveApproval(context.Context, string, model.ApprovalResolution) error
	SetRole(context.Context, model.ParticipantRole) error
	SetWorkspace(context.Context, string) error
	State() model.AgentState
	SessionID() string
}

type SteerState string

const (
	SteerAccepted    SteerState = "accepted"
	SteerUnavailable SteerState = "unavailable"
	SteerRejected    SteerState = "rejected"
	SteerUnknown     SteerState = "unknown"
)

type SteerOutcome struct {
	State  SteerState
	Detail string
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
	cfg.Runtime = model.RuntimeClaude
	return newHumanInputAdapter(cfg, actor, sink, func(innerSink EventSink) Adapter { return NewClaude(cfg, innerSink) })
}
func CodexFactory(cfg Config, sink EventSink) Adapter {
	actor := factoryActor(cfg, model.ActorCodex)
	cfg.Runtime = model.RuntimeCodex
	return newHumanInputAdapter(cfg, actor, sink, func(innerSink EventSink) Adapter { return NewCodex(cfg, innerSink) })
}
func GrokFactory(cfg Config, sink EventSink) Adapter {
	actor := factoryActor(cfg, model.ActorClaude)
	cfg.Runtime = model.RuntimeGrok
	return newHumanInputAdapter(cfg, actor, sink, func(innerSink EventSink) Adapter { return NewGrok(cfg, innerSink) })
}
func MockFactory(cfg Config, sink EventSink) Adapter {
	return newHumanInputAdapter(cfg, cfg.Actor, sink, func(innerSink EventSink) Adapter { return NewMock(cfg, innerSink) })
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
	// The legacy `serve` command and Engine defaults construct slot factories
	// directly, outside the Service's transcript-boundary wrapper. Keep the
	// credential redaction guarantee at this lowest common process boundary so
	// native stderr, protocol diagnostics, and startup failures cannot expose a
	// provider secret on any entry point.
	return RedactingFactory(FactoryFor(kind))
}

func collaborationPrompt(cfg Config) string {
	if strings.TrimSpace(cfg.SystemPrompt) != "" {
		return appendInstructions(cfg.SystemPrompt, cfg.AdditionalInstructions)
	}
	return appendInstructions(prompt.BootstrapPromptWithRuntime(cfg.Actor, cfg.Runtime, cfg.PeerRuntime), cfg.AdditionalInstructions)
}

func configuredParticipantName(cfg Config) string {
	runtimes := map[model.ActorID]model.RuntimeKind{
		cfg.Actor:                         cfg.Runtime.CanonicalForSlot(cfg.Actor),
		model.OtherParticipant(cfg.Actor): cfg.PeerRuntime.CanonicalForSlot(model.OtherParticipant(cfg.Actor)),
	}
	return model.ParticipantIdentityFor(cfg.Actor, runtimes).DisplayName
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

func redactRuntimeSecrets(text string, env map[string]string) string {
	if text == "" || len(env) == 0 {
		return text
	}
	values := make([]string, 0, len(env))
	for key, value := range env {
		upper := strings.ToUpper(key)
		sensitive := false
		for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "PRIVATE_KEY", "CREDENTIAL", "AUTH", "COOKIE", "HTTP_HEADER"} {
			if strings.Contains(upper, marker) {
				sensitive = true
				break
			}
		}
		if sensitive && len(value) >= 4 {
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		text = strings.ReplaceAll(text, value, "[REDACTED]")
		encoded, err := json.Marshal(value)
		if err == nil && len(encoded) >= 2 {
			text = strings.ReplaceAll(text, string(encoded[1:len(encoded)-1]), "[REDACTED]")
		}
	}
	return text
}

var ErrApprovalUnsupported = errors.New("runtime does not expose interactive approvals through this adapter")

func runtimeEvent(actor model.ActorID, kind string) model.RuntimeEvent {
	return model.RuntimeEvent{Agent: actor, Kind: kind, CreatedAt: time.Now().UTC()}
}
