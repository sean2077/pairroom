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

type claudeApprovalRequest struct {
	requestID   string
	toolName    string
	input       json.RawMessage
	suggestions json.RawMessage
	approval    model.Approval
}

func claudePermissionBypass(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "bypasspermissions", "bypass", "yolo", "always-approve", "always_approve":
		return true
	default:
		return false
	}
}

func appendClaudePermissionArgs(args []string, flags map[string]bool, mode string) []string {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return args
	}
	native := mode
	if claudePermissionBypass(mode) {
		native = "bypassPermissions"
		if flags["--allow-dangerously-skip-permissions"] {
			args = append(args, "--allow-dangerously-skip-permissions")
		}
		if flags["--dangerously-skip-permissions"] {
			args = append(args, "--dangerously-skip-permissions")
		}
	}
	if flags["--permission-mode"] {
		args = append(args, "--permission-mode", native)
	}
	return args
}

func (c *ClaudeAdapter) clearApprovals() {
	c.mu.Lock()
	c.approvals = make(map[string]claudeApprovalRequest)
	c.mu.Unlock()
}

func (c *ClaudeAdapter) handleControlRequest(line []byte, pending claudePending, hasPending bool) {
	var envelope struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype               string          `json:"subtype"`
			ToolName              string          `json:"tool_name"`
			Input                 json.RawMessage `json:"input"`
			PermissionSuggestions json.RawMessage `json:"permission_suggestions"`
			ToolUseID             string          `json:"tool_use_id"`
			AgentID               string          `json:"agent_id"`
			Title                 string          `json:"title"`
			DisplayName           string          `json:"display_name"`
			Description           string          `json:"description"`
		} `json:"request"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil || envelope.RequestID == "" {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		e.Name = "control_request.invalid"
		e.Data = append(json.RawMessage(nil), line...)
		c.sink(e)
		return
	}
	if envelope.Request.Subtype != "can_use_tool" {
		_ = c.writeControlError(envelope.RequestID, "PairRoom does not implement Claude control request subtype "+envelope.Request.Subtype)
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		e.Name = "control_request.unsupported"
		e.Text = envelope.Request.Subtype
		e.Data = append(json.RawMessage(nil), line...)
		c.sink(e)
		return
	}
	if c.cfg.RequireExactSession && !hasPending {
		// Resuming a vendor session may surface a control request created before
		// the PairRoom binding. It is outside the Room transcript boundary, so it
		// must neither become a visible approval nor leave this Runtime waiting.
		_ = c.writeControlError(envelope.RequestID, "PairRoom rejected a control request outside a Room-authored turn")
		e := runtimeEvent(c.cfg.Actor, model.RuntimeError)
		e.Name = "adapter.boundary_rejected"
		e.Text = "Claude emitted a control request outside a PairRoom-authored turn"
		c.sink(e)
		return
	}

	c.mu.Lock()
	access := c.access
	c.mu.Unlock()
	if access == model.NativeAccessReadOnly {
		switch envelope.Request.ToolName {
		case "Edit", "Write", "NotebookEdit", "ExitPlanMode":
			_ = c.writeControlResponse(envelope.RequestID, map[string]any{
				"behavior": "deny",
				"message":  "PairRoom read-only access cannot use " + envelope.Request.ToolName + "; change the participant permission profile first",
			})
			e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
			e.Name = "reviewer.tool.denied"
			e.Text = envelope.Request.ToolName
			e.Data = append(json.RawMessage(nil), line...)
			c.sink(e)
			return
		}
	}

	detail := map[string]any{
		"tool_name": envelope.Request.ToolName,
		"input":     json.RawMessage(envelope.Request.Input),
	}
	if len(envelope.Request.PermissionSuggestions) > 0 && string(envelope.Request.PermissionSuggestions) != "null" {
		detail["permission_suggestions"] = json.RawMessage(envelope.Request.PermissionSuggestions)
	}
	if envelope.Request.ToolUseID != "" {
		detail["tool_use_id"] = envelope.Request.ToolUseID
	}
	if envelope.Request.AgentID != "" {
		detail["agent_id"] = envelope.Request.AgentID
	}
	if envelope.Request.Description != "" {
		detail["description"] = envelope.Request.Description
	}
	detailJSON, _ := json.Marshal(detail)
	title := strings.TrimSpace(envelope.Request.Title)
	if title == "" {
		title = strings.TrimSpace(envelope.Request.DisplayName)
	}
	if title == "" {
		title = "Approve " + configuredParticipantName(c.cfg) + " " + envelope.Request.ToolName
	}
	kind := "claude.toolApproval"
	if envelope.Request.ToolName == "AskUserQuestion" {
		kind = "claude.userQuestion"
		title = configuredParticipantName(c.cfg) + " asks for input"
	}
	approval := model.Approval{
		ID: model.NewID("approval"), Agent: c.cfg.Actor, Kind: kind,
		Title: title, Detail: detailJSON, Status: "pending", RequestedAt: time.Now().UTC(),
	}
	c.mu.Lock()
	c.approvals[approval.ID] = claudeApprovalRequest{
		requestID: envelope.RequestID, toolName: envelope.Request.ToolName,
		input:       append(json.RawMessage(nil), envelope.Request.Input...),
		suggestions: append(json.RawMessage(nil), envelope.Request.PermissionSuggestions...),
		approval:    approval,
	}
	c.mu.Unlock()
	e := runtimeEvent(c.cfg.Actor, model.RuntimeApprovalRequested)
	if hasPending {
		e.TurnID = pending.turnID
		e.CorrelationID = pending.input.MessageID
	}
	e.Approval = &approval
	e.Data = append(json.RawMessage(nil), line...)
	c.sink(e)
	c.setState(model.StateWaiting, "waiting for approval")
}

func (c *ClaudeAdapter) ResolveApproval(ctx context.Context, approvalID string, resolution model.ApprovalResolution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	decision := strings.TrimSpace(resolution.Decision)
	c.mu.Lock()
	pending, ok := c.approvals[approvalID]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown approval %q", approvalID)
	}

	result := map[string]any{}
	switch decision {
	case "accept", "acceptForSession":
		result["behavior"] = "allow"
		var input map[string]any
		if len(pending.input) > 0 && string(pending.input) != "null" {
			if err := json.Unmarshal(pending.input, &input); err != nil {
				return fmt.Errorf("decode Claude tool input: %w", err)
			}
		}
		if input == nil {
			input = map[string]any{}
		}
		// PermissionResultAllow requires the complete tool input even when the
		// user approves it unchanged. AskUserQuestion adds the collected answers
		// while preserving the exact questions emitted by Claude Code.
		updatedInput := make(map[string]any, len(input)+1)
		for key, value := range input {
			updatedInput[key] = value
		}
		if pending.toolName == "AskUserQuestion" {
			if err := validateClaudeQuestionAnswers(updatedInput["questions"], resolution.Answers); err != nil {
				return err
			}
			updatedInput["answers"] = resolution.Answers
		}
		result["updatedInput"] = updatedInput
		if decision == "acceptForSession" && len(pending.suggestions) > 0 && string(pending.suggestions) != "null" {
			var suggestions any
			if err := json.Unmarshal(pending.suggestions, &suggestions); err == nil {
				result["updatedPermissions"] = suggestions
			}
		}
	case "decline", "cancel":
		result["behavior"] = "deny"
		message := strings.TrimSpace(resolution.Message)
		if message == "" {
			if decision == "cancel" {
				message = "Cancelled by the PairRoom user"
			} else {
				message = "Denied by the PairRoom user"
			}
		}
		result["message"] = message
	default:
		return fmt.Errorf("unsupported approval decision %q", decision)
	}

	if err := c.writeControlResponse(pending.requestID, result); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.approvals, approvalID)
	active := len(c.pending) > 0
	c.mu.Unlock()
	if active {
		c.setState(model.StateWorking, "")
	} else {
		c.setState(model.StateIdle, "")
	}
	return nil
}

// Native question text is the answer-map identity. Validate completeness before
// sending the control response, without rewriting questions or free-form answers.
func validateClaudeQuestionAnswers(raw any, answers map[string]string) error {
	questions, ok := raw.([]any)
	if !ok || len(questions) == 0 {
		return errors.New("Claude question request has no parseable questions")
	}
	expected := make(map[string]bool, len(questions))
	for _, rawQuestion := range questions {
		question, ok := rawQuestion.(map[string]any)
		if !ok {
			return errors.New("Claude question request contains a malformed question")
		}
		text, ok := question["question"].(string)
		if !ok || strings.TrimSpace(text) == "" || expected[text] {
			return errors.New("Claude question request has missing or ambiguous question text")
		}
		expected[text] = true
		if strings.TrimSpace(answers[text]) == "" {
			return fmt.Errorf("Claude question approval requires an answer to %q", text)
		}
	}
	for text := range answers {
		if !expected[text] {
			return fmt.Errorf("Claude question approval contains an unknown question %q", text)
		}
	}
	return nil
}

// SetNativeAccess maps PairRoom read-only access to Claude Code's native plan
// mode. Changes are rejected while a turn or permission prompt is active; this
// avoids silently changing a harness policy midway through execution.
func (c *ClaudeAdapter) SetNativeAccess(ctx context.Context, access model.NativeAccess) error {
	if !access.Valid() {
		return fmt.Errorf("invalid Claude native access %q", access)
	}
	desiredMode := c.baseMode
	if access == model.NativeAccessReadOnly {
		desiredMode = "plan"
	}

	c.mu.Lock()
	if c.access == access && c.cfg.PermissionMode == desiredMode {
		c.mu.Unlock()
		return nil
	}
	state := c.state
	if state == model.StateWorking || state == model.StateWaiting || state == model.StateStarting || len(c.pending) > 0 || len(c.approvals) > 0 {
		c.mu.Unlock()
		return errors.New("interrupt or stop Claude before changing its native access")
	}
	wasRunning := c.cmd != nil && c.cmd.Process != nil
	oldMode, oldAccess := c.cfg.PermissionMode, c.access
	c.cfg.PermissionMode = desiredMode
	c.access = access
	c.mu.Unlock()

	if !wasRunning {
		return nil
	}
	if err := c.Stop(ctx); err != nil {
		c.mu.Lock()
		c.cfg.PermissionMode, c.access = oldMode, oldAccess
		c.mu.Unlock()
		return err
	}
	if err := c.Start(ctx); err != nil {
		c.mu.Lock()
		c.cfg.PermissionMode, c.access = oldMode, oldAccess
		c.mu.Unlock()
		return fmt.Errorf("restart Claude with native access %q: %w", access, err)
	}
	return nil
}
