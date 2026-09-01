package room

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/version"
)

func TestCompileNaturalWorkflow(t *testing.T) {
	workflow, ok := compileWorkflow("Claude 规划，Codex review，Codex 执行，Claude 审查")
	if !ok {
		t.Fatal("expected workflow to compile")
	}
	wantActors := []model.ActorID{model.ActorClaude, model.ActorCodex, model.ActorCodex, model.ActorClaude}
	wantModes := []model.WorkflowMode{model.WorkflowPlan, model.WorkflowReview, model.WorkflowExecute, model.WorkflowReview}
	if len(workflow.Stages) != len(wantActors) {
		t.Fatalf("stages = %#v", workflow.Stages)
	}
	for index := range wantActors {
		if workflow.Stages[index].Actor != wantActors[index] || workflow.Stages[index].Mode != wantModes[index] {
			t.Fatalf("stage %d = %#v", index, workflow.Stages[index])
		}
	}
	if !workflow.RequiresApproval || workflow.Status != model.WorkflowStatusRunning || workflow.Stages[0].Status != model.WorkflowStageRunning {
		t.Fatalf("unexpected workflow state: %#v", workflow)
	}
	if workflow.Revision != 0 {
		t.Fatalf("uncompleted plan revision = %d, want 0", workflow.Revision)
	}
}

func TestCompileWorkflowSupportsExplicitNoGate(t *testing.T) {
	workflow, ok := compileWorkflow("Claude plan, Codex execute，无需审批")
	if !ok || workflow.RequiresApproval {
		t.Fatalf("explicit no-gate workflow = %#v, %v", workflow, ok)
	}
}

func TestCompileWorkflowPreservesNegatedApprovalGate(t *testing.T) {
	workflow, ok := compileWorkflow("Claude plan, Codex execute. Do not execute without approval.")
	if !ok || !workflow.RequiresApproval {
		t.Fatalf("negated no-gate workflow = %#v, %v", workflow, ok)
	}
}

func TestCompileNaturalWorkflowSupportsDiscuss(t *testing.T) {
	workflow, ok := compileWorkflow("Claude discuss, Codex execute")
	if !ok {
		t.Fatal("expected discuss workflow to compile")
	}
	if got := workflow.Stages[0].Mode; got != model.WorkflowDiscuss {
		t.Fatalf("first mode = %q, want %q", got, model.WorkflowDiscuss)
	}
}

func TestOrdinaryConversationDoesNotCompileWorkflow(t *testing.T) {
	for _, text := range []string{"@claude please review this", "Let Claude and Codex discuss", "Codex execute this fix"} {
		if workflow, ok := compileWorkflow(text); ok {
			t.Fatalf("%q unexpectedly compiled: %#v", text, workflow)
		}
	}
}

func TestWorkflowApprovalAndRejectDetection(t *testing.T) {
	if !workflowApprovalPattern.MatchString("批准执行当前计划") || !workflowApprovalPattern.MatchString("go ahead") {
		t.Fatal("expected approval phrases")
	}
	for _, text := range []string{"not approved", "不要执行，取消流程"} {
		if !workflowRejectPattern.MatchString(text) {
			t.Fatalf("expected rejection phrase %q", text)
		}
	}
	for _, text := range []string{"not approved", "this is approved for documentation", "Do not execute without approval"} {
		if workflowApprovalPattern.MatchString(text) {
			t.Fatalf("%q unexpectedly approved the workflow", text)
		}
	}
}

