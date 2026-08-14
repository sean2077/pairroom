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
