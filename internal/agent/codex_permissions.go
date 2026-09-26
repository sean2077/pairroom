package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/version"
)

type pendingApproval struct {
	rawID         json.RawMessage
	method        string
	params        json.RawMessage
	approval      model.Approval
	turnID        string
	correlationID string
}

func normalizeCodexApprovalPolicy(value string) string {
	switch value {
	case "unlessTrusted":
		return "untrusted"
	case "yolo":
		return "never"
	default:
		return value
	}
}

func (c *CodexAdapter) threadSandbox() string {
	// The thread/start sandbox enum uses CLI-style kebab-case, unlike
	// the camelCase tagged variants in turn/start sandboxPolicy objects.
	switch strings.ToLower(c.cfg.Sandbox) {
	case "readonly", "read_only", "read-only":
		return "read-only"
	case "dangerfullaccess", "danger_full_access", "danger-full-access", "full", "fullaccess", "full_access", "full-access":
		return "danger-full-access"
	default:
		return "workspace-write"
	}
}

func (c *CodexAdapter) sandboxPolicy(access model.NativeAccess) map[string]any {
	if access == model.NativeAccessReadOnly {
		return map[string]any{"type": "readOnly"}
	}
	switch strings.ToLower(c.cfg.Sandbox) {
	case "readonly", "read_only", "read-only":
		return map[string]any{"type": "readOnly"}
	case "dangerfullaccess", "danger_full_access", "danger-full-access", "full", "fullaccess", "full_access", "full-access":
		return map[string]any{"type": "dangerFullAccess"}
	default:
		return map[string]any{
			"type":          "workspaceWrite",
			"writableRoots": []string{c.cfg.Repo},
			"networkAccess": false,
		}
	}
}

func (c *CodexAdapter) handleServerRequest(rawID json.RawMessage, method string, params json.RawMessage) {
	commandApproval := strings.HasSuffix(method, "commandExecution/requestApproval")
	fileApproval := strings.HasSuffix(method, "fileChange/requestApproval")
	permissionApproval := strings.HasSuffix(method, "permissions/requestApproval")
	if !commandApproval && !fileApproval && !permissionApproval {
		// Structured user-input, MCP elicitation, and dynamic tool requests have
		// distinct response schemas. The adapter fails those
		// closed instead of accidentally granting capability with a generic yes.
		_ = c.sendRawResponse(rawID, nil, &codexRPCError{Code: -32601, Message: "PairRoom " + version.Current + " does not implement " + method})
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		e.Name = "server_request.unsupported"
		e.Text = method
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)
		return
	}
	displayID := model.NewID("approval")
	name := configuredParticipantName(c.cfg)
	title := "Approve " + name + " command"
	if fileApproval {
		title = "Approve " + name + " file change"
	} else if permissionApproval {
		title = "Grant " + name + " additional permissions"
	}
	approval := model.Approval{
		ID: displayID, Agent: c.cfg.Actor, Kind: method, Title: title,
		Detail: append(json.RawMessage(nil), params...), Status: "pending", RequestedAt: time.Now().UTC(),
	}
	var requestContext struct {
		TurnID string `json:"turnId"`
	}
	_ = json.Unmarshal(params, &requestContext)
	c.mu.Lock()
	turnID := strings.TrimSpace(requestContext.TurnID)
	if turnID == "" {
		turnID = c.currentTurn
	}
	input := c.latestTurnInputLocked(turnID)
	if input.MessageID == "" && c.startingInput != nil {
		input = *c.startingInput
	}
	correlationID := input.MessageID
	if c.cfg.RequireExactSession && correlationID == "" {
		c.mu.Unlock()
		// A resumed vendor thread may surface a request from work that predates
		// the Room binding. Fail it closed instead of presenting historical
		// transcript state as a new PairRoom approval or leaving the Runtime stuck.
		declined, resultErr := codexApprovalResult(pendingApproval{method: method, params: params}, "decline")
		if resultErr != nil {
			_ = c.sendRawResponse(rawID, nil, &codexRPCError{Code: -32602, Message: "PairRoom rejected an unbound approval request"})
		} else {
			_ = c.sendRawResponse(rawID, declined, nil)
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimeError)
		e.Name = "adapter.boundary_rejected"
		e.Text = "Codex emitted an approval request outside a PairRoom-authored turn"
		c.sink(e)
		return
	}
	c.approvals[displayID] = pendingApproval{
		rawID: append(json.RawMessage(nil), rawID...), method: method,
		params: append(json.RawMessage(nil), params...), approval: approval,
		turnID: turnID, correlationID: correlationID,
	}
	c.mu.Unlock()
	e := runtimeEvent(c.cfg.Actor, model.RuntimeApprovalRequested)
	e.TurnID = turnID
	e.CorrelationID = correlationID
	e.Approval = &approval
	e.Data = append(json.RawMessage(nil), params...)
	c.sink(e)
	c.setState(model.StateWaiting, "waiting for approval")
}

