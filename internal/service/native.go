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
	Claude   agent.Config
	Codex    agent.Config
	Resolver *AgentResolver
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
	if p.cfg.Resolver != nil {
		defaults := p.cfg.Resolver.DefaultSelections()
		selection, ok := defaults[actor]
		if !ok {
			return Binding{}, nil, fmt.Errorf("default Agent selection is missing for %q", actor)
		}
		peerSelection, ok := defaults[model.OtherParticipant(actor)]
		if !ok {
			return Binding{}, nil, fmt.Errorf("default peer Agent selection is missing for %q", actor)
		}
		return p.ProvisionSelection(ctx, project, actor, spec, selection, peerSelection.Runtime, dataDir)
	}
	var cfg agent.Config
	switch actor {
	case model.ActorClaude:
		cfg = p.cfg.Claude
	case model.ActorCodex:
		cfg = p.cfg.Codex
	default:
		return Binding{}, nil, fmt.Errorf("unsupported binding agent %q", actor)
	}
	return p.provisionWithConfig(ctx, project, actor, spec, dataDir, cfg)
}

func (p *NativeProvisioner) DefaultSelections() map[model.ActorID]model.AgentSelection {
	if p != nil && p.cfg.Resolver != nil {
		return p.cfg.Resolver.DefaultSelections()
	}
	return defaultAgentSelections()
}

func (p *NativeProvisioner) ProvisionSelection(ctx context.Context, project Project, actor model.ActorID, spec BindingSpec, selection model.AgentSelection, peerRuntime model.RuntimeKind, dataDir string) (Binding, func(context.Context) error, error) {
	if p == nil {
		return Binding{}, nil, errors.New("native binding provisioner is nil")
	}
	if p.cfg.Resolver == nil {
		return p.Provision(ctx, project, actor, spec, dataDir)
	}
	if err := spec.Validate(); err != nil {
		return Binding{}, nil, err
	}
	cfg, err := p.cfg.Resolver.Resolve(ctx, actor, selection, peerRuntime, project.Root, dataDir)
	if err != nil {
		return Binding{}, nil, err
	}
	return p.provisionWithConfig(ctx, project, actor, spec, dataDir, cfg)
}

func (p *NativeProvisioner) provisionWithConfig(ctx context.Context, project Project, actor model.ActorID, spec BindingSpec, dataDir string, cfg agent.Config) (Binding, func(context.Context) error, error) {
	cfg.Actor = actor
	cfg.Runtime = cfg.Runtime.CanonicalForSlot(actor)
	if cfg.PeerRuntime == "" && actor == model.ActorClaude {
		cfg.PeerRuntime = p.cfg.Codex.Runtime.CanonicalForSlot(model.ActorCodex)
	} else if cfg.PeerRuntime == "" {
		cfg.PeerRuntime = p.cfg.Claude.Runtime.CanonicalForSlot(model.ActorClaude)
	}
	factory := agent.RedactingFactory(agent.FactoryFor(cfg.Runtime))
	cfg.Repo = project.Root
	cfg.DataDir = dataDir
	cfg.RoomName = "PairRoom binding validation"
	cfg.ClientVersion = version.Current
	cfg.SessionID = strings.TrimSpace(spec.SessionID)
	cfg.RequireExactSession = spec.Mode == BindingExisting
	cfg.SystemPrompt = prompt.BootstrapPromptWithRuntime(actor, cfg.Runtime, cfg.PeerRuntime)
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
