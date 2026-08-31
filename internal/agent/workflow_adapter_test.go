package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

type workflowFakeAdapter struct {
	actor model.ActorID
	role  model.ParticipantRole
	input model.AgentInput
}

func (f *workflowFakeAdapter) Actor() model.ActorID        { return f.actor }
func (f *workflowFakeAdapter) Start(context.Context) error { return nil }
func (f *workflowFakeAdapter) Submit(_ context.Context, input model.AgentInput) (model.DeliveryState, error) {
	f.input = input
	return model.DeliveryStarted, nil
}
func (f *workflowFakeAdapter) Interrupt(context.Context) error { return nil }
func (f *workflowFakeAdapter) Stop(context.Context) error      { return nil }
func (f *workflowFakeAdapter) ResolveApproval(context.Context, string, model.ApprovalResolution) error {
	return nil
}
func (f *workflowFakeAdapter) SetRole(_ context.Context, role model.ParticipantRole) error {
	f.role = role
	return nil
}
func (f *workflowFakeAdapter) SetWorkspace(context.Context, string) error { return nil }
func (f *workflowFakeAdapter) State() model.AgentState                    { return model.StateIdle }
func (f *workflowFakeAdapter) SessionID() string                          { return "fake" }

func TestWorkflowAdapterProjectsReadOnlyAndExecuteModes(t *testing.T) {
	fake := &workflowFakeAdapter{actor: model.ActorClaude}
	wrapper := &workflowAdapter{actor: model.ActorClaude, inner: fake, sink: func(model.RuntimeEvent) {}, turnInput: map[string]model.AgentInput{}, pausedTurns: map[string]struct{}{}}
	input := model.AgentInput{MessageID: "m1", WorkflowID: "w", WorkflowMode: model.WorkflowPlan, WorkflowStage: 0, Role: model.RoleDriver, Text: "plan"}
	if _, err := wrapper.Submit(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if fake.role != model.RoleReviewer || fake.input.Role != model.RoleReviewer || !strings.Contains(fake.input.Text, "Plan only") {
		t.Fatalf("plan projection: role=%s input=%#v", fake.role, fake.input)
	}
	ordinary := model.AgentInput{MessageID: "m2", Role: model.RoleDriver, Text: "ordinary turn"}
	if _, err := wrapper.Submit(context.Background(), ordinary); err != nil {
		t.Fatal(err)
	}
	if fake.role != model.RoleDriver || fake.input.Role != model.RoleDriver || fake.input.Text != ordinary.Text {
		t.Fatalf("ordinary role restoration: role=%s input=%#v", fake.role, fake.input)
	}
	input.WorkflowMode = model.WorkflowExecute
	input.Text = "execute"
	if _, err := wrapper.Submit(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if fake.role != model.RoleDriver || fake.input.Role != model.RoleDriver || !strings.Contains(fake.input.Text, "approved plan") {
		t.Fatalf("execute projection: role=%s input=%#v", fake.role, fake.input)
	}
}

func TestCodexHiddenQuestionBecomesVisibleWait(t *testing.T) {
	fake := &workflowFakeAdapter{actor: model.ActorCodex}
	var events []model.RuntimeEvent
	wrapper := &workflowAdapter{actor: model.ActorCodex, inner: fake, sink: func(event model.RuntimeEvent) { events = append(events, event) }, turnInput: map[string]model.AgentInput{}, pausedTurns: map[string]struct{}{}}
	wrapper.latestInput = model.AgentInput{MessageID: "m1"}
	wrapper.activeTurn = "turn-1"
	raw, _ := json.Marshal(map[string]any{"questions": []map[string]any{{"id": "q", "question": "Choose the API", "options": []map[string]string{{"label": "A"}, {"label": "B"}}}}})
	wrapper.handleEvent(model.RuntimeEvent{Agent: model.ActorCodex, Kind: model.RuntimeLog, Name: "server_request.unsupported", Text: "item/tool/requestUserInput", Data: raw})
	found := false
	for _, event := range events {
		if event.Kind == model.RuntimeFinal && strings.Contains(event.Text, "Choose the API") && strings.Contains(event.Text, "@human") && strings.Contains(event.Text, "[PAIRROOM:WAIT]") {
			found = true
		}
	}
	if !found {
		t.Fatalf("visible question event not emitted: %#v", events)
	}
}
