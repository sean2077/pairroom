package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// workflowAdapter projects a compiled stage into native policy without
// replacing either vendor harness. Claude uses native plan mode for
// read-only stages; Codex receives a per-turn read-only sandbox role.
type workflowAdapter struct {
	cfg   Config
	actor model.ActorID
	inner Adapter
	sink  EventSink

	mu          sync.Mutex
	latestInput model.AgentInput
	activeTurn  string
	turnInput   map[string]model.AgentInput
	pausedTurns map[string]struct{}
}

func newWorkflowAdapter(cfg Config, actor model.ActorID, sink EventSink, build func(EventSink) Adapter) Adapter {
	adapter := &workflowAdapter{
		cfg: cfg, actor: actor, sink: sink,
		turnInput: make(map[string]model.AgentInput), pausedTurns: make(map[string]struct{}),
	}
	adapter.inner = build(adapter.handleEvent)
	return adapter
}

func (a *workflowAdapter) Actor() model.ActorID                { return a.inner.Actor() }
func (a *workflowAdapter) State() model.AgentState             { return a.inner.State() }
func (a *workflowAdapter) SessionID() string                   { return a.inner.SessionID() }
func (a *workflowAdapter) Start(ctx context.Context) error     { return a.inner.Start(ctx) }
func (a *workflowAdapter) Interrupt(ctx context.Context) error { return a.inner.Interrupt(ctx) }
func (a *workflowAdapter) Stop(ctx context.Context) error      { return a.inner.Stop(ctx) }
func (a *workflowAdapter) ResolveApproval(ctx context.Context, id string, resolution model.ApprovalResolution) error {
	return a.inner.ResolveApproval(ctx, id, resolution)
}
func (a *workflowAdapter) SetRole(ctx context.Context, role model.ParticipantRole) error {
	return a.inner.SetRole(ctx, role)
}
func (a *workflowAdapter) SetWorkspace(ctx context.Context, workspace string) error {
	return a.inner.SetWorkspace(ctx, workspace)
}

func (a *workflowAdapter) Submit(ctx context.Context, input model.AgentInput) (model.DeliveryState, error) {
	if input.WorkflowMode.Valid() {
		input.Role = workflowNativeRole(input.WorkflowMode, input.Role)
		input.Text = workflowRuntimeInstruction(input) + "\n\n" + input.Text
		if a.actor == model.ActorClaude {
			if err := a.inner.SetRole(ctx, input.Role); err != nil {
				return model.DeliveryFailed, fmt.Errorf("apply Claude workflow mode %s: %w", input.WorkflowMode, err)
			}
		}
	}
	a.mu.Lock()
	a.latestInput = input
	a.mu.Unlock()
	return a.inner.Submit(ctx, input)
}

func workflowNativeRole(mode model.WorkflowMode, fallback model.ParticipantRole) model.ParticipantRole {
	switch mode {
	case model.WorkflowPlan, model.WorkflowReview, model.WorkflowAudit:
		return model.RoleReviewer
	case model.WorkflowExecute:
		return model.RoleDriver
	default:
		return fallback
	}
}

func workflowRuntimeInstruction(input model.AgentInput) string {
	policy := "Discuss and investigate only the requested stage."
	switch input.WorkflowMode {
	case model.WorkflowPlan:
		policy = "Plan only. Inspect as needed, but do not edit files or execute the implementation."
	case model.WorkflowReview:
		policy = "Review independently and read-only. Do not edit files. Report concrete concerns or approval."
	case model.WorkflowAudit:
		policy = "Audit the completed implementation independently and read-only. Do not edit files."
	case model.WorkflowExecute:
		policy = "Execute the approved plan, test the result, and report changed scope and evidence."
	}
	return fmt.Sprintf(`[PairRoom compiled workflow stage]
workflow_id: %s
stage_index: %d
mode: %s
%s
Do not perform later stages early. When a human choice is needed, ask visibly in the final room response with @human and [PAIRROOM:WAIT]. Do not call a hidden request_user_input or MCP elicitation tool.`, input.WorkflowID, input.WorkflowStage+1, input.WorkflowMode, policy)
}

