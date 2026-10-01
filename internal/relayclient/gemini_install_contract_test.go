package relayclient

import (
	"encoding/json"
	"testing"
)

func TestGeminiReadinessNeedsBothEnabledHooks(t *testing.T) {
	for _, tc := range []struct {
		name, edit        string
		present, disabled bool
	}{
		{"ready", "", true, false},
		{"disabled-global", "global", false, true},
		{"disabled-named", "named", false, true},
		{"disabled-command", "command", false, true},
		{"missing-before", "missing", false, false},
		{"seconds-not-ms", "seconds", false, false},
		{"unmatched-tool", "matcher", false, false},
		{"async", "async", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := map[string]any{"hooks": map[string]any{"BeforeTool": []any{geminiHookGroup("BeforeTool")}, "AfterAgent": []any{geminiHookGroup("AfterAgent")}}}
			// Match JSON's numeric types, as the real settings reader does.
			data, _ := json.Marshal(config)
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			hooks := config["hooks"].(map[string]any)
			group := hooks["BeforeTool"].([]any)[0].(map[string]any)
			entry := group["hooks"].([]any)[0].(map[string]any)
			switch tc.edit {
			case "global":
				config["hooksConfig"] = map[string]any{"enabled": false}
			case "named":
				config["hooksConfig"] = map[string]any{"disabled": []any{"pairroom-relay-before-tool"}}
			case "command":
				config["hooksConfig"] = map[string]any{"disabled": []any{"pairroom relay hook --runtime gemini"}}
			case "missing":
				delete(hooks, "BeforeTool")
			case "seconds":
				entry["timeout"] = float64(45)
			case "matcher":
				group["matcher"] = "write_file"
			case "async":
				entry["async"] = true
			}
			present, disabled, err := geminiHooksPresent(config)
			if err != nil || present != tc.present || disabled != tc.disabled {
				t.Fatalf("readiness: %v %v %v", present, disabled, err)
			}
		})
	}
}
