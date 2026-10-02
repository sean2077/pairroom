package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// These are vendor-shaped protocol fixtures, not authenticated Gemini E2E.
func runGeminiACPHelper(args []string) int {
	mode := os.Getenv("PAIRROOM_GEMINI_MODE")
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("0.62.0")
		return 0
	}
	if len(args) == 1 && args[0] == "--help" {
		switch mode {
		case "acp-prefix":
			fmt.Println("--acp-debug --experimental-acp-debug --model")
		case "no-acp":
			fmt.Println("--model")
		case "legacy":
			fmt.Println("--experimental-acp --model --sandbox --approval-mode")
		default:
			fmt.Println("--acp --model --sandbox --approval-mode")
		}
		return 0
	}
	if len(args) == 0 || (args[0] != "--acp" && args[0] != "--experimental-acp") {
		return 97
	}
	if expected, ok := os.LookupEnv("PAIRROOM_GEMINI_EXPECT_SANDBOX_ENV"); ok && os.Getenv("GEMINI_SANDBOX") != expected {
		fmt.Fprintln(os.Stderr, "unexpected Gemini sandbox environment")
		return 86
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 65536), 4<<20)
	emit := func(value any) { _ = encoder.Encode(value) }
	reply := func(id any, result any) { emit(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
	session, prompts := "gemini-new-session", 0
	var active any
	update := func(kind string, fields map[string]any) {
		fields["sessionUpdate"] = kind
		emit(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": fields}})
	}
	text := func(value string) {
		update("agent_message_chunk", map[string]any{"content": map[string]any{"type": "text", "text": value}})
	}
	finish := func(reason string) {
		if active != nil {
			reply(active, map[string]any{"stopReason": reason})
			active = nil
		}
	}
	for scanner.Scan() {
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
			Result map[string]any `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			return 96
		}
		if _, foreign := req.Params["_meta"]; foreign {
			return 95
		}
		switch req.Method {
		case "initialize":
			version := 1
			if mode == "wrong-version" {
				version = 2
			}
			reply(req.ID, map[string]any{"protocolVersion": version, "agentInfo": map[string]any{"name": "gemini-cli", "version": "0.62.0"}, "authMethods": []any{map[string]any{"id": "oauth-personal"}, map[string]any{"id": "gemini-api-key"}}, "agentCapabilities": map[string]any{"loadSession": mode != "no-load", "promptCapabilities": map[string]any{"image": true}}})
		case "authenticate":
			return 94 // Must not change Gemini's persisted selected auth method.
		case "session/new":
			if mode == "setup-hang" {
				time.Sleep(time.Minute)
			}
			if mode == "resume" || mode == "wrong-session" {
				return 93
			}
			reply(req.ID, map[string]any{"sessionId": session, "modes": map[string]any{"currentModeId": "auto_edit"}})
		case "session/load":
			session, _ = req.Params["sessionId"].(string)
			if mode != "late-replay" {
				text("PRIVATE_REPLAY")
			}
			if mode == "wrong-session" {
				reply(req.ID, map[string]any{"sessionId": "different-session"})
				continue
			}
			reply(req.ID, map[string]any{"modes": map[string]any{"currentModeId": "auto_edit"}}) // Gemini load omits sessionId.
		case "session/set_mode":
			expected := os.Getenv("PAIRROOM_GEMINI_EXPECT_MODE")
			if expected == "" {
				expected = "auto_edit"
			}
			if req.Params["modeId"] != expected {
				return 92
			}
			reply(req.ID, map[string]any{})
		case "session/prompt":
			prompts++
			active = req.ID
			if mode == "late-replay" {
				// Gemini v0.62.0 AcpSessionManager.loadSession does not await
				// streamHistory. With stdout backpressure, replay continues even
				// after session/set_mode and the next prompt have crossed the wire.
				text("PRIVATE_REPLAY @codex do the old task\n")
			}
			blocks, _ := req.Params["prompt"].([]any)
			if len(blocks) == 0 {
				return 91
			}
			block, _ := blocks[0].(map[string]any)
			body, _ := block["text"].(string)
			bootstraps := strings.Count(body, "GEMINI-BOOTSTRAP")
			if (prompts == 1 && bootstraps != 1) || (prompts > 1 && bootstraps != 0) || !strings.Contains(body, "BODY 🌟") {
				return 90
			}
			if mode == "stderr-overflow" {
				fmt.Fprint(os.Stderr, strings.Repeat("x", 2<<20))
				return 89
			}
			if mode == "exit" {
				return 89
			}
			update("agent_thought_chunk", map[string]any{"content": map[string]any{"type": "text", "text": "PRIVATE_THOUGHT"}})
			text(fmt.Sprintf("Gemini answer %d", prompts))
			update("tool_call", map[string]any{"toolCallId": "tool-1", "title": "Run tests", "status": "pending"})
			update("tool_call_update", map[string]any{"toolCallId": "tool-1", "status": "in_progress"})
			if mode == "permission" || mode == "cancel" {
				emit(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{"sessionId": session, "toolCall": map[string]any{"toolCallId": "tool-1", "title": "Run tests", "kind": "execute"}, "options": []any{map[string]any{"optionId": "once", "kind": "allow_once", "name": "Once"}, map[string]any{"optionId": "always", "kind": "allow_always", "name": "Always"}, map[string]any{"optionId": "deny", "kind": "reject_once", "name": "No"}}}})
			} else {
				update("tool_call_update", map[string]any{"toolCallId": "tool-1", "status": "completed"})
				finish("end_turn")
			}
		case "session/cancel":
			finish("cancelled")
		case "":
			if req.ID != "permission-1" {
				return 88
			}
			outcome, _ := req.Result["outcome"].(map[string]any)
			if outcome["optionId"] == "once" {
				update("tool_call_update", map[string]any{"toolCallId": "tool-1", "status": "completed"})
				finish("end_turn")
			} else {
				finish("cancelled")
			}
		default:
			return 87 // Includes session/close, rename, and x.ai interject.
		}
	}
	return 0
}

func geminiTestAdapter(t *testing.T, mode string, cfg Config) (*ACPAdapter, chan model.RuntimeEvent, context.Context) {
	t.Helper()
	t.Setenv("PAIRROOM_GEMINI_HELPER", "1")
	t.Setenv("PAIRROOM_GEMINI_MODE", mode)
	events := make(chan model.RuntimeEvent, 256)
	cfg.Actor = model.ActorSlot2
	cfg.Command = os.Args[0]
	cfg.Repo = t.TempDir()
	cfg.SystemPrompt = "GEMINI-BOOTSTRAP"
	adapter := NewGemini(cfg, func(event model.RuntimeEvent) { events <- event })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_ = adapter.Stop(stopCtx)
		cancel()
	})
	return adapter, events, ctx
}

func geminiEvent(t *testing.T, ctx context.Context, events <-chan model.RuntimeEvent, kind string) model.RuntimeEvent {
	t.Helper()
	for {
		select {
		case e := <-events:
			if e.Kind == kind {
				return e
			}
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", kind, ctx.Err())
			return model.RuntimeEvent{}
		}
	}
}

func TestGeminiACPNewSessionAndLegacyTransport(t *testing.T) {
	for _, mode := range []string{"new", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			cfg := Config{}
			adapter, events, ctx := geminiTestAdapter(t, mode, cfg)
			if err := adapter.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if adapter.SessionID() != "" {
				t.Fatal("new session materialized before a real Turn")
			}
			for round := 1; round <= 2; round++ {
				input := model.AgentInput{MessageID: fmt.Sprintf("input-%d", round), Text: "BODY 🌟\n  keep body", FromHandle: "@user"}
				if err := adapter.StartTurn(ctx, input); err != nil {
					t.Fatal(err)
				}
				final := geminiEvent(t, ctx, events, model.RuntimeFinal)
				if final.Text != fmt.Sprintf("Gemini answer %d", round) || final.Agent != model.ActorSlot2 {
					t.Fatalf("final=%+v", final)
				}
				completed := geminiEvent(t, ctx, events, model.RuntimeTurnCompleted)
				if completed.CorrelationID != input.MessageID || completed.Name != "end_turn" {
					t.Fatalf("completed=%+v", completed)
				}
			}
			expected := "gemini-new-session"
			if adapter.SessionID() != expected {
				t.Fatalf("session replaced: %s", adapter.SessionID())
			}
			if outcome := adapter.Steer(ctx, model.AgentInput{Text: "steer"}); outcome.State != SteerUnavailable {
				t.Fatalf("invented steering: %+v", outcome)
			}
		})
	}
}

func TestGeminiACPApprovalAndCancel(t *testing.T) {
	for _, mode := range []string{"permission", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			adapter, events, ctx := geminiTestAdapter(t, mode, Config{})
			if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "first", Text: "BODY 🌟"}); err != nil {
				t.Fatal(err)
			}
			requested := geminiEvent(t, ctx, events, model.RuntimeApprovalRequested)
			if requested.Approval == nil || requested.Approval.Kind != "gemini.permission" {
				t.Fatalf("approval=%+v", requested)
			}
			if mode == "permission" {
				if err := adapter.ResolveApproval(ctx, requested.Approval.ID, model.ApprovalResolution{Decision: "accept"}); err != nil {
					t.Fatal(err)
				}
			} else if err := adapter.Interrupt(ctx); err != nil {
				t.Fatal(err)
			}
			completed := geminiEvent(t, ctx, events, model.RuntimeTurnCompleted)
			expected := "end_turn"
			if mode == "cancel" {
				expected = "cancelled"
			}
			if completed.Name != expected {
				t.Fatalf("terminal=%+v", completed)
			}
		})
	}
}

func TestGeminiACPRejectsUnavailableProtocol(t *testing.T) {
	for _, mode := range []string{"no-acp", "wrong-version"} {
		t.Run(mode, func(t *testing.T) {
			adapter, _, ctx := geminiTestAdapter(t, mode, Config{})
			if err := adapter.Start(ctx); err == nil {
				t.Fatal("unsupported protocol or different session accepted")
			}
			if adapter.SessionID() != "" {
				t.Fatal("failed startup allocated a session")
			}
		})
	}
}

func TestGeminiACPBlocksReplayWithoutCompletionBoundary(t *testing.T) {
	adapter, events, ctx := geminiTestAdapter(t, "late-replay", Config{SessionID: "exact-gemini-session", RequireExactSession: true})
	if err := adapter.Start(ctx); !errors.Is(err, errGeminiExactResume) {
		if err != nil {
			t.Fatal(err)
		}
		// Retain the vendor-shaped late-history fixture: without the guard,
		// the prior private reply is published as part of the new Turn.
		if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "new-task", Text: "BODY 🌟"}); err != nil {
			t.Fatal(err)
		}
		final := geminiEvent(t, ctx, events, model.RuntimeFinal)
		t.Fatalf("unsafe resume published replay as a current answer: %q", final.Text)
	}
	if adapter.SessionID() != "exact-gemini-session" || adapter.cmd != nil || adapter.sessionOpened {
		t.Fatal("blocked resume changed the binding or launched a runtime")
	}
	if err := adapter.ensureSession(ctx); !errors.Is(err, errGeminiExactResume) {
		t.Fatalf("session setup bypassed the resume boundary: %v", err)
	}
}

func TestGeminiACPBlocksEngagedSessionRestart(t *testing.T) {
	adapter, events, ctx := geminiTestAdapter(t, "new", Config{})
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "first", Text: "BODY 🌟"}); err != nil {
		t.Fatal(err)
	}
	geminiEvent(t, ctx, events, model.RuntimeTurnCompleted)
	if err := adapter.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	// A missing command proves that the restore guard runs before even probing
	// a new process, independently of what the vendor advertises at initialize.
	adapter.cfg.Command = t.TempDir() + "/must-not-run"
	if err := adapter.Start(ctx); !errors.Is(err, errGeminiExactResume) {
		t.Fatalf("restart did not fail at the safe boundary: %v", err)
	}
	if adapter.SessionID() != "gemini-new-session" || adapter.cmd != nil || adapter.sessionOpened {
		t.Fatal("blocked restart discarded the accepted session")
	}
}

func TestGeminiACPExplicitPoliciesAndProcessExit(t *testing.T) {
	for _, policy := range []string{"plan", "yolo", "default", "auto_edit"} {
		t.Run(policy, func(t *testing.T) {
			t.Setenv("PAIRROOM_GEMINI_EXPECT_MODE", policy)
			adapter, events, ctx := geminiTestAdapter(t, "new", Config{PermissionMode: policy})
			if err := adapter.StartTurn(ctx, model.AgentInput{Text: "BODY 🌟", MessageID: "policy"}); err != nil {
				t.Fatal(err)
			}
			geminiEvent(t, ctx, events, model.RuntimeTurnCompleted)
		})
	}
	t.Run("exit", func(t *testing.T) {
		adapter, events, ctx := geminiTestAdapter(t, "exit", Config{})
		if err := adapter.StartTurn(ctx, model.AgentInput{Text: "BODY 🌟", MessageID: "exit"}); err != nil {
			t.Fatal(err)
		}
		done := geminiEvent(t, ctx, events, model.RuntimeTurnCompleted)
		if done.Name != "process_exited" || adapter.SessionID() != "gemini-new-session" {
			t.Fatalf("exit lost exact binding: %+v", done)
		}
	})
}

func TestGeminiArgsAndPermissionInheritance(t *testing.T) {
	cfg := Config{Model: "gemini-custom", PermissionMode: "auto_edit", Sandbox: "off", AdditionalInstructions: "SECRET-PROMPT"}
	args := strings.Join(geminiACPArgs(cfg, ProbeResult{SupportedFlags: map[string]bool{"--acp": true}}), " ")
	if args != "--acp --model gemini-custom --approval-mode auto_edit --sandbox=false" {
		t.Fatal(args)
	}
	plain := geminiACPArgs(Config{}, ProbeResult{SupportedFlags: map[string]bool{"--acp": true}})
	if len(plain) != 1 {
		t.Fatalf("empty override changed native defaults: %v", plain)
	}
	readOnly := PermissionConfig(Config{Runtime: model.RuntimeGemini, Sandbox: "on"}, model.PermissionReadOnly)
	if readOnly.PermissionMode != "plan" || readOnly.Sandbox != "on" {
		t.Fatalf("read-only widened sandbox: %+v", readOnly)
	}
}

func TestGeminiExplicitSandboxOverridesNativeEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, inherited, configured, expected string
	}{
		{"enable", "false", "on", "true"},
		{"disable", "true", "off", "false"},
		{"inherit", "podman", "", "podman"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GEMINI_SANDBOX", tc.inherited)
			t.Setenv("PAIRROOM_GEMINI_EXPECT_SANDBOX_ENV", tc.expected)
			adapter, events, ctx := geminiTestAdapter(t, "new", Config{Sandbox: tc.configured})
			if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "sandbox", Text: "BODY 🌟"}); err != nil {
				t.Fatal(err)
			}
			geminiEvent(t, ctx, events, model.RuntimeTurnCompleted)
			if got := os.Getenv("GEMINI_SANDBOX"); got != tc.inherited {
				t.Fatalf("changed Service sandbox environment: %q", got)
			}
		})
	}
}

func TestGeminiConfiguredYOLOCompletesSandboxOverride(t *testing.T) {
	for _, tc := range []struct{ configured, expected string }{{"", "off"}, {"on", "on"}, {"off", "off"}} {
		cfg := PermissionConfig(Config{Runtime: model.RuntimeGemini, PermissionMode: "yolo", Sandbox: tc.configured}, model.PermissionConfigured)
		if cfg.Sandbox != tc.expected {
			t.Fatalf("configured sandbox %q: got %q, want %q", tc.configured, cfg.Sandbox, tc.expected)
		}
	}
	if cfg := PermissionConfig(Config{Runtime: model.RuntimeGemini}, model.PermissionConfigured); cfg.Sandbox != "" {
		t.Fatal("empty policy no longer inherits native sandbox")
	}
}

type acpShortWriter struct {
	count int
	err   error
}

func (w acpShortWriter) Write(p []byte) (int, error) { return w.count, w.err }
func (w acpShortWriter) Close() error                { return nil }

func TestACPPartialWritesAreNotSafeToReplay(t *testing.T) {
	for _, w := range []acpShortWriter{{1, io.ErrClosedPipe}, {1, nil}, {0, io.ErrClosedPipe}} {
		adapter := NewGemini(Config{}, func(model.RuntimeEvent) {})
		adapter.stdin = w
		err := adapter.send(map[string]any{"method": "session/prompt"})
		if err == nil || errors.Is(err, ErrSubmissionUnknown) != (w.count > 0) {
			t.Fatalf("writer=%+v err=%v", w, err)
		}
	}
}

func TestGeminiStalledAuthenticationSetupDoesNotHoldTransport(t *testing.T) {
	adapter, _, ctx := geminiTestAdapter(t, "setup-hang", Config{})
	if err := adapter.Start(ctx); err != nil {
		t.Fatal(err)
	}
	previous := geminiSetupTimeout
	geminiSetupTimeout = 500 * time.Millisecond
	t.Cleanup(func() { geminiSetupTimeout = previous })
	started := time.Now()
	err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "not-submitted", Text: "BODY 🌟"})
	if err == nil || !strings.Contains(err.Error(), "native CLI first") || time.Since(started) > 5*time.Second {
		t.Fatalf("unbounded setup: %v", err)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.cmd != nil || adapter.sessionEngaged || adapter.turn != nil {
		t.Fatal("failed setup retained execution ownership or submitted the task")
	}
}
