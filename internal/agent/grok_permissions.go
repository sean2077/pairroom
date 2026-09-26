package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

type grokPermissionOption struct {
	ID   string `json:"optionId"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type grokPendingApproval struct {
	rawID    json.RawMessage
	kind     string
	options  []grokPermissionOption
	approval model.Approval
}

func grokPermissionArgs(mode string) []string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "auto", "yolo", "bypass", "bypasspermissions", "always-approve", "always_approve":
		return []string{"--always-approve"}
	case "":
		return nil
	default:
		return []string{"--permission-mode", mode}
	}
}

func (g *GrokAdapter) applyAccessMode(ctx context.Context, sessionID string, access model.NativeAccess) error {
	mode := "default"
	if access == model.NativeAccessReadOnly || strings.EqualFold(g.cfg.PermissionMode, "plan") {
		mode = "plan"
	}
	_, err := g.call(ctx, "session/set_mode", map[string]any{"sessionId": sessionID, "modeId": mode})
	if err != nil {
		return fmt.Errorf("set Grok session mode %s: %w", mode, err)
	}
	return nil
}

func (g *GrokAdapter) ResolveApproval(ctx context.Context, approvalID string, resolution model.ApprovalResolution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	pending, ok := g.approvals[approvalID]
	if !ok {
		g.mu.Unlock()
		return fmt.Errorf("unknown Grok approval %q", approvalID)
	}
	result, err := grokApprovalResult(pending, resolution.Decision)
	if err != nil {
		g.mu.Unlock()
		return err // Invalid input must not consume a still-answerable request.
	}
	delete(g.approvals, approvalID)
	g.mu.Unlock()
	// Consume before writing: concurrent requests cannot answer the same native
	// RPC twice, and a transport failure is not safe for automatic replay.
	if err := g.sendRawResponse(pending.rawID, result, nil); err != nil {
		return err
	}
	g.setState(model.StateWorking, "")
	return nil
}

func grokApprovalResult(pending grokPendingApproval, decision string) (any, error) {
	if pending.kind == "plan" {
		switch decision {
		case "accept":
			return map[string]any{"outcome": "approved"}, nil
		case "decline", "cancel":
			return map[string]any{"outcome": "cancelled"}, nil
		default:
			return nil, fmt.Errorf("unsupported Grok plan decision %q", decision)
		}
	}
	cancelled := map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
	if decision == "cancel" {
		return cancelled, nil
	}
	optionID := selectGrokPermissionOption(pending.options, decision)
	if optionID == "" {
		if decision == "decline" {
			return cancelled, nil // Never turn one rejection into reject_always.
		}
		return nil, fmt.Errorf("Grok permission decision %q has no unambiguous native option", decision)
	}
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}}, nil
}

func (g *GrokAdapter) cancelPendingInteractions() error {
	g.mu.Lock()
	pending := make([]grokPendingApproval, 0, len(g.approvals))
	for id, approval := range g.approvals {
		pending = append(pending, approval)
		delete(g.approvals, id)
	}
	g.mu.Unlock()
	var result error
	for _, approval := range pending {
		response := any(map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
		if approval.kind == "plan" {
			response = map[string]any{"outcome": "cancelled"}
		}
		if err := g.sendRawResponse(approval.rawID, response, nil); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func selectGrokPermissionOption(options []grokPermissionOption, decision string) string {
	id, explicit := strings.CutPrefix(decision, "option:")
	kind := map[string]string{"accept": "allow_once", "acceptForSession": "allow_always", "decline": "reject_once"}[decision]
	if (!explicit && kind == "") || (explicit && id == "") {
		return ""
	}
	selected := ""
	for _, option := range options {
		if option.ID == "" || (explicit && option.ID != id) || (!explicit && option.Kind != kind) {
			continue
		}
		if selected != "" {
			return "" // Do not guess between semantically different native choices.
		}
		selected = option.ID
	}
	if selected != "" {
		matches := 0
		for _, option := range options {
			if option.ID == selected {
				matches++
			}
		}
		if matches != 1 {
			return ""
		}
	}
	return selected
}

func (g *GrokAdapter) SetNativeAccess(ctx context.Context, access model.NativeAccess) error {
	if !access.Valid() {
		return fmt.Errorf("invalid Grok native access %q", access)
	}
	g.mu.Lock()
	if g.turn != nil {
		g.mu.Unlock()
		return errors.New("interrupt or stop Grok before changing its native access")
	}
	oldAccess := g.access
	g.access = access
	opened := g.sessionOpened
	sessionID := g.sessionID
	g.mu.Unlock()
	if opened {
		if err := g.applyAccessMode(ctx, sessionID, access); err != nil {
			g.mu.Lock()
			if g.access == access {
				g.access = oldAccess
			}
			g.mu.Unlock()
			return err
		}
	}
	return nil
}

func (g *GrokAdapter) handleServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "session/request_permission":
		g.handlePermissionRequest(id, params)
	case "x.ai/exit_plan_mode", "_x.ai/exit_plan_mode":
		g.handlePlanExitRequest(id, params)
	case "x.ai/ask_user_question", "_x.ai/ask_user_question":
		e := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		e.Name = "server_request.unsupported"
		e.Text = method
		e.Data = g.redactRaw(unwrapGrokExtParams(params))
		g.attachCurrentTurn(&e)
		g.sink(e)
		_ = g.sendRawResponse(id, nil, &grokRPCError{Code: -32000, Message: "question surfaced in PairRoom"})
	default:
		e := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		e.Name = "server_request.unsupported"
		e.Text = method
		e.Data = g.redactRaw(params)
		g.attachCurrentTurn(&e)
		g.sink(e)
		_ = g.sendRawResponse(id, nil, &grokRPCError{Code: -32601, Message: "method not supported by PairRoom"})
	}
}

func (g *GrokAdapter) handlePermissionRequest(id json.RawMessage, raw json.RawMessage) {
	var params struct {
		Options  []grokPermissionOption `json:"options"`
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		_ = g.sendRawResponse(id, nil, &grokRPCError{Code: -32602, Message: "invalid permission request"})
		return
	}
	detail := map[string]any{}
	_ = json.Unmarshal(raw, &detail)
	detailRaw, _ := json.Marshal(detail)
	detailRaw = g.redactRaw(detailRaw)
	approval := model.Approval{
		ID: model.NewID("approval"), Agent: g.cfg.Actor, Kind: "grok.permission",
		Title:  firstNonEmpty(params.ToolCall.Title, params.ToolCall.Kind, configuredParticipantName(g.cfg)+" permission request"),
		Detail: detailRaw, Status: "pending", RequestedAt: time.Now().UTC(),
	}
	pending := grokPendingApproval{rawID: append(json.RawMessage(nil), id...), options: params.Options, approval: approval}
	g.mu.Lock()
	g.approvals[approval.ID] = pending
	g.mu.Unlock()
	e := runtimeEvent(g.cfg.Actor, model.RuntimeApprovalRequested)
	e.Approval = &approval
	g.attachCurrentTurn(&e)
	g.sink(e)
	g.setState(model.StateWaiting, "")
}

func (g *GrokAdapter) handlePlanExitRequest(id json.RawMessage, raw json.RawMessage) {
	detail := g.redactRaw(unwrapGrokExtParams(raw))
	approval := model.Approval{
		ID: model.NewID("approval"), Agent: g.cfg.Actor, Kind: "grok.planExit",
		Title: configuredParticipantName(g.cfg) + " requests permission to leave plan mode", Detail: detail,
		Status: "pending", RequestedAt: time.Now().UTC(),
	}
	g.mu.Lock()
	g.approvals[approval.ID] = grokPendingApproval{rawID: append(json.RawMessage(nil), id...), kind: "plan", approval: approval}
	g.mu.Unlock()
	e := runtimeEvent(g.cfg.Actor, model.RuntimeApprovalRequested)
	e.Approval = &approval
	g.attachCurrentTurn(&e)
	g.sink(e)
	g.setState(model.StateWaiting, "")
}
