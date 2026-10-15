package projectsetup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

func command(kind string) string { return "pairroom relay hook --runtime " + kind }

func decodeHooks(data []byte) (map[string]any, error) {
	config := map[string]any{}
	if len(data) == 0 {
		return config, nil
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber() // Preserve unrelated large numeric settings exactly.
	if err := decoder.Decode(&config); err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("hook configuration must be an object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("extra hook configuration data")
	}
	return config, nil
}

func hasRelayCommand(data []byte) bool {
	// Inspect decoded command values, not raw JSON: escaped commands must not
	// bypass the shared-hook duplicate guard. Wrappers also require review.
	config, err := decodeHooks(data)
	if err != nil {
		return false
	}
	var contains func(any) bool
	contains = func(value any) bool {
		switch value := value.(type) {
		case map[string]any:
			if text, ok := value["command"].(string); ok && strings.Contains(text, "pairroom relay hook") {
				return true
			}
			for _, child := range value {
				if contains(child) {
					return true
				}
			}
		case []any:
			for _, child := range value {
				if contains(child) {
					return true
				}
			}
		}
		return false
	}
	return contains(config["hooks"])
}

func planHooks(before fileState, kind string) ([]byte, string, string, error) {
	if before.exists && len(bytes.TrimSpace(before.data)) == 0 {
		return nil, "blocked", "invalid_hooks", errors.New("empty hook configuration")
	}
	config, err := decodeHooks(before.data)
	if err != nil {
		return nil, "blocked", "invalid_hooks", err
	}
	if config["disableAllHooks"] == true {
		return nil, "disabled", "hooks_disabled", nil
	}
	hooks, ok := config["hooks"].(map[string]any)
	if !ok {
		if _, exists := config["hooks"]; exists {
			return nil, "blocked", "invalid_hooks", errors.New("hooks must be an object")
		}
		hooks = map[string]any{}
	}
	if kind == "gemini" {
		tools, _ := config["tools"].(map[string]any)
		if hooks["enabled"] == false || tools["enableHooks"] == false {
			return nil, "disabled", "hooks_disabled", nil
		}
	}
	events := []string{"Stop"}
	if kind == "claude" || kind == "grok" {
		events = append(events, "StopFailure")
	}
	timeout := 45
	if kind == "gemini" {
		events, timeout = []string{"BeforeTool", "AfterAgent"}, 45000
	}
	current := true
	found := false
	for _, event := range events {
		groups, ok := hooks[event].([]any)
		if !ok {
			if _, exists := hooks[event]; exists {
				return nil, "blocked", "invalid_hooks", errors.New("hook groups must be an array")
			}
		}
		kept := []any{}
		count, ready := 0, false
		for _, raw := range groups {
			group, ok := raw.(map[string]any)
			if !ok {
				return nil, "blocked", "invalid_hooks", errors.New("hook group must be an object")
			}
			entries, ok := group["hooks"].([]any)
			if !ok {
				return nil, "blocked", "invalid_hooks", errors.New("hook entries must be an array")
			}
			rest := []any{}
			removed := false
			for _, rawEntry := range entries {
				entry, ok := rawEntry.(map[string]any)
				if !ok {
					return nil, "blocked", "invalid_hooks", errors.New("hook entry must be an object")
				}
				text, _ := entry["command"].(string)
				if text != command(kind) {
					// A wrapper/modified relay command is user-owned. Do not append a second
					// publication hook or infer ownership from a substring.
					if strings.Contains(text, "pairroom relay hook") {
						return nil, "blocked", "custom_hook", nil
					}
					rest = append(rest, entry)
					continue
				}
				count++
				removed = true
				found = true
				number, _ := entry["timeout"].(json.Number)
				seconds, _ := number.Float64()
				matcher, _ := group["matcher"].(string)
				matches := matcher == "" || matcher == "*"
				if kind == "gemini" && event == "BeforeTool" {
					matches = matcher == "run_shell_command" || matcher == "*" || matcher == ""
				}
				ready = entry["type"] == "command" && entry["async"] != true && entry["enabled"] != false && group["enabled"] != false && seconds >= float64(timeout) && matches
			}
			if len(rest) > 0 || !removed {
				group["hooks"] = rest
				kept = append(kept, group)
			}
		}
		if count != 1 || !ready {
			current = false
		}
		group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command(kind), "timeout": timeout}}}
		if kind == "gemini" && event == "BeforeTool" {
			group["matcher"] = "run_shell_command"
		}
		hooks[event] = append(kept, group)
	}
	if current {
		return nil, "current", "", nil
	}
	config["hooks"] = hooks
	data, err := json.MarshalIndent(config, "", "  ")
	state := "missing"
	if found {
		state = "update_required"
	}
	return append(data, '\n'), state, "", err
}

// Reject ambiguous JSON rather than silently losing an earlier user value
// during the merge. This validates syntax as well as nested duplicate keys.
func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 100 {
			return errors.New("hook configuration is too deeply nested")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || seen[key] {
					return errors.New("duplicate hook configuration key")
				}
				seen[key] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid hook configuration delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	return value(0)
}
