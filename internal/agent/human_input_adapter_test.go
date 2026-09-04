package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

type humanInputFakeAdapter struct {
	actor model.ActorID
	mu    sync.Mutex
	role  model.ParticipantRole
	input model.AgentInput
}

func (f *humanInputFakeAdapter) Actor() model.ActorID        { return f.actor }
func (f *humanInputFakeAdapter) Start(context.Context) error { return nil }
func (f *humanInputFakeAdapter) StartTurn(_ context.Context, input model.AgentInput) error {
	f.mu.Lock()
	f.input = input
	f.mu.Unlock()
	return nil
}
func (f *humanInputFakeAdapter) Steer(_ context.Context, input model.AgentInput) SteerOutcome {
	f.mu.Lock()
	f.input = input
	f.mu.Unlock()
	return SteerOutcome{State: SteerAccepted}
}
func (f *humanInputFakeAdapter) Interrupt(context.Context) error { return nil }
func (f *humanInputFakeAdapter) Stop(context.Context) error      { return nil }
func (f *humanInputFakeAdapter) ResolveApproval(context.Context, string, model.ApprovalResolution) error {
	return nil
}
func (f *humanInputFakeAdapter) SetRole(_ context.Context, role model.ParticipantRole) error {
	f.mu.Lock()
	f.role = role
	f.mu.Unlock()
	return nil
}
func (f *humanInputFakeAdapter) SetWorkspace(context.Context, string) error { return nil }
func (f *humanInputFakeAdapter) State() model.AgentState                    { return model.StateIdle }
func (f *humanInputFakeAdapter) SessionID() string                          { return "fake" }

func TestHumanInputAdapterAppliesTurnRoleWithoutWorkflowPolicy(t *testing.T) {
	fake := &humanInputFakeAdapter{actor: model.ActorClaude}
	wrapper := &humanInputAdapter{
		actor: model.ActorClaude, inner: fake, sink: func(model.RuntimeEvent) {},
		turnInput: map[string]model.AgentInput{}, pausedTurns: map[string]struct{}{},
	}
	input := model.AgentInput{MessageID: "m1", Role: model.RoleReviewer, Text: "review exactly this"}
	if err := wrapper.StartTurn(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if fake.role != model.RoleReviewer || fake.input.Text != input.Text {
		t.Fatalf("role bridge changed ordinary input: role=%s input=%#v", fake.role, fake.input)
	}
}

func TestHumanInputAdapterPreservesExplicitReviewerNativePolicy(t *testing.T) {
	fake := &humanInputFakeAdapter{actor: model.ActorCodex}
	wrapper := &humanInputAdapter{
		cfg:         Config{OrdinaryReviewerPolicy: model.ReviewerExplicit},
		actor:       model.ActorCodex,
		inner:       fake,
		sink:        func(model.RuntimeEvent) {},
		turnInput:   map[string]model.AgentInput{},
		pausedTurns: map[string]struct{}{},
	}
	input := model.AgentInput{MessageID: "m-explicit", Role: model.RoleReviewer, Text: "inspect in the isolated review workspace"}
	if err := wrapper.StartTurn(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if fake.role != model.RoleDriver || fake.input.Role != model.RoleReviewer {
		t.Fatalf("explicit Reviewer policy did not separate native and durable roles: native=%s input=%s", fake.role, fake.input.Role)
	}
}

func TestHiddenRuntimeQuestionsBecomeVisibleUserRequests(t *testing.T) {
	tests := []struct {
		name        string
		runtimeKind model.RuntimeKind
		method      string
		payload     map[string]any
		want        string
	}{
		{
			name: "codex", runtimeKind: model.RuntimeCodex, method: "item/tool/requestUserInput", want: "Choose the API",
			payload: map[string]any{"questions": []map[string]any{{"question": "Choose the API", "options": []map[string]string{{"label": "A"}, {"label": "B"}}}}},
		},
		{
			name: "grok", runtimeKind: model.RuntimeGrok, method: "x.ai/ask_user_question", want: "Choose the banner",
			payload: map[string]any{"questions": []map[string]any{{"question": "Choose the banner", "multiSelect": false, "options": []map[string]string{{"label": "Blue", "description": "Use blue"}}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &humanInputFakeAdapter{actor: model.ActorCodex}
			var events []model.RuntimeEvent
			wrapper := &humanInputAdapter{
				cfg: Config{Runtime: tt.runtimeKind}, actor: model.ActorCodex, inner: fake,
				sink:      func(event model.RuntimeEvent) { events = append(events, event) },
				turnInput: map[string]model.AgentInput{}, pausedTurns: map[string]struct{}{},
				latestInput: model.AgentInput{MessageID: "m1"}, activeTurn: "turn-1",
			}
			raw, err := json.Marshal(tt.payload)
			if err != nil {
				t.Fatal(err)
			}
			wrapper.handleEvent(model.RuntimeEvent{
				Agent: model.ActorCodex, Kind: model.RuntimeLog, Name: "server_request.unsupported",
				Text: tt.method, Data: raw,
			})
			var visible *model.RuntimeEvent
			for index := range events {
				if events[index].Kind == model.RuntimeFinal {
					visible = &events[index]
				}
			}
			if visible == nil || !strings.Contains(visible.Text, tt.want) || !strings.HasSuffix(visible.Text, "@user") {
				t.Fatalf("visible question event not emitted: %#v", events)
			}
			if strings.Contains(visible.Text, "PAIRROOM:") || strings.Contains(visible.Text, "@human") {
				t.Fatalf("removed control syntax leaked into question: %q", visible.Text)
			}
		})
	}
}