func TestActiveWorkflowPreservesExplicitRouting(t *testing.T) {
	engine, _ := newTestEngine(t, model.RoutingTurns, "")
	workflow, ok := compileWorkflow("Claude plan, Codex execute")
	if !ok {
		t.Fatal("expected workflow to compile")
	}
	engine.mu.Lock()
	engine.snapshot.Workflow = workflow
	engine.snapshot.Messages = append(engine.snapshot.Messages, model.Message{ID: "codex-response", From: model.ActorCodex})
	engine.mu.Unlock()

	for _, test := range []struct {
		name string
		text string
		req  SendRequest
		want bool
	}{
		{name: "unaddressed stage continuation", text: "continue", want: true},
		{name: "stage actor recipient", text: "continue", req: SendRequest{To: []model.ActorID{model.ActorClaude}}, want: true},
		{name: "other recipient", text: "unrelated", req: SendRequest{To: []model.ActorID{model.ActorCodex}}, want: false},
		{name: "other role", text: "unrelated", req: SendRequest{TargetRole: model.RoleReviewer}, want: false},
		{name: "other mention", text: "@codex unrelated", want: false},
		{name: "other reply", text: "follow up", req: SendRequest{ReplyTo: "codex-response"}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := engine.workflowRequestTargetsStage(test.text, test.req, model.ActorClaude); got != test.want {
				t.Fatalf("workflowRequestTargetsStage() = %v, want %v", got, test.want)
			}
		})
	}

	message, err := engine.Send(context.Background(), SendRequest{Text: "@codex unrelated"})
	if err != nil {
		t.Fatal(err)
	}
	if message.WorkflowID != "" || len(message.To) != 1 || message.To[0] != model.ActorCodex {
		t.Fatalf("explicitly routed message = %#v", message)
	}
}

func TestWorkflowStateReplaysUnderCurrentStoreSchema(t *testing.T) {
	if version.StoreSchema < 8 {
		t.Fatalf("Store schema = %d, workflow events require at least 8", version.StoreSchema)
	}
	dir := t.TempDir()
	engine, _ := newTestEngine(t, model.RoutingTurns, dir)
	workflow, ok := compileWorkflow("Claude plan, Codex execute")
	if !ok {
		t.Fatal("expected workflow to compile")
	}
	if _, err := engine.record(EventWorkflowUpdated, model.ActorUser, *workflow); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _ := newTestEngine(t, model.RoutingTurns, dir)
	got := reopened.Snapshot().Workflow
	if got == nil || got.ID != workflow.ID || len(got.Stages) != len(workflow.Stages) {
		t.Fatalf("replayed workflow = %#v, want %#v", got, workflow)
	}
}

func TestWorkflowDoneSignalAdvancesExplicitSequence(t *testing.T) {
	engine, _ := newTestEngine(t, model.RoutingTurns, "")
	workflow, ok := compileWorkflow("Claude plan, Codex execute")
	if !ok {
		t.Fatal("expected workflow to compile")
	}
	if _, err := engine.record(EventWorkflowUpdated, model.ActorUser, *workflow); err != nil {
		t.Fatal(err)
	}
	incoming := model.Message{ID: "workflow-input", WorkflowID: workflow.ID, WorkflowStage: 0, WorkflowMode: model.WorkflowPlan}
	output := model.Message{ID: "workflow-output", From: model.ActorClaude, TurnID: "turn-plan", WorkflowID: workflow.ID, WorkflowStage: 0, WorkflowMode: model.WorkflowPlan}
	if _, err := engine.record(EventMessageCreated, model.ActorClaude, output); err != nil {
		t.Fatal(err)
	}
	engine.workflowOnFinal(incoming, output, "DONE")
	if got := engine.Snapshot().Workflow.Status; got != model.WorkflowStatusRunning {
		t.Fatalf("DONE completed an intermediate stage's workflow: %s", got)
	}
	engine.advanceWorkflow(model.RuntimeEvent{Agent: model.ActorClaude, TurnID: output.TurnID, Name: "completed"})
	got := engine.Snapshot().Workflow
	if got.Status != model.WorkflowStatusAwaitingApproval || got.CurrentStage != 1 || got.Revision != 1 {
		t.Fatalf("advanced workflow = %#v", got)
	}
}

