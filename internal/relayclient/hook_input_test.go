package relayclient

import (
	"encoding/json"
	"strings"
	"testing"
)

func grokHookJSON(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestGrokHookNormalizesOnlyCompletedMainSession(t *testing.T) {
	fields := map[string]any{"hookEventName": "stop", "hook_event_name": "Stop", "sessionId": "grok-session", "cwd": "/workspace/subdir", "workspaceRoot": "/workspace", "reason": "end_turn", "lastAssistantMessage": "完整回复 @codex", "stopHookActive": true}
	hook, err := decodeNativeHook(grokHookJSON(t, fields), true)
	if err != nil || hook.Event != "Stop" || hook.SessionID != "grok-session" || hook.CWD != "/workspace" || hook.LastAssistantMessage == nil || *hook.LastAssistantMessage != "完整回复 @codex" || !hook.StopHookActive || hook.Clipped || hook.TranscriptPath != "" {
		t.Fatalf("lost official fields: %+v %v", hook, err)
	}
	for _, change := range []map[string]any{
		{"reason": "shutdown"}, {"reason": "channel_closed"}, {"reason": ""},
		{"subagentType": "explore"},
		{"hookEventName": "stop_cancelled", "hook_event_name": "StopCancelled", "reason": "user_interrupt"},
		{"hookEventName": "subagent_stop", "hook_event_name": "SubagentStop"},
	} {
		copy := make(map[string]any, len(fields))
		for k, v := range fields {
			copy[k] = v
		}
		for k, v := range change {
			copy[k] = v
		}
		got, err := decodeNativeHook(grokHookJSON(t, copy), true)
		if err != nil || got.Event != "" {
			t.Fatalf("non-completion became a relay: %+v %+v %v", change, got, err)
		}
	}
	delete(fields, "workspaceRoot")
	hook, err = decodeNativeHook(grokHookJSON(t, fields), true)
	if err != nil || hook.CWD != "/workspace/subdir" {
		t.Fatalf("cwd fallback lost: %+v %v", hook, err)
	}
}

func TestGrokStopPayloadPromotesFromClaudeRuntimeDecode(t *testing.T) {
	data := grokHookJSON(t, map[string]any{
		"hookEventName": "stop", "hook_event_name": "Stop", "sessionId": "g", "cwd": "/w",
		"workspaceRoot": "/w", "reason": "end_turn", "lastAssistantMessage": "@claude shared",
	})
	hook, err := decodeNativeHook(data, false)
	if err != nil || hook.Event != "" {
		t.Fatalf("Claude decode should ignore Grok camelCase: %+v %v", hook, err)
	}
	hook, err = decodeNativeHook(data, true)
	if err != nil || hook.Event != "Stop" || hook.SessionID != "g" || hook.CWD != "/w" {
		t.Fatalf("Grok decode lost Stop: %+v %v", hook, err)
	}
}

func TestGrokHookRejectsConflictingIdentityAndForeignPayloads(t *testing.T) {
	fields := map[string]any{"hookEventName": "stop", "hook_event_name": "Stop", "sessionId": "grok", "cwd": "/w", "reason": "end_turn", "lastAssistantMessage": "text"}
	// Grok imports Claude hooks too; the compatibility invocation must be inert.
	hook, err := decodeNativeHook(grokHookJSON(t, fields), false)
	if err != nil || hook.Event != "" {
		t.Fatal("Grok payload entered a Claude/Codex binding")
	}
	for _, change := range []map[string]any{
		{"session_id": "different"}, {"session_id": 42},
		{"hook_event_name": "StopFailure"}, {"last_assistant_message": "different"},
		{"last_assistant_message": nil}, {"stopHookActive": "true"},
	} {
		copy := make(map[string]any, len(fields))
		for k, v := range fields {
			copy[k] = v
		}
		for k, v := range change {
			copy[k] = v
		}
		if _, err := decodeNativeHook(grokHookJSON(t, copy), true); err == nil {
			t.Fatalf("conflicting/invalid payload accepted: %+v", change)
		}
	}
	for _, data := range [][]byte{[]byte(`{"hook_event_name":"Stop","session_id":"claude","cwd":"/w"}`), []byte(`null`), []byte(`[]`), []byte(`{`), {0xff}} {
		if _, err := decodeNativeHook(data, true); err == nil {
			t.Fatalf("non-Grok payload accepted: %q", data)
		}
	}
}

func TestGrokHookClippingIsNotMistakenForACompleteReply(t *testing.T) {
	for _, tc := range []struct {
		text    string
		clipped bool
	}{
		{strings.Repeat("界", grokHookReplyLimit), false},
		{strings.Repeat("界", grokHookReplyLimit+1), true},
		{"prefix… [+17 chars]", true},
		{"prefix… [+17 chars]\n", true},
		{"a literal marker … [+17 chars] in the middle", false},
	} {
		data := grokHookJSON(t, map[string]any{"hookEventName": "stop", "sessionId": "g", "cwd": "/w", "reason": "end_turn", "lastAssistantMessage": tc.text})
		hook, err := decodeNativeHook(data, true)
		if err != nil || hook.Clipped != tc.clipped || hook.LastAssistantMessage == nil || *hook.LastAssistantMessage != tc.text {
			t.Fatalf("clipping classification changed content: %v %v", hook.Clipped, err)
		}
	}
}

func TestGrokStopFailureAndLegacyStopStayDistinct(t *testing.T) {
	hook, err := decodeNativeHook([]byte(`{"hookEventName":"stop_failure","hook_event_name":"StopFailure","sessionId":"g","cwd":"/w","error":"rate_limit","lastAssistantMessage":"private partial"}`), true)
	if err != nil || hook.Event != "StopFailure" || hook.Error != "rate_limit" {
		t.Fatalf("failure lost: %+v %v", hook, err)
	}
	legacy := []byte(`{"hook_event_name":"Stop","session_id":"c","cwd":"/w","last_assistant_message":"unchanged","stop_hook_active":true}`)
	hook, err = decodeNativeHook(legacy, false)
	if err != nil || hook.Event != "Stop" || hook.SessionID != "c" || !hook.StopHookActive || *hook.LastAssistantMessage != "unchanged" {
		t.Fatalf("legacy hook changed: %+v %v", hook, err)
	}
}
