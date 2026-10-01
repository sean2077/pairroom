package agent

import (
	"context"
	"errors"

	"github.com/sean2077/pairroom/internal/model"
)

// A recognized Native-only runtime must never fall through to another vendor's
// Embedded adapter, even through standalone serve/Engine factory entry points.
var errNativeOnly = errors.New("Gemini CLI supports Native rooms only; run pairroom relay install --runtime gemini and bind from your Gemini session")

type nativeOnlyAdapter struct{ actor model.ActorID }

func nativeOnlyFactory(cfg Config, _ EventSink) Adapter {
	return &nativeOnlyAdapter{actor: factoryActor(cfg, model.ActorSlot1)}
}

func (a *nativeOnlyAdapter) Actor() model.ActorID       { return a.actor }
func (*nativeOnlyAdapter) Start(context.Context) error { return errNativeOnly }
func (*nativeOnlyAdapter) StartTurn(context.Context, model.AgentInput) error {
	return errNativeOnly
}
func (*nativeOnlyAdapter) Steer(context.Context, model.AgentInput) SteerOutcome {
	return SteerOutcome{State: SteerUnavailable, Detail: errNativeOnly.Error()}
}
func (*nativeOnlyAdapter) Interrupt(context.Context) error { return errNativeOnly }
func (*nativeOnlyAdapter) Stop(context.Context) error      { return nil }
func (*nativeOnlyAdapter) ResolveApproval(context.Context, string, model.ApprovalResolution) error {
	return ErrApprovalUnsupported
}
func (*nativeOnlyAdapter) SetNativeAccess(context.Context, model.NativeAccess) error {
	return errNativeOnly
}
func (*nativeOnlyAdapter) State() model.AgentState { return model.StateError }
func (*nativeOnlyAdapter) SessionID() string       { return "" }
