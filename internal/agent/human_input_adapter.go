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

// humanInputAdapter keeps native user-input requests visible in the shared
// Room without adding orchestration or replacing runtime approvals.
type humanInputAdapter struct {
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

func newHumanInputAdapter(cfg Config, actor model.ActorID, sink EventSink, build func(EventSink) Adapter) Adapter {
	adapter := &humanInputAdapter{
		cfg: cfg, actor: actor, sink: sink,
		turnInput: make(map[string]model.AgentInput), pausedTurns: make(map[string]struct{}),
	}
	adapter.inner = build(adapter.handleEvent)
	return adapter
}

func (a *humanInputAdapter) Actor() model.ActorID                { return a.inner.Actor() }
func (a *humanInputAdapter) State() model.AgentState             { return a.inner.State() }
func (a *humanInputAdapter) SessionID() string                   { return a.inner.SessionID() }
func (a *humanInputAdapter) Start(ctx context.Context) error     { return a.inner.Start(ctx) }
func (a *humanInputAdapter) Interrupt(ctx context.Context) error { return a.inner.Interrupt(ctx) }
func (a *humanInputAdapter) Stop(ctx context.Context) error      { return a.inner.Stop(ctx) }
func (a *humanInputAdapter) ResolveApproval(ctx context.Context, id string, resolution model.ApprovalResolution) error {
	return a.inner.ResolveApproval(ctx, id, resolution)
}
func (a *humanInputAdapter) SetRole(ctx context.Context, role model.ParticipantRole) error {
	return a.inner.SetRole(ctx, a.nativeRole(role))
}
func (a *humanInputAdapter) SetWorkspace(ctx context.Context, workspace string) error {
	return a.inner.SetWorkspace(ctx, workspace)
}

func (a *humanInputAdapter) StartTurn(ctx context.Context, input model.AgentInput) error {
	nativeRole := a.nativeRole(input.Role)
	if err := a.inner.SetRole(ctx, nativeRole); err != nil {
		return fmt.Errorf("apply input role %s: %w", nativeRole, err)
	}
	a.mu.Lock()
	a.latestInput = input
	a.mu.Unlock()
	return a.inner.StartTurn(ctx, input)
}

// nativeRole preserves the durable Room role in the PairRoom envelope while
// honoring the pre-existing explicit Reviewer policy. An explicitly configured
// Reviewer is still isolated to the Reviewer workspace by the Room engine, but
// its selected native permission/approval/sandbox policy is allowed to govern
// the turn. The default (and every empty direct-test config) remains read-only.
func (a *humanInputAdapter) nativeRole(role model.ParticipantRole) model.ParticipantRole {
	if role == model.RoleReviewer && a.cfg.OrdinaryReviewerPolicy == model.ReviewerExplicit {
		return model.RoleDriver
	}
	return role
}

func (a *humanInputAdapter) Steer(ctx context.Context, input model.AgentInput) SteerOutcome {
	a.mu.Lock()
	a.latestInput = input
	a.mu.Unlock()
	return a.inner.Steer(ctx, input)
}

func (a *humanInputAdapter) handleEvent(event model.RuntimeEvent) {
	a.mu.Lock()
	switch event.Kind {
	case model.RuntimeTurnStarted:
		if event.TurnID != "" {
			a.activeTurn = event.TurnID
			if event.CorrelationID != "" && a.latestInput.MessageID == event.CorrelationID {
				a.turnInput[event.TurnID] = a.latestInput
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
	runtimeKind := a.cfg.Runtime.CanonicalForSlot(a.actor)
	if event.Kind != model.RuntimeLog || event.Name != "server_request.unsupported" || !visibleHumanInputRequest(runtimeKind, event.Text) {
		return
	}
	a.surfaceHumanQuestion(runtimeKind, event)
}

func visibleHumanInputRequest(runtimeKind model.RuntimeKind, method string) bool {
	switch runtimeKind {
	case model.RuntimeCodex:
		return strings.Contains(method, "tool/requestUserInput") || strings.Contains(method, "elicitation/request")
	case model.RuntimeGrok:
		return strings.Contains(method, "ask_user_question")
	default:
		return false
	}
}

func (a *humanInputAdapter) surfaceHumanQuestion(runtimeKind model.RuntimeKind, event model.RuntimeEvent) {
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

	final := runtimeEvent(a.actor, model.RuntimeFinal)
	final.TurnID = turnID
	final.CorrelationID = input.MessageID
	final.Text = humanVisibleQuestion(configuredParticipantName(a.cfg), runtimeKind, event.Text, event.Data) + "\n\nReply in the shared Room so the Agent can continue in a new native turn.\n\n@user"
	a.sink(final)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.inner.Interrupt(ctx)
	}()
}

func humanVisibleQuestion(name string, runtimeKind model.RuntimeKind, method string, raw json.RawMessage) string {
	if strings.TrimSpace(name) == "" {
		name = runtimeKind.DisplayName()
	}
	if runtimeKind == model.RuntimeGrok && strings.Contains(method, "ask_user_question") {
		var payload struct {
			Questions []struct {
				Question    string `json:"question"`
				MultiSelect bool   `json:"multiSelect"`
				Options     []struct {
					Label       string `json:"label"`
					Description string `json:"description"`
				} `json:"options"`
			} `json:"questions"`
		}
		if json.Unmarshal(raw, &payload) == nil && len(payload.Questions) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "%s needs a human decision before it can continue:\n", name)
			for index, question := range payload.Questions {
				fmt.Fprintf(&b, "\n%d. %s", index+1, strings.TrimSpace(question.Question))
				if question.MultiSelect {
					b.WriteString(" (multiple selections allowed)")
				}
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
	if strings.Contains(method, "tool/requestUserInput") {
		var payload struct {
			Questions []struct {
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
			fmt.Fprintf(&b, "%s needs a human decision before it can continue:\n", name)
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
		text := name + " needs human input: " + strings.TrimSpace(elicitation.Message)
		if elicitation.URL != "" {
			text += "\nReference: " + elicitation.URL
		}
		return text
	}
	return name + " requested interactive input that its headless runtime could not expose. Please provide the missing decision or clarification."
}