func TestWorkflowWaitsForQueuedStageGuidanceAndHandsOffFinalResult(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingTurns, "")
	initial, err := engine.Send(context.Background(), SendRequest{Text: "请 Claude 规划修复计划，Codex review"})
	if err != nil {
		t.Fatal(err)
	}
	firstInput := receiveInput(t, adapters[model.ActorClaude])
	if firstInput.WorkflowID == "" || firstInput.WorkflowStage != 0 {
		t.Fatalf("initial workflow input = %#v", firstInput)
	}

	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeFinal, CorrelationID: initial.ID,
		TurnID: "plan-first", Text: "Initial plan result.", CreatedAt: time.Now().UTC(),
	})
	late, err := engine.Send(context.Background(), SendRequest{
		Text: "补充：把滚动稳定性也纳入计划", To: []model.ActorID{model.ActorClaude},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorClaude])
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeInputCompleted, CorrelationID: initial.ID,
		TurnID: "plan-first", CreatedAt: time.Now().UTC(),
	})
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeTurnCompleted, CorrelationID: initial.ID,
		TurnID: "plan-first", Name: "success", CreatedAt: time.Now().UTC(),
	})
	select {
	case got := <-adapters[model.ActorCodex].submissions:
		t.Fatalf("workflow advanced before queued stage guidance completed: %#v", got)
	case <-time.After(150 * time.Millisecond):
	}
	if got := engine.Snapshot().Workflow.CurrentStage; got != 0 {
		t.Fatalf("workflow stage advanced early: %d", got)
	}

	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeFinal, CorrelationID: late.ID,
		TurnID: "plan-revised", Text: "Revised plan result with stable scrolling evidence.", CreatedAt: time.Now().UTC(),
	})
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeInputCompleted, CorrelationID: late.ID,
		TurnID: "plan-revised", CreatedAt: time.Now().UTC(),
	})
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeTurnCompleted, CorrelationID: late.ID,
		TurnID: "plan-revised", Name: "success", CreatedAt: time.Now().UTC(),
	})
	next := receiveInput(t, adapters[model.ActorCodex])
	if next.WorkflowID != firstInput.WorkflowID || next.WorkflowStage != 1 {
		t.Fatalf("review stage input = %#v", next)
	}
	if !strings.Contains(next.Text, "Revised plan result with stable scrolling evidence") || strings.Contains(next.Text, "补充：把滚动稳定性也纳入计划") {
		t.Fatalf("workflow handoff used mutable latest message instead of completed result: %q", next.Text)
	}
}

func TestRetryReopensFailedCurrentWorkflowStage(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingTurns, "")
	workflow, ok := compileWorkflow("Claude plan, Codex execute")
	if !ok {
		t.Fatal("expected workflow to compile")
	}
	now := time.Now().UTC()
	workflow.Status = model.WorkflowStatusFailed
	workflow.CompletedAt = &now
	workflow.Stages[0].Status = model.WorkflowStageFailed
	workflow.Stages[0].CompletedAt = &now
	if _, err := engine.record(EventWorkflowUpdated, model.ActorSystem, *workflow); err != nil {
		t.Fatal(err)
	}
	original := model.Message{
		ID: "failed-workflow-input", From: model.ActorUser, To: []model.ActorID{model.ActorClaude}, Text: "Claude plan, Codex execute",
		WorkflowID: workflow.ID, WorkflowStage: 0, WorkflowMode: model.WorkflowPlan, CreatedAt: now,
		Delivery:   map[model.ActorID]model.DeliveryState{model.ActorClaude: model.DeliveryFailed},
		Processing: map[model.ActorID]model.ProcessingState{model.ActorClaude: model.ProcessingFailed},
	}
	if _, err := engine.record(EventMessageCreated, model.ActorUser, original); err != nil {
		t.Fatal(err)
	}
	retry, err := engine.Retry(context.Background(), original.ID, RetryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := engine.Snapshot().Workflow
	if got.Status != model.WorkflowStatusRunning || got.Stages[0].Status != model.WorkflowStageRunning || got.CompletedAt != nil {
		t.Fatalf("reopened workflow = %#v", got)
	}
	if retry.WorkflowID != workflow.ID || retry.WorkflowStage != 0 || retry.WorkflowMode != model.WorkflowPlan {
		t.Fatalf("workflow retry metadata = %#v", retry)
	}
	input := receiveInput(t, adapters[model.ActorClaude])
	if input.MessageID != retry.ID || input.WorkflowID != workflow.ID {
		t.Fatalf("workflow retry input = %#v", input)
	}
	if _, err := engine.Retry(context.Background(), original.ID, RetryRequest{}); err == nil {
		t.Fatal("duplicate in-flight workflow retry succeeded")
	}
}
