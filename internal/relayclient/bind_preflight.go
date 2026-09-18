package relayclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

type defaultPairRuntimeMismatchError struct {
	Runtime model.RuntimeKind
}

func (e *defaultPairRuntimeMismatchError) Error() string {
	return fmt.Sprintf("the %q harness matches no slot in the Service default pair", e.Runtime)
}

func requireSessionID(kind model.RuntimeKind) (string, error) {
	name, ok := sessionEnvVars[kind]
	if !ok {
		return "", errors.New("run bind as a tool call inside your native session; use --runtime when the harness cannot be identified")
	}
	id := sessionIDFromEnv(kind)
	if id == "" {
		if kind == model.RuntimeGrok {
			return "", fmt.Errorf("%s is missing; run bind as an agent tool call inside your native grok session, not a plain terminal or Grok shell mode (!)", name)
		}
		return "", fmt.Errorf("%s is missing; run bind as a tool call inside your native %s session, not a plain terminal", name, kind)
	}
	if len(id) > 256 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "/\\") || strings.ContainsFunc(id, unicode.IsControl) {
		return "", fmt.Errorf("%s is not a valid session identity; rerun inside the intended native session", name)
	}
	return id, nil
}

// Resolve and pin the actual Service-owned defaults before any provisioning.
// Both catalog endpoints are read-only; the creation endpoint still owns full
// validation, including Provider revalidation. No Registry schema changes.
func prepareNativeCreation(ctx context.Context, endpoint relay.Endpoint, root string, o options, slot model.ActorID) (options, model.ActorID, error) {
	caller := callerRuntime(o)
	if _, err := requireSessionID(caller); err != nil {
		return o, slot, err
	}
	if err := installed(root, caller); err != nil {
		return o, slot, err
	}
	if slot == "" && (o.kind != "" || o.peer != "") {
		var err error
		slot, err = inferCreateSlot(o)
		if err != nil {
			return o, slot, err
		}
	}
	agents, err := createAgents(o, slot)
	if err != nil {
		return o, slot, err
	}
	usingServiceDefaults := agents == nil
	if agents == nil {
		agents, err = readDefaultPair(ctx, endpoint)
		if err != nil {
			return o, slot, err
		}
	}
	if len(agents) != 2 {
		return o, slot, errors.New("Service defaults must contain both Agent selections")
	}
	for _, actor := range model.SlotActors() {
		selection, ok := agents[actor]
		if !ok {
			return o, slot, errors.New("Service defaults are missing an Agent selection")
		}
		selection = selection.Normalized(actor)
		if err := selection.Validate(actor); err != nil {
			return o, slot, err
		}
		if _, supported := sessionEnvVars[selection.Runtime]; !supported {
			return o, slot, fmt.Errorf("the default pair includes %s, which does not support native hosting", selection.Runtime)
		}
		agents[actor] = selection
	}
	if usingServiceDefaults && len(runtimeSlots(agents, caller)) == 0 {
		return o, slot, &defaultPairRuntimeMismatchError{Runtime: caller}
	}
	if slot == "" {
		slot, err = resolveSlotForRoom(serviceRoom{Agents: agents}, caller)
		if err != nil {
			return o, slot, err
		}
	}
	if agents[slot].Runtime != caller {
		return o, slot, errors.New("the selected creator slot does not match this session's runtime; choose a matching slot or configure the intended pair")
	}
	o.slot = string(slot)
	o.preparedAgents = agents
	return o, slot, nil
}

func runtimeSlots(agents map[model.ActorID]model.AgentSelection, runtime model.RuntimeKind) []model.ActorID {
	var matches []model.ActorID
	for _, slot := range model.SlotActors() {
		if selection, ok := agents[slot]; ok && selection.Runtime == runtime {
			matches = append(matches, slot)
		}
	}
	return matches
}

func readDefaultPair(ctx context.Context, endpoint relay.Endpoint) (map[model.ActorID]model.AgentSelection, error) {
	var profiles struct {
		DefaultProfileID string `json:"default_profile_id"`
		Profiles         []struct {
			ID     string                                 `json:"id"`
			Agents map[model.ActorID]model.AgentSelection `json:"agents"`
		} `json:"profiles"`
	}
	if err := management(ctx, endpoint, http.MethodGet, "/api/v1/agent-pair-profiles", nil, &profiles); err != nil {
		return nil, err
	}
	if profiles.DefaultProfileID != "" {
		for _, profile := range profiles.Profiles {
			if profile.ID == profiles.DefaultProfileID {
				return profile.Agents, nil
			}
		}
		return nil, errors.New("Service default Agent pair profile is missing")
	}
	var catalog struct {
		Defaults map[model.ActorID]model.AgentSelection `json:"defaults"`
	}
	if err := management(ctx, endpoint, http.MethodGet, "/api/v1/agent-catalog", nil, &catalog); err != nil {
		return nil, err
	}
	return catalog.Defaults, nil
}
