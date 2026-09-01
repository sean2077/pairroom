package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

type codexRPCRecorder struct {
	adapter  *CodexAdapter
	requests []json.RawMessage
}

func (r *codexRPCRecorder) Write(data []byte) (int, error) {
	line := append([]byte(nil), data...)
	r.requests = append(r.requests, line)
	var request struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(line, &request); err != nil {
		return 0, err
	}
	r.adapter.handleRPCLine([]byte(fmt.Sprintf(`{"id":%d,"result":{}}`, request.ID)))
	return len(data), nil
}

func (*codexRPCRecorder) Close() error { return nil }

func TestCodexApprovalResult(t *testing.T) {
	command := pendingApproval{method: "item/commandExecution/requestApproval"}
	got, err := codexApprovalResult(command, "acceptForSession")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]any{"decision": "acceptForSession"}) {
		t.Fatalf("unexpected command response: %#v", got)
	}

	permission := pendingApproval{
		method: "item/permissions/requestApproval",
		params: json.RawMessage(`{"permissions":{"fileSystem":{"write":["/repo"]},"network":{"enabled":true}}}`),
	}
	got, err = codexApprovalResult(permission, "accept")
	if err != nil {
		t.Fatal(err)
	}
	if got["scope"] != "turn" {
		t.Fatalf("unexpected turn scope: %#v", got)
	}
	permissions, ok := got["permissions"].(map[string]any)
	if !ok || permissions["fileSystem"] == nil || permissions["network"] == nil {
		t.Fatalf("requested permission profile was not preserved: %#v", got)
	}

	got, err = codexApprovalResult(permission, "acceptForSession")
	if err != nil {
		t.Fatal(err)
	}
	if got["scope"] != "session" {
		t.Fatalf("unexpected session scope: %#v", got)
	}

	got, err = codexApprovalResult(permission, "decline")
	if err != nil {
		t.Fatal(err)
	}
	denied, ok := got["permissions"].(map[string]any)
	if !ok || len(denied) != 0 {
		t.Fatalf("decline must grant an empty subset: %#v", got)
	}
}

func TestCodexApprovalResultRejectsMalformedPermissionRequest(t *testing.T) {
	_, err := codexApprovalResult(pendingApproval{
		method: "item/permissions/requestApproval",
		params: json.RawMessage(`{"permissions":`),
	}, "accept")
	if err == nil {
		t.Fatal("expected malformed request to fail closed")
	}
}

func TestParseCodexRequestID(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`42`), json.RawMessage(`"42"`)} {
		got, err := ParseCodexRequestID(raw)
		if err != nil || got != 42 {
			t.Fatalf("ParseCodexRequestID(%s) = %d, %v", raw, got, err)
		}
	}
}

