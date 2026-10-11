package relayclient

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/relay"
)

// Resolve existing per-session routes without falling back to any Service.
// This is a read-only locator check; it never repairs or creates an index.
func preflightBoundState(ctx context.Context, o options, root string) (*State, error) {
	caller, err := currentNativeCaller()
	if err != nil || caller.session == "" {
		return nil, nil // the caller report explains invalid/missing metadata
	}
	states, err := indexedSessions(caller)
	if err != nil {
		return nil, err
	}
	if len(states) == 0 && root != "" {
		states, err = matchingSessions(root, caller)
		if err != nil {
			return nil, err
		}
	}
	if len(states) == 0 {
		meta, err := directSessionMetadata(ctx, caller)
		if err != nil || meta == nil {
			return nil, err
		}
		if _, err := selectDirectWorkspace(ctx, *meta, "preflight", &o); err != nil {
			return nil, err
		}
		return directMetadataState(*meta)
	}
	if _, err := selectSessionWorkspace(ctx, states, "preflight", &o); err != nil {
		return nil, err
	}
	for _, state := range states {
		if state.Room == o.room && string(state.Slot) == o.slot {
			return &state, nil
		}
	}
	return nil, errors.New("native session binding changed during preflight")
}

func preflightLANState(ctx context.Context, state State) preflightService {
	result := preflightService{Status: checkFail, Transport: "lan_direct"}
	dir := filepath.Join(state.Workspace, ".pairroom", "rooms", state.Room, "slots", string(state.Slot))
	client, err := load(dir)
	if err != nil {
		result.Hint = "The direct LAN binding's private client record is unavailable or changed. Restore its original client state before reconnecting; a local Service is not required."
		return result
	}
	defer client.close()
	var summary relay.Summary
	if err := client.call(ctx, "summary", nil, &summary); err != nil {
		result.Hint = "The Room host did not accept the direct connection. Check the host's reachability and admission with its owner; starting a local Service does not repair this binding."
		return result
	}
	result.Status = checkPass
	return result
}