func (a *workflowAdapter) handleEvent(event model.RuntimeEvent) {
	a.mu.Lock()
	switch event.Kind {
	case model.RuntimeTurnStarted:
		if event.TurnID != "" {
			a.activeTurn = event.TurnID
			if event.CorrelationID != "" {
				input := a.latestInput
				if input.MessageID == event.CorrelationID {
					a.turnInput[event.TurnID] = input
				}
			}
		}
	case model.RuntimeTurnCompleted:
		if event.TurnID != "" {
			delete(a.turnInput, event.TurnID)
			if a.activeTurn == event.TurnID {
				a.activeTurn = ""
			}
		}
	}
	_, paused := a.pausedTurns[event.TurnID]
	if event.Kind == model.RuntimeFinal && paused {
		a.mu.Unlock()
		return
	}
	if event.Kind == model.RuntimeTurnCompleted && paused {
		delete(a.pausedTurns, event.TurnID)
	}
	a.mu.Unlock()

	a.sink(event)
	if a.actor != model.ActorCodex || event.Kind != model.RuntimeLog || event.Name != "server_request.unsupported" || !visibleCodexRequest(event.Text) {
		return
	}
	a.surfaceCodexQuestion(event)
}

func visibleCodexRequest(method string) bool {
	return strings.Contains(method, "tool/requestUserInput") || strings.Contains(method, "elicitation/request")
}

func (a *workflowAdapter) surfaceCodexQuestion(event model.RuntimeEvent) {
	a.mu.Lock()
	turnID := event.TurnID
	if turnID == "" {
		turnID = a.activeTurn
	}
	input := a.turnInput[turnID]
	if input.MessageID == "" {
		input = a.latestInput
	}
	if turnID != "" {
		a.pausedTurns[turnID] = struct{}{}
	}
	a.mu.Unlock()

	question := codexVisibleQuestion(event.Text, event.Data)
	final := runtimeEvent(model.ActorCodex, model.RuntimeFinal)
	final.TurnID = turnID
	final.CorrelationID = input.MessageID
	final.Text = question + "\n\nReply in the shared room; PairRoom will resume this workflow stage as a new native turn.\n\n@human\n[PAIRROOM:WAIT]"
	a.sink(final)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.inner.Interrupt(ctx)
	}()
}

func codexVisibleQuestion(method string, raw json.RawMessage) string {
	if strings.Contains(method, "tool/requestUserInput") {
		var payload struct {
			Questions []struct {
				ID       string `json:"id"`
				Header   string `json:"header"`
				Question string `json:"question"`
				Options  []struct {
					Label       string `json:"label"`
					Description string `json:"description"`
				} `json:"options"`
			} `json:"questions"`
		}
		if json.Unmarshal(raw, &payload) == nil && len(payload.Questions) > 0 {
			var b strings.Builder
			b.WriteString("Codex needs a human decision before it can continue:\n")
			for index, question := range payload.Questions {
				text := strings.TrimSpace(question.Question)
				if text == "" {
					text = strings.TrimSpace(question.Header)
				}
				fmt.Fprintf(&b, "\n%d. %s", index+1, text)
				for _, option := range question.Options {
					fmt.Fprintf(&b, "\n   - %s", option.Label)
					if option.Description != "" {
						fmt.Fprintf(&b, ": %s", option.Description)
					}
				}
			}
			return b.String()
		}
	}
	var elicitation struct {
		Message string `json:"message"`
		URL     string `json:"url"`
	}
	if json.Unmarshal(raw, &elicitation) == nil && strings.TrimSpace(elicitation.Message) != "" {
		text := "Codex needs human input: " + strings.TrimSpace(elicitation.Message)
		if elicitation.URL != "" {
			text += "\nReference: " + elicitation.URL
		}
		return text
	}
	return "Codex requested interactive input that its headless runtime could not expose. Please provide the missing decision or clarification."
}
