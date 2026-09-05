package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestClaudeSuccessfulResultSettlesHeadAndStartsQueuedInput(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewClaude(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.state = model.StateWorking
	adapter.pending = []claudePending{
		{input: model.AgentInput{MessageID: "msg-first"}, turnID: "turn-first"},
		{input: model.AgentInput{MessageID: "msg-second"}, turnID: "turn-second"},
	}
	adapter.output.WriteString("streamed fallback")

	adapter.handleLine([]byte(`{"type":"result","subtype":"success","result":"final answer","session_id":"session-new","duration_ms":12,"usage":{"input_tokens":3}}`))

	var final *model.RuntimeEvent
	completed := false
	startedNext := false
	processingNext := false
	for i := range events {
		event := &events[i]
		switch {
		case event.Kind == model.RuntimeFinal:
			final = event
		case event.Kind == model.RuntimeInputCompleted && event.CorrelationID == "msg-first":
			completed = true
		case event.Kind == model.RuntimeTurnStarted && event.CorrelationID == "msg-second":
			startedNext = true
		case event.Kind == model.RuntimeInputProcessing && event.CorrelationID == "msg-second":
			processingNext = true
		}
	}
	if final == nil || final.Text != "final answer" || final.CorrelationID != "msg-first" || final.TurnID != "turn-first" {
		t.Fatalf("unexpected final event: %#v", final)
	}
	if !completed || !startedNext || !processingNext {
		t.Fatalf("queue lifecycle was incomplete: %#v", events)
	}
	if adapter.SessionID() != "session-new" || adapter.State() != model.StateWorking || len(adapter.pending) != 1 {
		t.Fatalf("unexpected adapter state after result: session=%q state=%q pending=%#v", adapter.SessionID(), adapter.State(), adapter.pending)
	}
}

func TestClaudeInterruptedResultIsCancelledWithoutFinalMessage(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewClaude(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.state = model.StateWorking
	adapter.pending = []claudePending{{input: model.AgentInput{MessageID: "msg-1"}, turnID: "turn-1"}}

	adapter.handleLine([]byte(`{"type":"result","subtype":"interrupted","is_error":true,"error":"user interrupted"}`))

	cancelled := false
	for _, event := range events {
		if event.Kind == model.RuntimeFinal {
			t.Fatalf("interrupted result must not publish a final room message: %#v", event)
		}
		if event.Kind == model.RuntimeInputCancelled && event.CorrelationID == "msg-1" && event.Text == "user interrupted" {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatalf("interrupted input was not settled as cancelled: %#v", events)
	}
	if adapter.State() != model.StateError {
		t.Fatalf("adapter should surface the interrupted process state, got %q", adapter.State())
	}
}

func TestClaudeUnmatchedResultIsDiagnosticOnly(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewClaude(Config{}, func(event model.RuntimeEvent) { events = append(events, event) })
	adapter.handleLine([]byte(`{"type":"result","subtype":"success","result":"stale native answer"}`))
	for _, event := range events {
		if event.Kind == model.RuntimeTurnCompleted || event.Kind == model.RuntimeFinal {
			t.Fatalf("unmatched Claude result created a Room boundary: %#v", events)
		}
	}
	if len(events) != 1 || events[0].Kind != model.RuntimeLog || events[0].Name != "result.unmatched" {
		t.Fatalf("unexpected unmatched result projection: %#v", events)
	}
}

func TestClaudeInitUpdatesSessionAndRuntimeInfo(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewClaude(Config{Model: "configured"}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.runtimeInfo = model.RuntimeInfo{
		Available: true, Protocol: "claude-stream-json", Capabilities: []string{"stream-json"},
	}

	adapter.handleLine([]byte(`{"type":"system","subtype":"init","session_id":"session-real","model":"claude-opus","version":"2.1.231","permissionMode":"auto","capabilities":{"hooks":true,"unused":false}}`))

	if adapter.SessionID() != "session-real" {
		t.Fatalf("session was not updated: %q", adapter.SessionID())
	}
	var sessionSeen, infoSeen bool
	for _, event := range events {
		if event.Kind == model.RuntimeSession && event.SessionID == "session-real" {
			sessionSeen = true
		}
		if event.Kind == model.RuntimeInfoUpdated && event.Runtime != nil {
			infoSeen = true
			if event.Runtime.Model != "claude-opus" || event.Runtime.Version != "2.1.231" || event.Runtime.PermissionMode != "auto" {
				t.Fatalf("unexpected runtime info: %#v", event.Runtime)
			}
			encoded, _ := json.Marshal(event.Runtime.Capabilities)
			if string(encoded) != `["hooks","stream-json"]` {
				t.Fatalf("unexpected capabilities: %s", encoded)
			}
		}
	}
	if !sessionSeen || !infoSeen {
		t.Fatalf("missing init projections: %#v", events)
	}
}

type testWriteCloser struct{ bytes.Buffer }

func (w *testWriteCloser) Close() error { return nil }

func TestClaudeControlInitializeHandshakeCompletesBeforeUse(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	var events []model.RuntimeEvent
	adapter := NewClaude(Config{}, func(event model.RuntimeEvent) { events = append(events, event) })
	adapter.stdin = writer
	adapter.runtimeInfo = model.RuntimeInfo{Available: true, Protocol: "claude-stream-json", Capabilities: []string{"stream-json"}}

	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		errCh <- adapter.initializeControl(ctx)
	}()

	line, err := bufio.NewReader(reader).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string          `json:"subtype"`
			Hooks   json.RawMessage `json:"hooks"`
		} `json:"request"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &request); err != nil {
		t.Fatal(err)
	}
	if request.Type != "control_request" || request.RequestID == "" || request.Request.Subtype != "initialize" || string(request.Request.Hooks) != "null" {
		t.Fatalf("unexpected initialize request: %#v", request)
	}
	adapter.handleLine([]byte(fmt.Sprintf(`{"type":"control_response","response":{"subtype":"success","request_id":%q,"response":{"commands":[],"models":[],"agents":[],"account":null,"pid":42}}}`, request.RequestID)))
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if !adapter.controlReady {
		t.Fatal("control protocol was not marked ready")
	}
	capabilities := strings.Join(adapter.runtimeInfo.Capabilities, ",")
	if !strings.Contains(capabilities, "interactive-approvals") || !strings.Contains(capabilities, "user-questions") {
		t.Fatalf("control capabilities were not projected: %q", capabilities)
	}
	found := false
	for _, event := range events {
		if event.Kind == model.RuntimeLog && event.Name == "control.initialized" {
			found = true
		}
	}
	if !found {
		t.Fatal("control initialization was not visible in the inspector")
	}
}

func TestClaudeUnsupportedControlRequestReturnsProtocolError(t *testing.T) {
	writer := &testWriteCloser{}
	adapter := NewClaude(Config{}, func(model.RuntimeEvent) {})
	adapter.stdin = writer
	adapter.handleLine([]byte(`{"type":"control_request","request_id":"unsupported-1","request":{"subtype":"unknown_future_request"}}`))
	var response struct {
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Error     string `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(writer.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if response.Response.Subtype != "error" || response.Response.RequestID != "unsupported-1" || !strings.Contains(response.Response.Error, "does not implement") {
		t.Fatalf("unexpected fail-closed control response: %#v", response)
	}
}

func TestClaudeStartTurnWritesInputExactlyOnce(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewClaude(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	writer := &testWriteCloser{}
	adapter.cmd = &exec.Cmd{Process: process}
	adapter.stdin = writer
	adapter.state = model.StateIdle
	adapter.protocolSent = true

	if err := adapter.StartTurn(context.Background(), model.AgentInput{MessageID: "msg-once", ThreadID: "thread-1"}); err != nil {
		t.Fatal(err)
	}
	if len(adapter.pending) != 1 || adapter.pending[0].input.MessageID != "msg-once" {
		t.Fatalf("input was not tracked exactly once: %#v", adapter.pending)
	}
	if got := bytes.Count(writer.Bytes(), []byte{'\n'}); got != 1 {
		t.Fatalf("expected one stream-json input line, got %d: %q", got, writer.String())
	}
}

func TestClaudeControlRequestCreatesApprovalAndReturnsAllowResponse(t *testing.T) {
	var events []model.RuntimeEvent
	writer := &testWriteCloser{}
	adapter := NewClaude(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.stdin = writer
	adapter.state = model.StateWorking
	adapter.pending = []claudePending{{input: model.AgentInput{MessageID: "msg-approval"}, turnID: "turn-approval"}}

	adapter.handleLine([]byte(`{"type":"control_request","request_id":"native-request-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"go test ./..."},"permission_suggestions":[{"type":"addRules","rules":[{"toolName":"Bash"}]}]}}`))

	var approval *model.Approval
	for _, event := range events {
		if event.Kind == model.RuntimeApprovalRequested && event.Approval != nil {
			copy := *event.Approval
			approval = &copy
			if event.CorrelationID != "msg-approval" || event.TurnID != "turn-approval" {
				t.Fatalf("approval lost input correlation: %#v", event)
			}
		}
	}
	if approval == nil || approval.Agent != model.ActorClaude || approval.Kind != "claude.toolApproval" {
		t.Fatalf("unexpected Claude approval event: %#v", approval)
	}
	if adapter.State() != model.StateWaiting {
		t.Fatalf("Claude should wait for approval, got %q", adapter.State())
	}

	if err := adapter.ResolveApproval(context.Background(), approval.ID, model.ApprovalResolution{Decision: "acceptForSession"}); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(writer.Bytes()), &response); err != nil {
		t.Fatalf("decode control response: %v: %q", err, writer.String())
	}
	outer, _ := response["response"].(map[string]any)
	inner, _ := outer["response"].(map[string]any)
	if response["type"] != "control_response" || outer["request_id"] != "native-request-1" || inner["behavior"] != "allow" {
		t.Fatalf("unexpected control response: %#v", response)
	}
	if _, ok := inner["updatedPermissions"]; !ok {
		t.Fatalf("session approval omitted native permission suggestions: %#v", inner)
	}
	if adapter.State() != model.StateWorking || len(adapter.approvals) != 0 {
		t.Fatalf("approval did not resume Claude: state=%q approvals=%d", adapter.State(), len(adapter.approvals))
	}
}

func TestClaudeAskUserQuestionReturnsStructuredAnswers(t *testing.T) {
	writer := &testWriteCloser{}
	var requested *model.Approval
	adapter := NewClaude(Config{}, func(event model.RuntimeEvent) {
		if event.Kind == model.RuntimeApprovalRequested && event.Approval != nil {
			copy := *event.Approval
			requested = &copy
		}
	})
	adapter.stdin = writer
	adapter.state = model.StateWorking
	adapter.pending = []claudePending{{input: model.AgentInput{MessageID: "msg-question"}, turnID: "turn-question"}}
	adapter.handleLine([]byte(`{"type":"control_request","request_id":"question-native","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Which approach?","header":"Approach","options":[{"label":"A","description":"first"},{"label":"B","description":"second"}],"multiSelect":false}]}}}`))
	if requested == nil || requested.Kind != "claude.userQuestion" {
		t.Fatalf("question was not projected as an interactive approval: %#v", requested)
	}

	if err := adapter.ResolveApproval(context.Background(), requested.ID, model.ApprovalResolution{
		Decision: "accept",
		Answers:  map[string]string{"Which approach?": "B"},
	}); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Response struct {
			Response struct {
				Behavior     string `json:"behavior"`
				UpdatedInput struct {
					Questions []map[string]any  `json:"questions"`
					Answers   map[string]string `json:"answers"`
				} `json:"updatedInput"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(writer.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if response.Response.Response.Behavior != "allow" || response.Response.Response.UpdatedInput.Answers["Which approach?"] != "B" || len(response.Response.Response.UpdatedInput.Questions) != 1 {
		t.Fatalf("unexpected question response: %#v", response)
	}
}

func TestClaudeRoleMapsReviewerToPlanModeWhileStopped(t *testing.T) {
	adapter := NewClaude(Config{PermissionMode: "auto"}, func(model.RuntimeEvent) {})
	if err := adapter.SetRole(context.Background(), model.RoleReviewer); err != nil {
		t.Fatal(err)
	}
	if adapter.role != model.RoleReviewer || adapter.cfg.PermissionMode != "plan" {
		t.Fatalf("reviewer policy was not applied: role=%q mode=%q", adapter.role, adapter.cfg.PermissionMode)
	}
	if err := adapter.SetRole(context.Background(), model.RoleDriver); err != nil {
		t.Fatal(err)
	}
	if adapter.role != model.RoleDriver || adapter.cfg.PermissionMode != "auto" {
		t.Fatalf("driver policy was not restored: role=%q mode=%q", adapter.role, adapter.cfg.PermissionMode)
	}
}

func TestClaudeEmptyPermissionModeReturnsToNativeInheritance(t *testing.T) {
	adapter := NewClaude(Config{}, func(model.RuntimeEvent) {})
	if adapter.baseMode != "" || adapter.cfg.PermissionMode != "" {
		t.Fatalf("empty permission override was synthesized: base=%q current=%q", adapter.baseMode, adapter.cfg.PermissionMode)
	}
	if err := adapter.SetRole(context.Background(), model.RoleReviewer); err != nil {
		t.Fatal(err)
	}
	if adapter.cfg.PermissionMode != "plan" {
		t.Fatalf("reviewer mode = %q, want plan", adapter.cfg.PermissionMode)
	}
	if err := adapter.SetRole(context.Background(), model.RoleDriver); err != nil {
		t.Fatal(err)
	}
	if adapter.cfg.PermissionMode != "" {
		t.Fatalf("driver retained a synthesized permission override: %q", adapter.cfg.PermissionMode)
	}
}

func TestClaudeYoloPermissionArgsEmitNativeBypassFlags(t *testing.T) {
	flags := map[string]bool{
		"--permission-mode":                    true,
		"--allow-dangerously-skip-permissions": true,
		"--dangerously-skip-permissions":       true,
	}
	for _, mode := range []string{"yolo", "bypassPermissions", "always-approve"} {
		got := strings.Join(appendClaudePermissionArgs(nil, flags, mode), " ")
		for _, want := range []string{
			"--allow-dangerously-skip-permissions",
			"--dangerously-skip-permissions",
			"--permission-mode bypassPermissions",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("mode %q missing %q in %s", mode, want, got)
			}
		}
	}
	auto := strings.Join(appendClaudePermissionArgs(nil, flags, "auto"), " ")
	if auto != "--permission-mode auto" {
		t.Fatalf("auto mode = %q", auto)
	}
	if got := appendClaudePermissionArgs(nil, flags, ""); len(got) != 0 {
		t.Fatalf("empty mode emitted %v", got)
	}
}

func TestClaudeStopClearsPendingApprovals(t *testing.T) {
	adapter := NewClaude(Config{}, func(model.RuntimeEvent) {})
	adapter.approvals["approval-1"] = claudeApprovalRequest{requestID: "native"}
	if err := adapter.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(adapter.approvals) != 0 {
		t.Fatalf("stopped adapter retained stale approvals: %#v", adapter.approvals)
	}
}

func TestClaudeReviewerFailClosesNativeWriteRequest(t *testing.T) {
	writer := &testWriteCloser{}
	var events []model.RuntimeEvent
	adapter := NewClaude(Config{}, func(event model.RuntimeEvent) { events = append(events, event) })
	adapter.stdin = writer
	adapter.state = model.StateWorking
	adapter.role = model.RoleReviewer
	adapter.handleLine([]byte(`{"type":"control_request","request_id":"reviewer-write","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"unsafe.txt","content":"no"}}}`))

	if len(adapter.approvals) != 0 {
		t.Fatalf("reviewer write request entered the approval queue: %#v", adapter.approvals)
	}
	var response struct {
		Response struct {
			Response struct {
				Behavior string `json:"behavior"`
				Message  string `json:"message"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(writer.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if response.Response.Response.Behavior != "deny" || !strings.Contains(response.Response.Response.Message, "reviewer role") {
		t.Fatalf("unexpected reviewer response: %#v", response)
	}
	foundLog := false
	for _, event := range events {
		if event.Kind == model.RuntimeLog && event.Name == "reviewer.tool.denied" {
			foundLog = true
		}
	}
	if !foundLog {
		t.Fatal("reviewer denial was not visible in the runtime inspector")
	}
}

func TestClaudeInputContentUsesNativeImageBlocks(t *testing.T) {
	path := t.TempDir() + string(os.PathSeparator) + "diagram.png"
	payload := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	content, err := claudeInputContent("inspect the diagram", []model.AgentAttachment{{
		Attachment: model.Attachment{
			ID: "att-test", Name: "diagram.png", Kind: "image", MediaType: "image/png", Size: int64(len(payload)),
		},
		Path: path,
	}})
	if err != nil {
		t.Fatal(err)
	}
	blocks, ok := content.([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("unexpected multimodal content: %#v", content)
	}
	imageBlock, ok := blocks[0].(map[string]any)
	if !ok || imageBlock["type"] != "image" {
		t.Fatalf("unexpected image block: %#v", blocks[0])
	}
	textBlock, ok := blocks[1].(map[string]any)
	if !ok || textBlock["type"] != "text" || textBlock["text"] != "inspect the diagram" {
		t.Fatalf("unexpected text block: %#v", blocks[1])
	}
	source, ok := imageBlock["source"].(map[string]any)
	if !ok || source["type"] != "base64" || source["media_type"] != "image/png" {
		t.Fatalf("unexpected image source: %#v", imageBlock["source"])
	}
	if source["data"] != "iVBORw0KGgoBAgM=" {
		t.Fatalf("unexpected base64 payload: %q", source["data"])
	}
}

func TestClaudeInputContentTextOnlyRemainsString(t *testing.T) {
	content, err := claudeInputContent("plain text", nil)
	if err != nil {
		t.Fatal(err)
	}
	if content != "plain text" {
		t.Fatalf("unexpected text-only content: %#v", content)
	}
}

func TestClaudeInputContentRejectsChangedAttachment(t *testing.T) {
	path := t.TempDir() + string(os.PathSeparator) + "diagram.png"
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := claudeInputContent("inspect", []model.AgentAttachment{{
		Attachment: model.Attachment{Name: "diagram.png", Kind: "image", MediaType: "image/png", Size: 3},
		Path:       path,
	}})
	if err == nil || !strings.Contains(err.Error(), "changed after attachment validation") {
		t.Fatalf("expected changed attachment error, got %v", err)
	}
}

func TestClaudeToolApprovalReturnsCompleteUpdatedInput(t *testing.T) {
	writer := &testWriteCloser{}
	adapter := NewClaude(Config{}, func(model.RuntimeEvent) {})
	adapter.stdin = writer
	adapter.state = model.StateWaiting
	adapter.approvals["approval-1"] = claudeApprovalRequest{
		requestID: "request-1",
		toolName:  "Bash",
		input:     json.RawMessage(`{"command":"go test ./...","description":"Run tests"}`),
	}

	if err := adapter.ResolveApproval(context.Background(), "approval-1", model.ApprovalResolution{Decision: "accept"}); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Type     string `json:"type"`
		Response struct {
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior     string         `json:"behavior"`
				UpdatedInput map[string]any `json:"updatedInput"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(writer.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if response.Type != "control_response" || response.Response.RequestID != "request-1" || response.Response.Response.Behavior != "allow" {
		t.Fatalf("unexpected control response: %#v", response)
	}
	if response.Response.Response.UpdatedInput["command"] != "go test ./..." || response.Response.Response.UpdatedInput["description"] != "Run tests" {
		t.Fatalf("tool input was not preserved: %#v", response.Response.Response.UpdatedInput)
	}
}

func TestClaudeQuestionApprovalAddsAnswersToOriginalInput(t *testing.T) {
	writer := &testWriteCloser{}
	adapter := NewClaude(Config{}, func(model.RuntimeEvent) {})
	adapter.stdin = writer
	adapter.state = model.StateWaiting
	adapter.approvals["approval-question"] = claudeApprovalRequest{
		requestID: "request-question",
		toolName:  "AskUserQuestion",
		input:     json.RawMessage(`{"questions":[{"question":"Which?","header":"Choice","options":[{"label":"A"},{"label":"B"}],"multiSelect":false}],"metadata":{"source":"pairroom-test"}}`),
	}

	answers := map[string]string{"Which?": "B"}
	if err := adapter.ResolveApproval(context.Background(), "approval-question", model.ApprovalResolution{Decision: "accept", Answers: answers}); err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(writer.Bytes()), &envelope); err != nil {
		t.Fatal(err)
	}
	outer := envelope["response"].(map[string]any)
	result := outer["response"].(map[string]any)
	updated := result["updatedInput"].(map[string]any)
	if _, ok := updated["questions"]; !ok {
		t.Fatalf("questions were not preserved: %#v", updated)
	}
	if _, ok := updated["metadata"]; !ok {
		t.Fatalf("metadata was not preserved: %#v", updated)
	}
	gotAnswers := updated["answers"].(map[string]any)
	if gotAnswers["Which?"] != "B" {
		t.Fatalf("answers were not inserted: %#v", gotAnswers)
	}
}
