package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestMain(m *testing.M) {
	if os.Getenv("PAIRROOM_CLAUDE_HELPER") == "1" {
		os.Exit(runClaudeStrictResumeHelper(os.Args[1:]))
	}
	if os.Getenv("PAIRROOM_CODEX_HELPER") == "1" {
		os.Exit(runCodexStrictResumeHelper(os.Args[1:]))
	}
	if os.Getenv("PAIRROOM_GROK_HELPER") == "1" {
		os.Exit(runGrokACPHelper(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runClaudeStrictResumeHelper(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("claude 2.1.231")
		return 0
	}
	if len(args) == 1 && args[0] == "--help" {
		fmt.Println("--input-format --output-format --session-id --verbose")
		return 0
	}
	return 97
}

func TestClaudeStrictResumeRejectsCLIWithoutResumeSupport(t *testing.T) {
	t.Setenv("PAIRROOM_CLAUDE_HELPER", "1")
	adapter := NewClaude(Config{
		Command: os.Args[0], Repo: t.TempDir(), DataDir: t.TempDir(),
		SessionID: "claude-required", RequireExactSession: true,
	}, func(model.RuntimeEvent) {})
	err := adapter.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot resume required session") {
		t.Fatalf("strict Claude resume error=%v", err)
	}
	if adapter.SessionID() != "claude-required" {
		t.Fatalf("strict resume replaced durable ID with %q", adapter.SessionID())
	}
}

func TestClaudeStrictResumeDoesNotRequestHistoricalUserReplay(t *testing.T) {
	flags := map[string]bool{
		"--include-partial-messages": true,
		"--replay-user-messages":     true,
		"--forward-subagent-text":    true,
		"--include-hook-events":      true,
	}
	strict := appendClaudeStreamFlags([]string{"-p"}, flags, true)
	for _, value := range strict {
		if value == "--replay-user-messages" {
			t.Fatalf("strict resume enabled transcript replay: %v", strict)
		}
	}
	for _, expected := range []string{"--include-partial-messages", "--forward-subagent-text", "--include-hook-events"} {
		if !containsString(strict, expected) {
			t.Fatalf("strict resume unexpectedly removed %s: %v", expected, strict)
		}
	}
	legacy := appendClaudeStreamFlags([]string{"-p"}, flags, false)
	if !containsString(legacy, "--replay-user-messages") {
		t.Fatalf("legacy single-Room behavior no longer enables supported replay: %v", legacy)
	}
}

func TestClaudeStrictResumeRejectsPreBindingApprovalRequest(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewClaude(Config{RequireExactSession: true}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.handleControlRequest([]byte(`{
		"request_id":"pre-binding-request",
		"request":{
			"subtype":"can_use_tool",
			"tool_name":"Bash",
			"input":{"command":"historical-secret"}
		}
	}`), claudePending{}, false)

	if len(adapter.approvals) != 0 {
		t.Fatalf("pre-binding Claude approval was retained: %#v", adapter.approvals)
	}
	for _, event := range events {
		if event.Kind == model.RuntimeApprovalRequested {
			t.Fatalf("pre-binding Claude approval was emitted: %#v", event)
		}
		if strings.Contains(event.Text, "historical-secret") || strings.Contains(string(event.Data), "historical-secret") {
			t.Fatalf("pre-binding Claude transcript leaked through diagnostic event: %#v", event)
		}
	}
}

func TestCodexStrictResumeRejectsMissingThread(t *testing.T) {
	t.Setenv("PAIRROOM_CODEX_HELPER", "1")
	t.Setenv("PAIRROOM_CODEX_HELPER_MODE", "resume-error")
	adapter := NewCodex(Config{
		Command: os.Args[0], Repo: t.TempDir(), SessionID: "thread-required", RequireExactSession: true,
	}, func(model.RuntimeEvent) {})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	err := adapter.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), `resume required Codex thread "thread-required"`) {
		t.Fatalf("strict Codex resume error=%v", err)
	}
	if adapter.SessionID() != "thread-required" {
		t.Fatalf("strict resume replaced durable thread with %q", adapter.SessionID())
	}
}

func TestCodexStrictResumeRejectsMismatchedThread(t *testing.T) {
	t.Setenv("PAIRROOM_CODEX_HELPER", "1")
	t.Setenv("PAIRROOM_CODEX_HELPER_MODE", "resume-mismatch")
	adapter := NewCodex(Config{
		Command: os.Args[0], Repo: t.TempDir(), SessionID: "thread-required", RequireExactSession: true,
	}, func(model.RuntimeEvent) {})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	err := adapter.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), `instead of required thread "thread-required"`) {
		t.Fatalf("strict Codex mismatch error=%v", err)
	}
}

