package relayclient

// Gemini's hook timeout is milliseconds (other supported hosts use seconds).
// Keep both events named so users can inspect/disable them in /hooks panel.
func geminiHookGroup(event string) map[string]any {
	name := "pairroom-relay-after-agent"
	group := map[string]any{}
	if event == "BeforeTool" {
		name = "pairroom-relay-before-tool"
		group["matcher"] = "^run_shell_command$"
	}
	group["hooks"] = []any{map[string]any{
		"type": "command", "name": name,
		"command": "pairroom relay hook --runtime gemini", "timeout": 45000,
	}}
	return group
}

// Read-only project evidence, not proof of user/system approval. Both hooks are
// necessary: AfterAgent alone cannot supply identity to foreground shell tools.
func geminiHooksPresent(config map[string]any) (bool, bool, error) {
	control, _ := config["hooksConfig"].(map[string]any)
	if enabled, ok := control["enabled"].(bool); ok && !enabled {
		return false, true, nil
	}
	disabled, _ := control["disabled"].([]any)
	hooks, _ := config["hooks"].(map[string]any)
	found := 0
	wasDisabled := false
	for _, event := range []string{"BeforeTool", "AfterAgent"} {
		groups, _ := hooks[event].([]any)
		present := false
		for _, raw := range groups {
			group, _ := raw.(map[string]any)
			matcher, _ := group["matcher"].(string)
			if matcher != "" && matcher != "*" && !(event == "BeforeTool" && (matcher == "^run_shell_command$" || matcher == "run_shell_command")) {
				continue
			}
			entries, _ := group["hooks"].([]any)
			for _, rawEntry := range entries {
				entry, _ := rawEntry.(map[string]any)
				if entry["type"] != "command" || entry["command"] != "pairroom relay hook --runtime gemini" {
					continue
				}
				inactive := false
				for _, rawName := range disabled {
					name, ok := rawName.(string)
					if ok && (name == entry["name"] || name == entry["command"]) {
						inactive = true
					}
				}
				wasDisabled = wasDisabled || inactive
				async, _ := entry["async"].(bool)
				timeout, numeric := entry["timeout"].(float64)
				// Absence uses Gemini's documented 60000 ms default.
				if !inactive && !async && (entry["timeout"] == nil || numeric && timeout >= 45000) {
					present = true
				}
			}
		}
		if present {
			found++
		}
	}
	return found == 2, wasDisabled && found != 2, nil
}
