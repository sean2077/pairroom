package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestGrokACPCommandOmitsUnsetOverridesAndPromptText(t *testing.T) {
	adapter := NewGrok(Config{
		Actor: model.ActorClaude, Command: "grok", Repo: "/repo",
		AdditionalInstructions: "Never mention secrets.",
	}, func(model.RuntimeEvent) {})
	joined := strings.Join(adapter.buildACPArgs(), " ")
	for _, want := range []string{"--no-auto-update", "--cwd /repo", "agent stdio"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	for _, forbidden := range []string{"--prompt-file", "--single", "Never mention", "--model", "--reasoning-effort", "--permission-mode", "--sandbox"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unexpected %q in %s", forbidden, joined)
		}
	}
}

func TestGrokACPCommandPassesExplicitRuntimeOverrides(t *testing.T) {
	adapter := NewGrok(Config{
		Actor: model.ActorCodex, Command: "grok", Repo: "/repo", Model: "grok-4.6",
		Effort: "high", PermissionMode: "auto", Sandbox: "workspace-write",
	}, func(model.RuntimeEvent) {})
	joined := strings.Join(adapter.buildACPArgs(), " ")
	for _, want := range []string{"--model grok-4.6", "--reasoning-effort high", "--always-approve", "--sandbox workspace-write", "agent stdio"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
}

func TestSelectGrokAuthMethodUsesAdvertisedDefault(t *testing.T) {
	raw := json.RawMessage(`{"authMethods":[{"id":"xai.api_key"},{"id":"cached_token"}],"_meta":{"defaultAuthMethodId":"cached_token"}}`)
	method, err := selectGrokAuthMethod(raw)
	if err != nil || method != "cached_token" {
		t.Fatalf("method=%q err=%v", method, err)
	}
	if _, err := selectGrokAuthMethod(json.RawMessage(`{"authMethods":[{"id":"grok.com"}]}`)); err == nil {
		t.Fatal("interactive-only auth must fail closed")
	}
}

func TestParseGrokCapabilitiesPinsLifecycleAndImageSupport(t *testing.T) {
	capabilities, err := parseGrokCapabilities(json.RawMessage(`{"agentCapabilities":{"loadSession":true,"promptCapabilities":{"image":true},"sessionCapabilities":{"close":{}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.loadSession || !capabilities.close || !capabilities.promptImage {
		t.Fatalf("unexpected capabilities: %+v", capabilities)
	}
	withoutClose, err := parseGrokCapabilities(json.RawMessage(`{"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"close":false}}}`))
	if err != nil || withoutClose.close {
		t.Fatalf("false close capability must not be treated as supported: %+v err=%v", withoutClose, err)
	}
}

func TestGrokSessionUpdateProjectsRootAgentText(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewGrok(Config{Actor: model.ActorCodex}, func(event model.RuntimeEvent) { events = append(events, event) })
	turn := &grokTurn{turnID: "turn-1", inputs: []model.AgentInput{{MessageID: "msg-1"}}}
	adapter.sessionID = "session-1"
	adapter.turn = turn
	adapter.handleSessionUpdate(json.RawMessage(`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}`))
	if turn.final.String() != "hello" || len(events) != 1 || events[0].Kind != model.RuntimeTextDelta || events[0].CorrelationID != "msg-1" {
		t.Fatalf("unexpected projection: final=%q events=%#v", turn.final.String(), events)
	}
	adapter.handleSessionUpdate(json.RawMessage(`{"sessionId":"child","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"leak"}}}`))
	if turn.final.String() != "hello" {
		t.Fatalf("child output leaked into root final: %q", turn.final.String())
	}
}

type grokRPCRecorder struct {
	mu      sync.Mutex
	adapter *GrokAdapter
	lines   [][]byte
	reject  bool
}

func (r *grokRPCRecorder) Write(data []byte) (int, error) {
	line := append([]byte(nil), data...)
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.mu.Unlock()
	var request struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	if json.Unmarshal(data, &request) == nil && request.ID != 0 {
		response := []byte(`{"jsonrpc":"2.0","id":` + jsonNumber(request.ID) + `,"result":{"status":"queued"}}`)
		if r.reject {
			response = []byte(`{"jsonrpc":"2.0","id":` + jsonNumber(request.ID) + `,"error":{"code":-32601,"message":"missing"}}`)
		}
		r.adapter.handleRPCLine(response)
	}
	return len(data), nil
}

func (r *grokRPCRecorder) Close() error { return nil }

func jsonNumber(value int64) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestGrokSteerUsesInterjectAndClassifiesMethodMissing(t *testing.T) {
	adapter := NewGrok(Config{Actor: model.ActorClaude}, func(model.RuntimeEvent) {})
	recorder := &grokRPCRecorder{adapter: adapter}
	adapter.stdin = recorder
	adapter.sessionID = "session-1"
	adapter.turn = &grokTurn{turnID: "turn-1", inputs: []model.AgentInput{{MessageID: "initial"}}}
	outcome := adapter.Steer(context.Background(), model.AgentInput{MessageID: "steer-1", Text: "focus"})
	if outcome.State != SteerAccepted || len(adapter.turn.inputs) != 2 {
		t.Fatalf("accepted steer = %+v inputs=%d", outcome, len(adapter.turn.inputs))
	}
	recorder.reject = true
	outcome = adapter.Steer(context.Background(), model.AgentInput{MessageID: "steer-2", Text: "again"})
	if outcome.State != SteerUnavailable || len(adapter.turn.inputs) != 2 {
		t.Fatalf("missing method steer = %+v inputs=%d", outcome, len(adapter.turn.inputs))
	}
	recorder.mu.Lock()
	joined := string(recorder.lines[0])
	recorder.mu.Unlock()
	if !strings.Contains(joined, `"method":"_x.ai/interject"`) || !strings.Contains(joined, `"interjectionId":"steer-1"`) {
		t.Fatalf("unexpected interject request: %s", joined)
	}
}

func TestClassifyGrokInterjectAcknowledgement(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		state SteerState
	}{
		{name: "queued", raw: `{"status":"queued"}`, state: SteerAccepted},
		{name: "rejected", raw: `{"status":"rejected","reason":"busy"}`, state: SteerRejected},
		{name: "unavailable", raw: `{"status":"unsupported"}`, state: SteerUnavailable},
		{name: "unknown", raw: `{"status":"future-state"}`, state: SteerUnknown},
		{name: "missing", raw: `{}`, state: SteerUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, detail := classifyGrokInterjectAcknowledgement(json.RawMessage(tt.raw))
			if state != tt.state || strings.TrimSpace(detail) == "" {
				t.Fatalf("state=%q detail=%q, want %q with detail", state, detail, tt.state)
			}
		})
	}
}

func TestGrokACPLifecycleCreatesSessionAndInterjects(t *testing.T) {
	t.Setenv("PAIRROOM_GROK_HELPER", "1")
	t.Setenv("PAIRROOM_GROK_HELPER_MODE", "interject")
	events := make(chan model.RuntimeEvent, 64)
	adapter := NewGrok(Config{Actor: model.ActorClaude, Command: os.Args[0], Repo: t.TempDir()}, func(event model.RuntimeEvent) { events <- event })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := adapter.StartTurn(ctx, model.AgentInput{
		MessageID: "first", ThreadID: "thread", From: model.ActorUser, To: model.ActorClaude,
		FromHandle: "@user", SelfHandle: "@grok", PeerHandle: "@codex", Role: model.RoleDriver, Text: "begin",
	}); err != nil {
		t.Fatal(err)
	}
	if event := waitRuntimeEvent(t, events, model.RuntimeTextDelta); event.Text != "initial" {
		t.Fatalf("first Grok chunk = %#v", event)
	}
	outcome := adapter.Steer(ctx, model.AgentInput{
		MessageID: "steer", ThreadID: "thread", From: model.ActorUser, To: model.ActorClaude,
		FromHandle: "@user", SelfHandle: "@grok", PeerHandle: "@codex", Role: model.RoleDriver, Text: "change direction",
	})
	if outcome.State != SteerAccepted {
		t.Fatalf("Grok interject outcome = %+v", outcome)
	}
	final := waitRuntimeEvent(t, events, model.RuntimeFinal)
	if final.Text != "initial steered" || final.CorrelationID != "steer" {
		t.Fatalf("Grok final = %#v", final)
	}
	completed := waitRuntimeEvent(t, events, model.RuntimeTurnCompleted)
	if completed.Name != "end_turn" {
		t.Fatalf("Grok completion = %#v", completed)
	}
	if adapter.SessionID() != "grok-new-session" {
		t.Fatalf("Grok session ID = %q", adapter.SessionID())
	}
	if err := adapter.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestGrokACPExactLoadFiltersReplayAndInjectsBootstrapOnce(t *testing.T) {
	t.Setenv("PAIRROOM_GROK_HELPER", "1")
	t.Setenv("PAIRROOM_GROK_HELPER_MODE", "resume")
	events := make(chan model.RuntimeEvent, 64)
	adapter := NewGrok(Config{
		Actor: model.ActorCodex, Command: os.Args[0], Repo: t.TempDir(), SessionID: "required-session", RequireExactSession: true,
	}, func(event model.RuntimeEvent) { events <- event })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if adapter.SessionID() != "required-session" || !adapter.sessionOpened || !adapter.bootstrapPending {
		t.Fatalf("Grok did not validate exact session/load during startup: session=%q opened=%v bootstrap=%v", adapter.SessionID(), adapter.sessionOpened, adapter.bootstrapPending)
	}
	if err := adapter.StartTurn(ctx, model.AgentInput{
		MessageID: "resume", ThreadID: "thread", From: model.ActorUser, To: model.ActorCodex,
		FromHandle: "@user", SelfHandle: "@grok", PeerHandle: "@claude", Role: model.RoleReviewer, Text: "continue",
	}); err != nil {
		t.Fatal(err)
	}
	final := waitRuntimeEvent(t, events, model.RuntimeFinal)
	if final.Text != "bootstrap-present" || adapter.SessionID() != "required-session" {
		t.Fatalf("Grok resumed final=%#v session=%q", final, adapter.SessionID())
	}
	for {
		select {
		case event := <-events:
			if strings.Contains(event.Text, "historical-secret") || strings.Contains(string(event.Data), "historical-secret") {
				t.Fatalf("Grok session/load replay leaked into PairRoom: %#v", event)
			}
		default:
			if err := adapter.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
}

func TestGrokACPPermissionResolutionAndCancellation(t *testing.T) {
	for _, decision := range []string{"accept", "cancel"} {
		t.Run(decision, func(t *testing.T) {
			t.Setenv("PAIRROOM_GROK_HELPER", "1")
			t.Setenv("PAIRROOM_GROK_HELPER_MODE", "permission")
			events := make(chan model.RuntimeEvent, 64)
			adapter := NewGrok(Config{Actor: model.ActorClaude, Command: os.Args[0], Repo: t.TempDir()}, func(event model.RuntimeEvent) { events <- event })
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "permission", ThreadID: "thread", Role: model.RoleDriver, Text: "run"}); err != nil {
				t.Fatal(err)
			}
			requested := waitRuntimeEvent(t, events, model.RuntimeApprovalRequested)
			if requested.Approval == nil || requested.Approval.Kind != "grok.permission" {
				t.Fatalf("Grok permission = %#v", requested)
			}
			if decision == "accept" {
				if err := adapter.ResolveApproval(ctx, requested.Approval.ID, model.ApprovalResolution{Decision: "accept"}); err != nil {
					t.Fatal(err)
				}
				if final := waitRuntimeEvent(t, events, model.RuntimeFinal); final.Text != "initialapproved" {
					t.Fatalf("Grok approved final = %#v", final)
				}
			} else if err := adapter.Interrupt(ctx); err != nil {
				t.Fatal(err)
			}
			completed := waitRuntimeEvent(t, events, model.RuntimeTurnCompleted)
			if decision == "cancel" && completed.Name != "cancelled" {
				t.Fatalf("Grok cancelled completion = %#v", completed)
			}
			if err := adapter.Stop(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func waitRuntimeEvent(t *testing.T, events <-chan model.RuntimeEvent, kind string) model.RuntimeEvent {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if event.Kind == kind {
				return event
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for runtime event %s", kind)
			return model.RuntimeEvent{}
		}
	}
}

func TestGrokFactoryEmitsConfiguredSlotActor(t *testing.T) {
	adapter := GrokFactory(Config{Actor: model.ActorCodex, Runtime: model.RuntimeGrok}, func(model.RuntimeEvent) {})
	if adapter.Actor() != model.ActorCodex {
		t.Fatalf("Grok factory actor = %s", adapter.Actor())
	}
}

func TestGrokRuntimeLogsRedactConfiguredSecrets(t *testing.T) {
	const secret = "grok-provider-secret-value"
	var events []model.RuntimeEvent
	adapter := NewGrok(Config{
		Actor: model.ActorClaude,
		Env:   map[string]string{"XAI_API_KEY": secret},
	}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.readStderr(strings.NewReader("native stderr echoed " + secret + "\n"))
	adapter.handleRPCLine([]byte("invalid rpc line containing " + secret))

	for _, event := range events {
		if strings.Contains(event.Text, secret) || strings.Contains(string(event.Data), secret) {
			t.Fatalf("configured secret leaked into Grok runtime event: %#v", event)
		}
	}
	if len(events) != 2 || !strings.Contains(events[0].Text, "[REDACTED]") || !strings.Contains(events[1].Text, "[REDACTED]") {
		t.Fatalf("Grok logs were not visibly redacted: %#v", events)
	}
}