func runCodexStrictResumeHelper(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex-cli 1.2.3")
		return 0
	}
	if len(args) >= 2 && args[0] == "app-server" && args[1] == "--help" {
		fmt.Println("Codex app-server")
		return 0
	}
	if len(args) == 0 || args[0] != "app-server" {
		return 97
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ThreadID string `json:"threadId"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			continue
		}
		if len(request.ID) == 0 || string(request.ID) == "null" {
			continue
		}
		var id any
		if err := json.Unmarshal(request.ID, &id); err != nil {
			continue
		}
		switch request.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{}})
		case "thread/resume":
			switch os.Getenv("PAIRROOM_CODEX_HELPER_MODE") {
			case "resume-error":
				_ = encoder.Encode(map[string]any{"id": id, "error": map[string]any{"code": -32000, "message": "thread not found"}})
			case "resume-mismatch":
				_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"thread": map[string]any{"id": "different-thread"}}})
			default:
				_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"thread": map[string]any{"id": request.Params.ThreadID}}})
			}
		default:
			_ = encoder.Encode(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": fmt.Sprintf("unsupported %s", request.Method)}})
		}
	}
	return 0
}

func runGrokACPHelper(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("grok 1.0.13")
		return 0
	}
	if len(args) == 1 && args[0] == "--help" {
		fmt.Println("agent --no-auto-update --model --reasoning-effort --always-approve --permission-mode --sandbox")
		return 0
	}
	if len(args) < 2 || args[len(args)-2] != "agent" || args[len(args)-1] != "stdio" {
		return 97
	}
	mode := os.Getenv("PAIRROOM_GROK_HELPER_MODE")
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	var promptID any
	sessionID := "grok-new-session"
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage        `json:"id"`
			Method string                 `json:"method"`
			Params map[string]any         `json:"params"`
			Result map[string]interface{} `json:"result"`
			Error  map[string]interface{} `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		var id any
		if len(request.ID) > 0 && string(request.ID) != "null" {
			_ = json.Unmarshal(request.ID, &id)
		}
		switch request.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": 1,
				"agentCapabilities": map[string]any{
					"loadSession": true, "promptCapabilities": map[string]any{"image": true},
					"sessionCapabilities": map[string]any{"close": map[string]any{}},
				},
				"authMethods": []any{},
			}})
		case "session/new":
			meta, _ := request.Params["_meta"].(map[string]any)
			_, hasMCP := request.Params["mcpServers"]
			if !hasMCP || !strings.Contains(fmt.Sprint(meta["rules"]), "pairroom-protocol/v5") {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32602, "message": "missing PairRoom session rules or mcpServers"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"sessionId": sessionID}})
		case "session/load":
			sessionID = fmt.Sprint(request.Params["sessionId"])
			if _, ok := request.Params["mcpServers"]; !ok {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32602, "message": "missing mcpServers"}})
				continue
			}
			if mode == "resume" {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "historical-secret"}}}})
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		case "session/set_mode", "session/close":
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		case "session/prompt":
			promptID = id
			promptText := ""
			if blocks, ok := request.Params["prompt"].([]any); ok && len(blocks) > 0 {
				if block, ok := blocks[0].(map[string]any); ok {
					promptText = fmt.Sprint(block["text"])
				}
			}
			if mode == "resume" {
				text := "bootstrap-missing"
				if strings.Contains(promptText, "pairroom-protocol/v5") {
					text = "bootstrap-present"
				}
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}}}})
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]any{"stopReason": "end_turn"}})
				promptID = nil
				continue
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "initial"}}}})
			if mode == "permission" {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{
					"sessionId": sessionID, "toolCall": map[string]any{"toolCallId": "tool-1", "title": "Run tests", "kind": "execute"},
					"options": []map[string]any{{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"}, {"optionId": "reject-once", "name": "Reject", "kind": "reject_once"}},
				}})
			}
		case "_x.ai/interject":
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"status": "queued"}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": " steered"}}}})
			if promptID != nil {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]any{"stopReason": "end_turn"}})
				promptID = nil
			}
		case "session/cancel":
			if promptID != nil {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]any{"stopReason": "cancelled"}})
				promptID = nil
			}
		case "":
			if fmt.Sprint(id) == "permission-1" && promptID != nil {
				outcome, _ := request.Result["outcome"].(map[string]any)
				if fmt.Sprint(outcome["outcome"]) != "selected" {
					continue
				}
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "approved"}}}})
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]any{"stopReason": "end_turn"}})
				promptID = nil
			}
		default:
			if id != nil {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "unsupported " + request.Method}})
			}
		}
	}
	return 0
}
