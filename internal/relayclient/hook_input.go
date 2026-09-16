package relayclient

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// HookInput is normalized only at the supported native hook boundary. It is not
// session discovery, and never reads a vendor transcript.
type HookInput struct {
	Event                string  `json:"hook_event_name"`
	SessionID            string  `json:"session_id"`
	CWD                  string  `json:"cwd"`
	TranscriptPath       string  `json:"transcript_path"`
	LastAssistantMessage *string `json:"last_assistant_message"`
	StopHookActive       bool    `json:"stop_hook_active"`
	Error                string  `json:"error"`
	Clipped              bool    `json:"-"`
}

const grokHookReplyLimit = 32768

var grokClipMarker = regexp.MustCompile(`… \[\+[0-9]+ chars\]\s*$`)

// Grok's file hooks use camelCase, except the optional hook_event_name alias.
// Its compatible Claude hook sources can invoke our Claude command too: ignore
// that foreign payload rather than treating a Grok session as Claude. Conversely,
// --runtime grok never turns a Claude/Codex payload into a Grok session.
func decodeNativeHook(data []byte, grok bool) (HookInput, error) {
	if !utf8.Valid(data) {
		return HookInput{}, errors.New("invalid UTF-8 in official hook input")
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || raw == nil {
		return HookInput{}, errors.New("invalid official hook input")
	}
	_, camelEvent := raw["hookEventName"]
	_, camelSession := raw["sessionId"]
	if !grok {
		if camelEvent || camelSession {
			return HookInput{}, nil
		}
		var hook HookInput
		if json.Unmarshal(data, &hook) != nil {
			return HookInput{}, errors.New("invalid official hook input")
		}
		return hook, nil
	}
	if !camelEvent || !camelSession {
		return HookInput{}, errors.New("Grok file hook requires hookEventName and sessionId; no identity fallback")
	}
	var wire struct {
		Event      string  `json:"hookEventName"`
		EventAlias string  `json:"hook_event_name"`
		Session    string  `json:"sessionId"`
		CWD        string  `json:"cwd"`
		Workspace  string  `json:"workspaceRoot"`
		Text       *string `json:"lastAssistantMessage"`
		Active     bool    `json:"stopHookActive"`
		Reason     string  `json:"reason"`
		Subagent   string  `json:"subagentType"`
		Error      string  `json:"error"`
	}
	if json.Unmarshal(data, &wire) != nil {
		return HookInput{}, errors.New("invalid Grok hook fields")
	}
	events := map[string]string{"stop": "Stop", "stop_failure": "StopFailure", "stop_cancelled": "StopCancelled"}
	event := events[wire.Event]
	if event == "" || event == "StopCancelled" || wire.Subagent != "" {
		return HookInput{}, nil
	}
	if wire.EventAlias != "" && wire.EventAlias != event {
		return HookInput{}, errors.New("conflicting Grok hook event names")
	}
	// Session-end Stop is observe-only; it cannot associate, publish a second
	// copy or request a continuation. Never reopen cancelled/subagent turns.
	if event == "Stop" && wire.Reason != "end_turn" {
		return HookInput{}, nil
	}
	// Reject competing snake_case identity/text instead of choosing one.
	if value, ok := raw["session_id"]; ok {
		var got string
		if json.Unmarshal(value, &got) != nil || got != wire.Session {
			return HookInput{}, errors.New("conflicting Grok hook session metadata")
		}
	}
	if value, ok := raw["last_assistant_message"]; ok {
		var text *string
		if json.Unmarshal(value, &text) != nil || (text == nil) != (wire.Text == nil) || text != nil && *text != *wire.Text {
			return HookInput{}, errors.New("conflicting Grok hook reply fields")
		}
	}
	root := wire.Workspace
	if root == "" {
		root = wire.CWD
	}
	hook := HookInput{Event: event, SessionID: wire.Session, CWD: root, LastAssistantMessage: wire.Text, StopHookActive: wire.Active, Error: wire.Error}
	if wire.Text != nil {
		hook.Clipped = utf8.RuneCountInString(*wire.Text) > grokHookReplyLimit || grokClipMarker.MatchString(strings.TrimSpace(*wire.Text))
	}
	return hook, nil
}
