package relayclient

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGeminiAfterAgentUsesOfficialReply(t *testing.T) {
	data := []byte(`{"hook_event_name":"AfterAgent","session_id":"gemini-1","cwd":"/repo","transcript_path":"/never/read.json","prompt":"not the reply","prompt_response":"@codex please review\n完整回复","last_assistant_message":"wrong vendor field","stop_hook_active":true}`)
	hook, immediate, err := decodeGeminiHook(data, false)
	if err != nil || immediate != nil || hook.Event != "Stop" || hook.SessionID != "gemini-1" || hook.CWD != "/repo" || hook.TranscriptPath != "/never/read.json" || !hook.StopHookActive || hook.LastAssistantMessage == nil || *hook.LastAssistantMessage != "@codex please review\n完整回复" {
		t.Fatalf("hook=%+v immediate=%v err=%v", hook, immediate, err)
	}
}

func TestGeminiHookDoesNotBorrowOtherEventsOrReplies(t *testing.T) {
	for _, event := range []string{"Stop", "StopFailure", "SessionStart", "SessionEnd", "AfterModel", "stop", ""} {
		t.Run(event, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"hook_event_name": event, "session_id": "g", "cwd": "/repo", "prompt_response": "@codex not a completed turn"})
			if err != nil {
				t.Fatal(err)
			}
			hook, immediate, err := decodeGeminiHook(data, false)
			if err != nil || immediate != nil || hook.Event != "" || hook.LastAssistantMessage != nil {
				t.Fatalf("unsupported event produced an effect: %+v %v %v", hook, immediate, err)
			}
		})
	}
	hook, _, err := decodeGeminiHook([]byte(`{"hook_event_name":"AfterAgent","session_id":"g","cwd":"/repo","last_assistant_message":"must not be used"}`), false)
	if err != nil || hook.LastAssistantMessage != nil {
		t.Fatalf("missing prompt_response must remain missing: %+v %v", hook, err)
	}
}

func TestGeminiBeforeToolBridgesOnlyDirectRelayCommands(t *testing.T) {
	for _, tc := range []struct {
		name, command, want string
		windows             bool
	}{
		{"posix", `pairroom relay bind --create --name "review"`, `PAIRROOM_GEMINI_SESSION_ID='gemini-1' pairroom relay bind --create --name "review"`, false},
		{"windows", `pairroom.exe relay wait --timeout 0`, `$env:PAIRROOM_GEMINI_SESSION_ID='gemini-1'; pairroom.exe relay wait --timeout 0`, true},
		{"indent", "\t pairroom relay status", `PAIRROOM_GEMINI_SESSION_ID='gemini-1' pairroom relay status`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"hook_event_name": "BeforeTool", "session_id": "gemini-1", "tool_name": "run_shell_command", "tool_input": map[string]any{"command": tc.command, "dir_path": "/elsewhere", "is_background": true}})
			if err != nil {
				t.Fatal(err)
			}
			hook, result, err := decodeGeminiHook(data, tc.windows)
			if err != nil || hook.Event != "" || result == nil {
				t.Fatalf("hook=%+v result=%v err=%v", hook, result, err)
			}
			specific := result["hookSpecificOutput"].(map[string]any)
			input := specific["tool_input"].(map[string]string)
			if len(result) != 1 || len(input) != 1 || specific["hookEventName"] != "BeforeTool" || input["command"] != tc.want {
				t.Fatalf("unexpected tool override: %v", result)
			}
		})
	}
	for _, command := range []string{"go test ./...", "echo pairroom relay bind", "pairroom relayx", "other-pairroom relay bind", "cd repo && pairroom relay bind", "\npairroom relay bind", "pairroom service start", ""} {
		data, _ := json.Marshal(map[string]any{"hook_event_name": "BeforeTool", "session_id": "", "tool_name": "run_shell_command", "tool_input": map[string]string{"command": command}})
		hook, result, err := decodeGeminiHook(data, false)
		if err != nil || hook.Event != "" || result == nil || len(result) != 0 {
			t.Fatalf("unrelated command %q must be inert: %+v %v %v", command, hook, result, err)
		}
	}
	_, result, err := decodeGeminiHook([]byte(`{"hook_event_name":"BeforeTool","tool_name":"read_file","tool_input":{"command":"pairroom relay bind"}}`), false)
	if err != nil || result == nil || len(result) != 0 {
		t.Fatalf("non-shell tool must be inert: %v %v", result, err)
	}
}

func TestGeminiBeforeToolRejectsUnsafeMetadata(t *testing.T) {
	for _, session := range []string{"", " g", "g ", "../g", "g\\h", "g\nx", "g\x00x", strings.Repeat("g", 257)} {
		data, _ := json.Marshal(map[string]any{"hook_event_name": "BeforeTool", "session_id": session, "tool_name": "run_shell_command", "tool_input": map[string]string{"command": "pairroom relay bind"}})
		_, result, err := decodeGeminiHook(data, false)
		if err == nil || result != nil {
			t.Fatalf("unsafe session %q forwarded: %v %v", session, result, err)
		}
	}
	for _, data := range [][]byte{[]byte(`null`), []byte(`[]`), []byte(`{`), []byte(`{"hook_event_name":12}`), {0xff}} {
		if _, _, err := decodeGeminiHook(data, false); err == nil {
			t.Fatalf("invalid hook accepted: %q", data)
		}
	}
}

func TestGeminiBeforeToolQuotesSessionRatherThanExecutingIt(t *testing.T) {
	data := []byte(`{"hook_event_name":"BeforeTool","session_id":"g'$(whoami);x","tool_name":"run_shell_command","tool_input":{"command":"pairroom relay status"}}`)
	for _, tc := range []struct {
		windows bool
		want    string
	}{
		{false, `PAIRROOM_GEMINI_SESSION_ID='g'\''$(whoami);x' pairroom relay status`},
		{true, `$env:PAIRROOM_GEMINI_SESSION_ID='g''$(whoami);x'; pairroom relay status`},
	} {
		_, result, err := decodeGeminiHook(data, tc.windows)
		if err != nil {
			t.Fatal(err)
		}
		got := result["hookSpecificOutput"].(map[string]any)["tool_input"].(map[string]string)["command"]
		if got != tc.want {
			t.Fatalf("windows=%v command=%q want=%q", tc.windows, got, tc.want)
		}
	}
}
