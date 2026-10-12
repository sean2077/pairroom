package relayclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func requireSessionID(kind model.RuntimeKind) (string, error) {
	if kind == model.RuntimeGemini {
		caller, err := currentNativeCaller()
		if err == nil && caller.runtime == kind && caller.session != "" {
			return caller.session, nil
		}
		return "", errors.New("Gemini BeforeTool session metadata is missing or stale; install --runtime gemini, approve/reload its hooks, then run bind as a standalone run_shell_command tool call in Gemini CLI")
	}
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

// New Native Rooms have two untyped seats. Verify this tool caller and its
// installation before provisioning; the Service records its runtime at bind.
// Managed default pairs and installed peer binaries cannot choose a runtime.
func prepareNativeCreation(ctx context.Context, endpoint relay.Endpoint, root string, o options, slot model.ActorID) (options, model.ActorID, error) {
	if o.peer != "" {
		return o, slot, errors.New("native Room runtimes are identified when each agent joins; omit --peer-runtime")
	}
	caller := callerRuntime(o)
	if _, err := requireSessionID(caller); err != nil {
		return o, slot, err
	}
	if err := installed(root, caller); err != nil {
		return o, slot, err
	}
	if slot == "" {
		slot = model.ActorSlot1
	}
	o.slot = string(slot)
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
