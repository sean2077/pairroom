package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

type claudeControlResult struct {
	response json.RawMessage
	err      error
}

// initializeControl completes the same native control handshake used by the
// official Agent SDK before PairRoom sends the first user message. The
// handshake is deliberately awaited: it activates the bidirectional control
// channel that carries permission requests and interactive questions.
func (c *ClaudeAdapter) initializeControl(ctx context.Context) error {
	response, err := c.sendControlRequest(ctx, map[string]any{
		"subtype": "initialize",
		"hooks":   nil,
	})
	if err != nil {
		return err
	}
	var info struct {
		Commands []json.RawMessage `json:"commands"`
		Models   []json.RawMessage `json:"models"`
		Agents   []json.RawMessage `json:"agents"`
		PID      int               `json:"pid"`
	}
	if len(response) > 0 && string(response) != "null" {
		if err := json.Unmarshal(response, &info); err != nil {
			return fmt.Errorf("decode initialize response: %w", err)
		}
	}

	c.controlMu.Lock()
	c.controlReady = true
	c.controlMu.Unlock()
	c.mu.Lock()
	runtimeInfo := c.runtimeInfo
	runtimeInfo.Capabilities = mergeUniqueStrings(append(runtimeInfo.Capabilities,
		"control-protocol", "interactive-approvals", "user-questions"))
	c.runtimeInfo = runtimeInfo
	c.mu.Unlock()
	emitRuntimeInfo(c.sink, c.cfg.Actor, runtimeInfo)

	e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
	e.Name = "control.initialized"
	e.Text = fmt.Sprintf("commands=%d models=%d agents=%d", len(info.Commands), len(info.Models), len(info.Agents))
	e.Data = append(json.RawMessage(nil), response...)
	c.sink(e)
	return nil
}

func (c *ClaudeAdapter) sendControlRequest(ctx context.Context, request map[string]any) (json.RawMessage, error) {
	requestID := model.NewID("claude-control")
	waiter := make(chan claudeControlResult, 1)
	c.controlMu.Lock()
	if c.control == nil {
		c.control = make(map[string]chan claudeControlResult)
	}
	c.control[requestID] = waiter
	c.controlMu.Unlock()

	payload := map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request":    request,
	}
	if err := c.writePayload(payload, "Claude control request"); err != nil {
		c.controlMu.Lock()
		delete(c.control, requestID)
		c.controlMu.Unlock()
		return nil, err
	}

	select {
	case result := <-waiter:
		return result.response, result.err
	case <-ctx.Done():
		c.controlMu.Lock()
		delete(c.control, requestID)
		c.controlMu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *ClaudeAdapter) handleControlResponse(line []byte) {
	var envelope struct {
		Response struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Response  json.RawMessage `json:"response"`
			Error     string          `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil || envelope.Response.RequestID == "" {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		e.Name = "control_response.invalid"
		e.Data = append(json.RawMessage(nil), line...)
		c.sink(e)
		return
	}
	c.controlMu.Lock()
	waiter, ok := c.control[envelope.Response.RequestID]
	if ok {
		delete(c.control, envelope.Response.RequestID)
	}
	c.controlMu.Unlock()
	if !ok {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		e.Name = "control_response.unmatched"
		e.Text = envelope.Response.RequestID
		e.Data = append(json.RawMessage(nil), line...)
		c.sink(e)
		return
	}
	if envelope.Response.Subtype == "success" {
		waiter <- claudeControlResult{response: append(json.RawMessage(nil), envelope.Response.Response...)}
		return
	}
	detail := strings.TrimSpace(envelope.Response.Error)
	if detail == "" {
		detail = "Claude control request failed with subtype " + envelope.Response.Subtype
	}
	waiter <- claudeControlResult{err: errors.New(detail)}
}

func (c *ClaudeAdapter) failControlWaiters(err error) {
	c.controlMu.Lock()
	pending := c.control
	c.control = make(map[string]chan claudeControlResult)
	c.controlReady = false
	c.controlMu.Unlock()
	for _, waiter := range pending {
		select {
		case waiter <- claudeControlResult{err: err}:
		default:
		}
	}
}

func (c *ClaudeAdapter) writeControlResponse(requestID string, result map[string]any) error {
	return c.writePayload(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   result,
		},
	}, "Claude control response")
}

func (c *ClaudeAdapter) writeControlError(requestID, detail string) error {
	return c.writePayload(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "error",
			"request_id": requestID,
			"error":      detail,
		},
	}, "Claude control error")
}

func (c *ClaudeAdapter) writePayload(payload any, label string) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s: %w", label, err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("Claude stdin is not available")
	}
	if _, err := stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("send %s: %w", label, err)
	}
	return nil
}
