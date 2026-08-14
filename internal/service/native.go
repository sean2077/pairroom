package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
	"github.com/sean2077/pairroom/internal/version"
)

type NativeProvisionerConfig struct {
	Claude agent.Config
	Codex  agent.Config
}

type NativeProvisioner struct {
	cfg NativeProvisionerConfig
}

func NewNativeProvisioner(cfg NativeProvisionerConfig) *NativeProvisioner {
	return &NativeProvisioner{cfg: cfg}
}

func (p *NativeProvisioner) Provision(ctx context.Context, project Project, actor model.ActorID, spec BindingSpec, dataDir string) (Binding, func(context.Context) error, error) {
	if p == nil {
		return Binding{}, nil, errors.New("native binding provisioner is nil")
	}
	if err := spec.Validate(); err != nil {
		return Binding{}, nil, err
	}
	var cfg agent.Config
	var factory agent.Factory
	switch actor {
	case model.ActorClaude:
		cfg = p.cfg.Claude
		factory = agent.ClaudeFactory
	case model.ActorCodex:
		cfg = p.cfg.Codex
		factory = agent.CodexFactory
	default:
		return Binding{}, nil, fmt.Errorf("unsupported binding agent %q", actor)
	}
	cfg.Actor = actor
	cfg.Repo = project.Root
	cfg.DataDir = dataDir
	cfg.RoomName = "PairRoom binding validation"
	cfg.ClientVersion = version.Current
	cfg.SessionID = strings.TrimSpace(spec.SessionID)
	cfg.RequireExactSession = spec.Mode == BindingExisting
	cfg.SystemPrompt = prompt.SystemPrompt(actor, cfg.RoomName, project.Root)
	if spec.Mode == BindingNew {
		// Neither official harness persists an empty native conversation. Probe the
		// required protocol now, but defer allocation of the vendor identity until
		// the Room owns a real input that the live adapter has accepted.
		if _, err := agent.ProbeRuntime(ctx, cfg); err != nil {
			return Binding{}, nil, err
		}
		return Binding{
			Agent: actor, Mode: BindingNew, Pending: true, BoundAt: time.Now().UTC(),
		}, func(context.Context) error { return nil }, nil
	}

	// Validation output deliberately terminates here. Starting an Existing
	// binding may cause the official harness to restore vendor context, but no
	// pre-binding transcript or opaque event is copied into PairRoom storage or
	// exposed through SSE.
	adapter := factory(cfg, func(model.RuntimeEvent) {})
	cleanup := func(stopCtx context.Context) error { return adapter.Stop(stopCtx) }
	cleanupFailure := func(cause error) error {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		stopErr := cleanup(stopCtx)
		cancel()
		if stopErr != nil {
			return errors.Join(cause, fmt.Errorf("stop temporary %s validator: %w", actor, stopErr))
		}
		return cause
	}
	if err := adapter.Start(ctx); err != nil {
		return Binding{}, nil, cleanupFailure(err)
	}
	sessionID := strings.TrimSpace(adapter.SessionID())
	if sessionID == "" {
		return Binding{}, nil, cleanupFailure(errors.New("vendor runtime returned an empty session ID"))
	}
	if spec.Mode == BindingExisting && sessionID != strings.TrimSpace(spec.SessionID) {
		return Binding{}, nil, cleanupFailure(fmt.Errorf("requested session %q could not be resumed; vendor returned %q", spec.SessionID, sessionID))
	}
	return Binding{Agent: actor, Mode: spec.Mode, SessionID: sessionID, BoundAt: time.Now().UTC()}, cleanup, nil
}

// SyntheticProvisioner is used only for deterministic Mock mode and tests. It
// never claims to validate a real vendor session.
type SyntheticProvisioner struct{}

func (SyntheticProvisioner) Provision(_ context.Context, _ Project, actor model.ActorID, spec BindingSpec, _ string) (Binding, func(context.Context) error, error) {
	if err := spec.Validate(); err != nil {
		return Binding{}, nil, err
	}
	sessionID := strings.TrimSpace(spec.SessionID)
	if spec.Mode == BindingNew {
		sessionID = model.NewID("mock-" + string(actor))
	}
	return Binding{Agent: actor, Mode: spec.Mode, SessionID: sessionID, BoundAt: time.Now().UTC()}, func(context.Context) error { return nil }, nil
}
