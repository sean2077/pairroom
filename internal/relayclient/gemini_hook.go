package relayclient

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Gemini exposes GEMINI_SESSION_ID to hooks, not to run_shell_command children.
// The approved BeforeTool hook forwards that official identity to a direct
// PairRoom invocation. This is a selector, not a credential or a new session.
const geminiSessionEnv = "PAIRROOM_GEMINI_SESSION_ID"

var geminiRelayCommand = regexp.MustCompile(`^[\t ]*pairroom(?:\.exe)?[\t ]+relay(?:[\t ]|$)`)

// decodeGeminiHook normalizes only the documented AfterAgent boundary. A
// non-nil immediate result belongs to BeforeTool and must be returned without
// workspace discovery, disk writes, or Service contact. No transcript is read.
func decodeGeminiHook(data []byte, windows bool) (HookInput, map[string]any, error) {
	if !utf8.Valid(data) {
		return HookInput{}, nil, errors.New("invalid UTF-8 in Gemini hook input")
	}
	var wire struct {
		Event      string                     `json:"hook_event_name"`
		Session    string                     `json:"session_id"`
		CWD        string                     `json:"cwd"`
		Transcript string                     `json:"transcript_path"`
		Response   *string                    `json:"prompt_response"`
		Active     bool                       `json:"stop_hook_active"`
		Tool       string                     `json:"tool_name"`
		Input      map[string]json.RawMessage `json:"tool_input"`
	}
	// A JSON null is not an official hook object.
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil || json.Unmarshal(data, &wire) != nil {
		return HookInput{}, nil, errors.New("invalid Gemini hook input")
	}
	switch wire.Event {
	case "BeforeTool":
		result := map[string]any{}
		if wire.Tool != "run_shell_command" {
			return HookInput{}, result, nil
		}
		var command string
		if json.Unmarshal(wire.Input["command"], &command) != nil || !geminiRelayCommand.MatchString(command) {
			return HookInput{}, result, nil
		}
		if wire.Session == "" || len(wire.Session) > 256 || strings.TrimSpace(wire.Session) != wire.Session || strings.ContainsAny(wire.Session, "/\\") || strings.ContainsFunc(wire.Session, unicode.IsControl) {
			return HookInput{}, nil, errors.New("invalid official Gemini session_id; refusing to forward session metadata")
		}
		// Do not interpolate any model-generated argument. Only a shell-quoted
		// official session ID is added; all original tool arguments are retained
		// by Gemini's documented tool_input merge. No permission decision is set.
		prefix := geminiSessionEnv + "='" + strings.ReplaceAll(wire.Session, "'", "'\\''") + "' "
		if windows {
			prefix = "$env:" + geminiSessionEnv + "='" + strings.ReplaceAll(wire.Session, "'", "''") + "'; "
		}
		result["hookSpecificOutput"] = map[string]any{
			"hookEventName": "BeforeTool",
			"tool_input":    map[string]string{"command": prefix + strings.TrimLeft(command, "\t ")},
		}
		return HookInput{}, result, nil
	case "AfterAgent":
		return HookInput{
			Event: "Stop", SessionID: wire.Session, CWD: wire.CWD,
			TranscriptPath: wire.Transcript, LastAssistantMessage: wire.Response,
			StopHookActive: wire.Active,
		}, nil, nil
	default:
		// SessionEnd, cancellation, and other vendors' Stop events do not
		// publish replies, associate a session, or request a continuation.
		return HookInput{}, nil, nil
	}
}