func TestCodexEarlyTurnStartedBindsStartingInput(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewCodex(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	input := model.AgentInput{MessageID: "msg-early", ThreadID: "thread-1"}
	adapter.startingInput = &input

	adapter.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"turn-early"}}`))

	bound, ok := adapter.turnInputs["turn-early"]
	if !ok || len(bound) != 1 || bound[0].MessageID != input.MessageID {
		t.Fatalf("early turn was not correlated to starting input: %#v", bound)
	}
	var started *model.RuntimeEvent
	for i := range events {
		if events[i].Kind == model.RuntimeTurnStarted {
			started = &events[i]
			break
		}
	}
	if started == nil || started.TurnID != "turn-early" || started.CorrelationID != input.MessageID {
		t.Fatalf("unexpected early turn event: %#v", started)
	}
}

func TestCodexApprovalEventsRetainRoomCorrelation(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewCodex(Config{RequireExactSession: true}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	input := model.AgentInput{MessageID: "msg-approval"}
	adapter.mu.Lock()
	adapter.currentTurn = "turn-approval"
	adapter.turnInputs[adapter.currentTurn] = []model.AgentInput{input}
	adapter.mu.Unlock()

	adapter.handleServerRequest(
		json.RawMessage(`17`),
		"item/commandExecution/requestApproval",
		json.RawMessage(`{"turnId":"turn-approval","command":"go test ./..."}`),
	)
	if len(adapter.approvals) != 1 {
		t.Fatalf("current-turn approval was not retained: %#v", adapter.approvals)
	}
	var requested *model.RuntimeEvent
	for index := range events {
		if events[index].Kind == model.RuntimeApprovalRequested {
			requested = &events[index]
			break
		}
	}
	if requested == nil || requested.TurnID != "turn-approval" || requested.CorrelationID != input.MessageID {
		t.Fatalf("approval request correlation=%#v", requested)
	}

	adapter.handleServerRequestResolved(json.RawMessage(`{"requestId":17}`))
	var resolved *model.RuntimeEvent
	for index := range events {
		if events[index].Kind == model.RuntimeApprovalResolved {
			resolved = &events[index]
		}
	}
	if resolved == nil || resolved.TurnID != "turn-approval" || resolved.CorrelationID != input.MessageID {
		t.Fatalf("approval resolution correlation=%#v", resolved)
	}
}

func TestCodexStrictResumeDeclinesPreBindingApproval(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewCodex(Config{RequireExactSession: true}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	recorder := &codexRPCRecorder{adapter: adapter}
	adapter.stdin = recorder
	adapter.handleServerRequest(
		json.RawMessage(`23`),
		"item/commandExecution/requestApproval",
		json.RawMessage(`{"command":"historical-secret"}`),
	)

	if len(adapter.approvals) != 0 {
		t.Fatalf("pre-binding Codex approval was retained: %#v", adapter.approvals)
	}
	if len(recorder.requests) != 1 {
		t.Fatalf("expected one fail-closed response, got %d", len(recorder.requests))
	}
	var response struct {
		ID     int64 `json:"id"`
		Result struct {
			Decision string `json:"decision"`
		} `json:"result"`
	}
	if err := json.Unmarshal(recorder.requests[0], &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != 23 || response.Result.Decision != "decline" {
		t.Fatalf("pre-binding approval response=%s", recorder.requests[0])
	}
	for _, event := range events {
		if event.Kind == model.RuntimeApprovalRequested {
			t.Fatalf("pre-binding Codex approval was emitted: %#v", event)
		}
		if strings.Contains(event.Text, "historical-secret") || strings.Contains(string(event.Data), "historical-secret") {
			t.Fatalf("pre-binding Codex transcript leaked through diagnostic event: %#v", event)
		}
	}
}

func TestCodexTurnRequestsUseDocumentedCorrelationFields(t *testing.T) {
	adapter := NewCodex(Config{
		Repo: "/repo", Model: "gpt-5.3-codex", Effort: "high",
		ApprovalPolicy: "untrusted", Sandbox: "workspaceWrite",
	}, func(model.RuntimeEvent) {})
	input := model.AgentInput{MessageID: "msg-correlation", Role: model.RoleDriver}

	started := adapter.turnStartParams("thread-1", "hello", input)
	if got := started["clientUserMessageId"]; got != input.MessageID {
		t.Fatalf("turn/start clientUserMessageId = %#v", got)
	}
	if got := started["threadId"]; got != "thread-1" {
		t.Fatalf("turn/start threadId = %#v", got)
	}

	steered := codexTurnSteerParams("thread-1", "turn-1", "change direction", input)
	if got := steered["clientUserMessageId"]; got != input.MessageID {
		t.Fatalf("turn/steer clientUserMessageId = %#v", got)
	}
	if got := steered["expectedTurnId"]; got != "turn-1" {
		t.Fatalf("turn/steer expectedTurnId = %#v", got)
	}
}

func TestCodexApprovalPolicyUsesCurrentAppServerVariant(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "default", want: "untrusted"},
		{name: "legacy PairRoom config", value: "unlessTrusted", want: "untrusted"},
		{name: "current explicit policy", value: "on-request", want: "on-request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := NewCodex(Config{Repo: "/repo", ApprovalPolicy: test.value}, func(model.RuntimeEvent) {})
			if got := adapter.threadStartParams()["approvalPolicy"]; got != test.want {
				t.Fatalf("thread/start approvalPolicy = %#v, want %q", got, test.want)
			}
			if got := adapter.turnStartParams("thread-1", "hello", model.AgentInput{})["approvalPolicy"]; got != test.want {
				t.Fatalf("turn/start approvalPolicy = %#v, want %q", got, test.want)
			}
		})
	}
}

func TestCodexInputItemsIncludeLocalImages(t *testing.T) {
	items := codexInputItems("inspect", []model.AgentAttachment{{
		Attachment: model.Attachment{Name: "diagram.png", MediaType: "image/png"},
		Path:       "/tmp/diagram.png",
	}})
	if len(items) != 2 {
		t.Fatalf("expected text and image input, got %#v", items)
	}
	image, ok := items[1].(map[string]any)
	if !ok || image["type"] != "localImage" || image["path"] != "/tmp/diagram.png" {
		t.Fatalf("unexpected image input: %#v", items[1])
	}
}

func TestCodexThreadRequestsUseDeveloperInstructions(t *testing.T) {
	const instructions = "PAIRROOM-COLLABORATION-PROTOCOL"
	adapter := NewCodex(Config{Repo: "/repo", SystemPrompt: instructions}, func(model.RuntimeEvent) {})

	for name, params := range map[string]map[string]any{
		"start":  adapter.threadStartParams(),
		"resume": adapter.threadResumeParams("thread-existing"),
	} {
		if got := params["developerInstructions"]; got != instructions {
			t.Fatalf("thread/%s developerInstructions = %#v, want %q", name, got, instructions)
		}
		if got := params["cwd"]; got != "/repo" {
			t.Fatalf("thread/%s cwd = %#v", name, got)
		}
	}
	if got := adapter.threadResumeParams("thread-existing")["threadId"]; got != "thread-existing" {
		t.Fatalf("thread/resume threadId = %#v", got)
	}
	defaultAdapter := NewCodex(Config{}, func(model.RuntimeEvent) {})
	if got := defaultAdapter.threadStartParams()["developerInstructions"]; got != prompt.BootstrapPrompt(model.ActorCodex) {
		t.Fatalf("default developerInstructions = %#v", got)
	}
}

func TestCodexTurnRequestsDoNotInlineDeveloperInstructions(t *testing.T) {
	const instructions = "PAIRROOM-COLLABORATION-PROTOCOL"
	adapter := NewCodex(Config{SystemPrompt: instructions}, func(model.RuntimeEvent) {})
	input := model.AgentInput{MessageID: "msg-first", Text: "first intervention"}
	started, err := json.Marshal(adapter.turnStartParams("thread-id", prompt.Envelope(input), input))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(started), instructions) {
		t.Fatalf("turn/start repeated developer instructions: %s", started)
	}

	recorder := &codexRPCRecorder{adapter: adapter}
	adapter.cmd = &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
	adapter.stdin = recorder
	adapter.state = model.StateWorking
	adapter.threadID = "thread-active"
	adapter.currentTurn = "turn-active"

	for _, input := range []model.AgentInput{
		input,
		{MessageID: "msg-second", Text: "second intervention"},
	} {
		state, err := adapter.Submit(context.Background(), input)
		if err != nil || state != model.DeliveryInjected {
			t.Fatalf("steer %s = %q, %v", input.MessageID, state, err)
		}
	}
	if len(recorder.requests) != 2 {
		t.Fatalf("request count = %d", len(recorder.requests))
	}
	for _, request := range recorder.requests {
		if strings.Contains(string(request), instructions) {
			t.Fatalf("turn/steer repeated developer instructions: %s", request)
		}
	}
}

func TestCodexUserMessageClientIDBindsEarlyWireInputWithoutToolProjection(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewCodex(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	input := model.AgentInput{MessageID: "msg-wire", ThreadID: "thread-1"}
	adapter.stageWireInput(input)

	adapter.handleItem("item/started", json.RawMessage(`{
		"turnId":"turn-wire",
		"item":{"id":"item-user","type":"userMessage","clientId":"msg-wire"}
	}`))

	bound := adapter.turnInputs["turn-wire"]
	if len(bound) != 1 || bound[0].MessageID != input.MessageID {
		t.Fatalf("wire input was not correlated: %#v", bound)
	}
	processing := false
	for _, event := range events {
		if event.Kind == model.RuntimeToolStarted || event.Kind == model.RuntimeToolCompleted {
			t.Fatalf("userMessage must not be projected as a tool event: %#v", event)
		}
		if event.Kind == model.RuntimeInputProcessing && event.CorrelationID == input.MessageID && event.TurnID == "turn-wire" {
			processing = true
		}
	}
	if !processing {
		t.Fatalf("userMessage acknowledgement was not projected: %#v", events)
	}
}

func TestCodexSandboxNormalization(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		wantThread string
		wantTurn   string
	}{
		{name: "default", wantThread: "workspace-write", wantTurn: "workspaceWrite"},
		{name: "workspace camel case", value: "workspaceWrite", wantThread: "workspace-write", wantTurn: "workspaceWrite"},
		{name: "workspace kebab case", value: "workspace-write", wantThread: "workspace-write", wantTurn: "workspaceWrite"},
		{name: "read only camel case", value: "readOnly", wantThread: "read-only", wantTurn: "readOnly"},
		{name: "read only kebab case", value: "read-only", wantThread: "read-only", wantTurn: "readOnly"},
		{name: "danger camel case", value: "dangerFullAccess", wantThread: "danger-full-access", wantTurn: "dangerFullAccess"},
		{name: "danger kebab case", value: "danger-full-access", wantThread: "danger-full-access", wantTurn: "dangerFullAccess"},
		{name: "full access alias", value: "full-access", wantThread: "danger-full-access", wantTurn: "dangerFullAccess"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := NewCodex(Config{Sandbox: test.value, Repo: "/repo"}, func(model.RuntimeEvent) {})
			if got := adapter.threadStartParams()["sandbox"]; got != test.wantThread {
				t.Fatalf("thread/start sandbox = %#v, want %q", got, test.wantThread)
			}
			if got := adapter.turnStartParams("thread-1", "hello", model.AgentInput{Role: model.RoleDriver})["sandboxPolicy"].(map[string]any)["type"]; got != test.wantTurn {
				t.Fatalf("turn/start sandboxPolicy.type = %#v, want %q", got, test.wantTurn)
			}
		})
	}

	adapter := NewCodex(Config{Sandbox: "dangerFullAccess", Repo: "/repo"}, func(model.RuntimeEvent) {})
	reviewerPolicy := adapter.sandboxPolicy(model.RoleReviewer)
	if got := reviewerPolicy["type"]; got != "readOnly" {
		t.Fatalf("reviewer must remain readOnly, got %#v", got)
	}
	if len(reviewerPolicy) != 1 {
		t.Fatalf("readOnly policy must not include workspace-write fields: %#v", reviewerPolicy)
	}
}

func TestCodexPlanDeltaUsesCurrentNotificationAndMessageCorrelation(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewCodex(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.turnInputs["turn-plan"] = []model.AgentInput{{MessageID: "msg-plan"}}

	adapter.handleNotification("item/plan/delta", json.RawMessage(`{
		"threadId":"thread-1",
		"turnId":"turn-plan",
		"itemId":"item-plan",
		"delta":"Inspect the call graph"
	}`))

	if len(events) != 1 {
		t.Fatalf("expected one plan event, got %#v", events)
	}
	event := events[0]
	if event.Kind != model.RuntimePlanUpdated || event.TurnID != "turn-plan" || event.ItemID != "item-plan" || event.Text != "Inspect the call graph" || event.CorrelationID != "msg-plan" {
		t.Fatalf("unexpected plan delta projection: %#v", event)
	}
}

func TestCodexErrorNotificationIsNonTerminalDiagnostic(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewCodex(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.state = model.StateWorking
	adapter.currentTurn = "turn-active"
	adapter.turnInputs["turn-active"] = []model.AgentInput{{MessageID: "msg-active"}}

	adapter.handleNotification("error", json.RawMessage(`{
		"message":"tool stream temporarily unavailable"
	}`))

	if len(events) != 1 {
		t.Fatalf("error notification events = %#v", events)
	}
	event := events[0]
	if event.Kind != model.RuntimeError || event.Name != "error" || event.TurnID != "turn-active" || event.CorrelationID != "msg-active" || event.Text != "tool stream temporarily unavailable" {
		t.Fatalf("error notification became terminal: %#v", event)
	}
	if adapter.state != model.StateWorking || adapter.currentTurn != "turn-active" {
		t.Fatalf("diagnostic error mutated active turn: state=%q turn=%q", adapter.state, adapter.currentTurn)
	}
}

func TestCodexCompletedTurnSettlesEverySteeredInput(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewCodex(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.turnInputs["turn-1"] = []model.AgentInput{
		{MessageID: "msg-start", ThreadID: "thread-1"},
		{MessageID: "msg-steer", ThreadID: "thread-1"},
	}
	adapter.turnFinal["turn-1"] = "final answer"
	adapter.currentTurn = "turn-1"

	adapter.handleTurnCompleted(json.RawMessage(`{"turn":{"id":"turn-1","status":"success"}}`))

	completed := map[string]bool{}
	var final *model.RuntimeEvent
	for i := range events {
		event := &events[i]
		if event.Kind == model.RuntimeInputCompleted {
			completed[event.CorrelationID] = true
		}
		if event.Kind == model.RuntimeFinal {
			final = event
		}
	}
	if !completed["msg-start"] || !completed["msg-steer"] || len(completed) != 2 {
		t.Fatalf("not every input was settled: %#v", events)
	}
	if final == nil || final.Text != "final answer" || final.CorrelationID != "msg-steer" {
		t.Fatalf("final answer was not correlated to the latest intervention: %#v", final)
	}
	if adapter.currentTurn != "" || len(adapter.turnInputs) != 0 {
		t.Fatalf("turn bookkeeping was not cleared: current=%q inputs=%#v", adapter.currentTurn, adapter.turnInputs)
	}
}

func TestCodexFailedTurnFailsQueuedInputs(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewCodex(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.turnInputs["turn-1"] = []model.AgentInput{{MessageID: "msg-active"}}
	adapter.queued = []model.AgentInput{{MessageID: "msg-queued"}}
	adapter.currentTurn = "turn-1"

	adapter.handleTurnCompleted(json.RawMessage(`{"turn":{"id":"turn-1","status":"failed","error":{"message":"sandbox failed"}}}`))

	failed := map[string]string{}
	for _, event := range events {
		if event.Kind == model.RuntimeInputFailed {
			failed[event.CorrelationID] = event.Text
		}
	}
	if failed["msg-active"] != "sandbox failed" {
		t.Fatalf("active input did not receive native failure: %#v", failed)
	}
	if failed["msg-queued"] == "" {
		t.Fatalf("queued input was left unresolved: %#v", failed)
	}
	if len(adapter.queued) != 0 {
		t.Fatalf("queued bookkeeping was not drained: %#v", adapter.queued)
	}
}

func TestCodexProcessExitEmitsBoundaryAfterOutstandingInputsFail(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewCodex(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.state = model.StateWorking
	adapter.currentTurn = "turn-active"
	adapter.turnInputs["turn-active"] = []model.AgentInput{{MessageID: "msg-active"}}
	adapter.queued = []model.AgentInput{{MessageID: "msg-queued"}}

	adapter.handleUnexpectedProcessExit(errors.New("exit status 1"))

	failed := map[string]bool{}
	lastFailure := -1
	boundary := -1
	for i, event := range events {
		switch event.Kind {
		case model.RuntimeInputFailed:
			failed[event.CorrelationID] = true
			lastFailure = i
			if event.CorrelationID == "msg-active" && event.TurnID != "turn-active" {
				t.Fatalf("active process-exit failure lost turn correlation: %#v", event)
			}
		case model.RuntimeTurnCompleted:
			if boundary >= 0 {
				t.Fatalf("multiple process-exit boundaries: %#v", events)
			}
			boundary = i
			if event.TurnID != "turn-active" || event.CorrelationID != "msg-active" || event.Name != "process_exited" {
				t.Fatalf("unexpected process-exit boundary: %#v", event)
			}
		}
	}
	if !failed["msg-active"] || !failed["msg-queued"] {
		t.Fatalf("process exit did not settle every outstanding input: %#v", events)
	}
	if boundary <= lastFailure {
		t.Fatalf("process-exit boundary must follow input settlement: failures=%d boundary=%d events=%#v", lastFailure, boundary, events)
	}
	if adapter.state != model.StateError || adapter.currentTurn != "" || len(adapter.turnInputs) != 0 || len(adapter.queued) != 0 {
		t.Fatalf("process exit left stale adapter state: state=%q turn=%q inputs=%#v queued=%#v", adapter.state, adapter.currentTurn, adapter.turnInputs, adapter.queued)
	}
}

func TestCodexProcessExitClosesUntrackedActiveTurn(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewCodex(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.state = model.StateWorking
	adapter.currentTurn = "turn-untracked"

	adapter.handleUnexpectedProcessExit(nil)

	var boundaries []model.RuntimeEvent
	for _, event := range events {
		if event.Kind == model.RuntimeTurnCompleted {
			boundaries = append(boundaries, event)
		}
	}
	if len(boundaries) != 1 || boundaries[0].TurnID != "turn-untracked" || boundaries[0].Name != "process_exited" {
		t.Fatalf("untracked active turn did not receive process-exit boundary: %#v", events)
	}
	if adapter.state != model.StateError || adapter.currentTurn != "" {
		t.Fatalf("active process exit left stale adapter state: state=%q turn=%q", adapter.state, adapter.currentTurn)
	}
}

func TestCodexFailPendingRPCsClearsConnectionState(t *testing.T) {
	adapter := NewCodex(Config{}, func(model.RuntimeEvent) {})
	ch := make(chan rpcReply, 1)
	adapter.pending[101] = ch
	adapter.approvals["approval-1"] = pendingApproval{approval: model.Approval{ID: "approval-1"}}

	adapter.failPendingRPCs("runtime stopped")

	select {
	case reply := <-ch:
		if reply.err == nil || reply.err.Error() != "runtime stopped" {
			t.Fatalf("unexpected pending RPC result: %#v", reply)
		}
	default:
		t.Fatal("pending RPC caller was not released")
	}
	if len(adapter.pending) != 0 || len(adapter.approvals) != 0 {
		t.Fatalf("connection-scoped state was not cleared: pending=%d approvals=%d", len(adapter.pending), len(adapter.approvals))
	}
}

func TestCodexRoleChangeRequiresSafeTurnBoundary(t *testing.T) {
	adapter := NewCodex(Config{}, func(model.RuntimeEvent) {})
	if err := adapter.SetRole(context.Background(), model.RoleReviewer); err != nil {
		t.Fatalf("idle role change failed: %v", err)
	}
	if err := adapter.SetRole(context.Background(), model.ParticipantRole("invalid")); err == nil {
		t.Fatal("expected invalid role rejection")
	}

	adapter.mu.Lock()
	adapter.state = model.StateWorking
	adapter.currentTurn = "turn-active"
	adapter.mu.Unlock()
	if err := adapter.SetRole(context.Background(), model.RoleDriver); err == nil || !strings.Contains(err.Error(), "interrupt or stop") {
		t.Fatalf("expected active-turn rejection, got %v", err)
	}

	adapter.mu.Lock()
	adapter.state = model.StateIdle
	adapter.currentTurn = ""
	adapter.queued = []model.AgentInput{{MessageID: "queued"}}
	adapter.mu.Unlock()
	if err := adapter.SetRole(context.Background(), model.RoleReviewer); err == nil {
		t.Fatal("expected queued-input role rejection")
	}
}
