package main

import (
	"context"
	"os"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/model"
)

// --live is explicit consent for a fresh native session and possible model
// charges. Unlike the Management endpoint, CLI doctor uses the config file's
// two selections, not the Service-scoped default Agent pair profile.
func doctorLiveChecks(parent context.Context, cfg config.File, actor model.ActorID) []agent.DiagnosticCheck {
	failure := func(code string) []agent.DiagnosticCheck {
		return []agent.DiagnosticCheck{{ID: "selection", Actor: actor, Status: "fail", Code: code}}
	}
	ctx, cancel := context.WithTimeout(parent, 75*time.Second)
	defer cancel()
	resolver, err := configuredAgentResolver(cfg, false)
	if err != nil {
		return failure("provider_unavailable")
	}
	overlay, err := os.MkdirTemp("", "pairroom-doctor-provider-")
	if err != nil {
		return failure("workspace_unavailable")
	}
	defer os.RemoveAll(overlay)
	selections := resolver.DefaultSelections()
	native, err := resolver.Resolve(ctx, actor, selections[actor], selections[model.OtherParticipant(actor)].Runtime, overlay, overlay)
	if err != nil {
		return failure("provider_unavailable")
	}
	return agent.CheckRuntime(ctx, native)
}