func (c *CodexAdapter) handleServerRequestResolved(params json.RawMessage) {
	var payload struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(params, &payload); err != nil || len(payload.RequestID) == 0 {
		return
	}
	canonical := strings.TrimSpace(string(payload.RequestID))
	var cleared *model.Approval
	var turnID, correlationID string
	c.mu.Lock()
	for id, pending := range c.approvals {
		if strings.TrimSpace(string(pending.rawID)) != canonical {
			continue
		}
		approval := pending.approval
		now := time.Now().UTC()
		approval.Status = "cleared"
		approval.Decision = "cleared"
		approval.ResolvedAt = &now
		cleared = &approval
		turnID = pending.turnID
		correlationID = pending.correlationID
		delete(c.approvals, id)
		break
	}
	c.mu.Unlock()
	if cleared != nil {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeApprovalResolved)
		e.TurnID = turnID
		e.CorrelationID = correlationID
		e.Approval = cleared
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)
		c.mu.Lock()
		active := c.currentTurn != ""
		c.mu.Unlock()
		if active {
			c.setState(model.StateWorking, "")
		} else {
			c.setState(model.StateIdle, "")
		}
	}
}

// clearStaleApprovals answers approval requests that a terminal turn left
// unresolved so the vendor is not left waiting on a dead request and the local
// record can never wedge the SetNativeAccess boundary gate. It mirrors Grok's
// cancelPendingInteractions; the Room-side projection expires through the
// ordinary turn-boundary handling.
func (c *CodexAdapter) clearStaleApprovals(stale []pendingApproval) {
	for _, pending := range stale {
		result, err := codexApprovalResult(pending, "decline")
		if err != nil {
			continue
		}
		_ = c.sendRawResponse(pending.rawID, result, nil)
	}
}

func (c *CodexAdapter) ResolveApproval(ctx context.Context, approvalID string, resolution model.ApprovalResolution) error {
	_ = ctx
	decision := resolution.Decision
	allowed := map[string]bool{
		"accept": true, "acceptForSession": true, "decline": true, "cancel": true,
	}
	if !allowed[decision] {
		return fmt.Errorf("unsupported approval decision %q", decision)
	}
	c.mu.Lock()
	pending, ok := c.approvals[approvalID]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown approval %q", approvalID)
	}
	result, err := codexApprovalResult(pending, decision)
	if err != nil {
		return err
	}
	if err := c.sendRawResponse(pending.rawID, result, nil); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.approvals, approvalID)
	active := c.currentTurn != ""
	c.mu.Unlock()
	// The room engine owns the user-facing approval projection after this call
	// succeeds. serverRequest/resolved remains available for server-side clears.
	// Only a still-active turn returns to Working; the turn may have completed
	// while the decision was in flight, and resurrecting Working would project
	// a state the vendor no longer has.
	if active {
		c.setState(model.StateWorking, "")
	}
	return nil
}

// Codex receives sandbox policy per turn, so an access change does not require
// an app-server restart. It must still happen at a safe turn boundary: already
// queued or in-flight inputs retain the access/policy captured when they were
// created and must not be relabelled midway through execution.
func (c *CodexAdapter) SetNativeAccess(_ context.Context, access model.NativeAccess) error {
	if !access.Valid() {
		return fmt.Errorf("invalid Codex native access %q", access)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.access == access {
		// The submission path re-asserts the current access before every turn.
		// A same-access assertion changes nothing and must not fail on turn
		// state or a stale approval record; only a real transition requires
		// the safe-boundary gate below.
		return nil
	}
	if c.state == model.StateStarting || c.state == model.StateWorking || c.state == model.StateWaiting ||
		c.currentTurn != "" || c.startingInput != nil || len(c.wireInputs) > 0 || len(c.approvals) > 0 {
		return errors.New("interrupt or stop Codex before changing its native access")
	}
	c.access = access
	return nil
}

func codexApprovalResult(pending pendingApproval, decision string) (map[string]any, error) {
	if !strings.HasSuffix(pending.method, "permissions/requestApproval") {
		return map[string]any{"decision": decision}, nil
	}

	// Permission requests use a different response schema than command/file
	// approvals. Grant only the exact profile requested by app-server; an empty
	// object means every requested permission is denied.
	var request struct {
		Permissions json.RawMessage `json:"permissions"`
	}
	if err := json.Unmarshal(pending.params, &request); err != nil {
		return nil, fmt.Errorf("decode Codex permission request: %w", err)
	}
	permissions := any(map[string]any{})
	if decision == "accept" || decision == "acceptForSession" {
		if len(request.Permissions) == 0 || string(request.Permissions) == "null" {
			return nil, errors.New("Codex permission request omitted permissions")
		}
		if err := json.Unmarshal(request.Permissions, &permissions); err != nil {
			return nil, fmt.Errorf("decode requested Codex permissions: %w", err)
		}
	}
	scope := "turn"
	if decision == "acceptForSession" {
		scope = "session"
	}
	return map[string]any{"scope": scope, "permissions": permissions}, nil
}
