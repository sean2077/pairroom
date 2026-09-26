package agent

import (
	"encoding/json"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

func (g *GrokAdapter) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "session/update", "prompt/update":
		g.handleSessionUpdate(params)
	default:
		e := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		e.Name = method
		e.Data = g.redactRaw(params)
		g.sink(e)
	}
}

func (g *GrokAdapter) handleSessionUpdate(raw json.RawMessage) {
	var params struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind    string `json:"sessionUpdate"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Status     string `json:"status"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return
	}
	g.mu.Lock()
	turn := g.turn
	rootSession := g.sessionID
	if turn == nil || params.SessionID != rootSession {
		g.mu.Unlock()
		return
	}
	turnID := turn.turnID
	correlationID := turn.inputs[len(turn.inputs)-1].MessageID
	if params.Update.Kind == "agent_message_chunk" && params.Update.Content.Type == "text" {
		turn.final.WriteString(redactRuntimeSecrets(params.Update.Content.Text, g.cfg.Env))
	}
	g.mu.Unlock()

	event := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
	event.TurnID = turnID
	event.CorrelationID = correlationID
	event.Name = params.Update.Kind
	event.Data = g.redactRaw(raw)
	switch params.Update.Kind {
	case "agent_message_chunk":
		if params.Update.Content.Type != "text" || params.Update.Content.Text == "" {
			return
		}
		event.Kind = model.RuntimeTextDelta
		event.Text = redactRuntimeSecrets(params.Update.Content.Text, g.cfg.Env)
	case "tool_call":
		event.Kind = model.RuntimeToolStarted
		event.ItemID = params.Update.ToolCallID
		event.Name = params.Update.Title
	case "tool_call_update":
		event.Kind = model.RuntimeToolCompleted
		event.ItemID = params.Update.ToolCallID
		event.Name = params.Update.Status
	case "plan":
		event.Kind = model.RuntimePlanUpdated
	default:
		if strings.Contains(params.Update.Kind, "usage") {
			event.Kind = model.RuntimeUsageUpdated
		}
	}
	g.sink(event)
}
