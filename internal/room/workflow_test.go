package room

import (
	"testing"

	"github.com/sean2077/pairroom/internal/model"
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
}

func TestCompileWorkflowSupportsExplicitNoGate(t *testing.T) {
	workflow, ok := compileWorkflow("Claude plan, Codex execute，无需审批")
	if !ok || workflow.RequiresApproval {
		t.Fatalf("explicit no-gate workflow = %#v, %v", workflow, ok)
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
	if !workflowRejectPattern.MatchString("不要执行，取消流程") {
		t.Fatal("expected rejection phrase")
	}
}
