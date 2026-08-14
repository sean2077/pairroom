package agent

import (
	"context"
	"errors"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

type EventSink func(model.RuntimeEvent)

type Config struct {
	Actor               model.ActorID
	Repo                string
	DataDir             string
	RoomName            string
	ClientVersion       string
	Command             string
	Model               string
	Effort              string
	PermissionMode      string
	ApprovalPolicy      string
	Sandbox             string
	SessionID           string
	RequireExactSession bool
	SystemPrompt        string
	MockDelay           time.Duration
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

func ClaudeFactory(cfg Config, sink EventSink) Adapter { return NewClaude(cfg, sink) }
func CodexFactory(cfg Config, sink EventSink) Adapter  { return NewCodex(cfg, sink) }
func MockFactory(cfg Config, sink EventSink) Adapter   { return NewMock(cfg, sink) }

var ErrApprovalUnsupported = errors.New("runtime does not expose interactive approvals through this adapter")

func runtimeEvent(actor model.ActorID, kind string) model.RuntimeEvent {
	return model.RuntimeEvent{Agent: actor, Kind: kind, CreatedAt: time.Now().UTC()}
}
