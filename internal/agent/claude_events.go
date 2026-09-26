package agent

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func (c *ClaudeAdapter) readStdout(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), claudeMaxStdoutLine)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		c.handleLine(line)
	}
	if err := scanner.Err(); err != nil {
		// Skipping the record could drop the result that settles the Turn or a
		// control response a caller is waiting for.
		c.failStream(streamFailureReason("Claude Code", claudeMaxStdoutLine, err))
	}
}

func (c *ClaudeAdapter) readStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16*1024), 1024*1024)
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		e.Name = "stderr"
		e.Text = text
		c.sink(e)
	}
}

func (c *ClaudeAdapter) handleLine(line []byte) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		e.Name = "stdout"
		e.Text = string(line)
		c.sink(e)
		return
	}
	var typ string
	_ = json.Unmarshal(raw["type"], &typ)
	pending, hasPending := c.currentPending()

	switch typ {
	case "control_request":
		c.handleControlRequest(line, pending, hasPending)
	case "control_response":
		c.handleControlResponse(line)

	case "system":
		var subtype, sessionID string
		_ = json.Unmarshal(raw["subtype"], &subtype)
		_ = json.Unmarshal(raw["session_id"], &sessionID)
		if subtype == "init" && sessionID != "" {
			c.mu.Lock()
			c.sessionID = sessionID
			c.mu.Unlock()
			e := runtimeEvent(c.cfg.Actor, model.RuntimeSession)
			e.SessionID = sessionID
			e.Data = append(json.RawMessage(nil), line...)
			c.sink(e)
			c.updateRuntimeInfoFromInit(line)
			return
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		e.Name = "system." + subtype
		e.Data = append(json.RawMessage(nil), line...)
		c.sink(e)

	case "stream_event":
		var envelope struct {
			Event struct {
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			} `json:"event"`
		}
		if err := json.Unmarshal(line, &envelope); err == nil && envelope.Event.Delta.Type == "text_delta" && envelope.Event.Delta.Text != "" {
			c.mu.Lock()
			c.output.WriteString(envelope.Event.Delta.Text)
			c.mu.Unlock()
			e := runtimeEvent(c.cfg.Actor, model.RuntimeTextDelta)
			if hasPending {
				e.TurnID = pending.turnID
				e.CorrelationID = pending.input.MessageID
			}
			e.Text = envelope.Event.Delta.Text
			c.sink(e)
		}

	case "assistant":
		c.emitClaudeAssistantItems(line, pending, hasPending)

	case "user":
		c.emitClaudeToolResults(line, pending, hasPending)

	case "result":
		var result struct {
			Subtype   string          `json:"subtype"`
			Result    string          `json:"result"`
			SessionID string          `json:"session_id"`
			CostUSD   float64         `json:"total_cost_usd"`
			Duration  int64           `json:"duration_ms"`
			Error     string          `json:"error"`
			IsError   bool            `json:"is_error"`
			Usage     json.RawMessage `json:"usage"`
		}
		_ = json.Unmarshal(line, &result)
		item, ok, next := c.popPending()
		c.mu.Lock()
		streamedText := c.output.String()
		fallback := c.fallback
		c.output.Reset()
		c.fallback = ""
		c.mu.Unlock()
		if !ok {
			// A resumed Claude process may emit a late result for native work that
			// PairRoom did not submit. It is diagnostic-only: publishing a terminal
			// event without a correlated pending input could release the Room owner
			// or relay stale transcript text.
			e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
			e.Name = "result.unmatched"
			e.Data = append(json.RawMessage(nil), line...)
			c.sink(e)
			return
		}
		if strings.TrimSpace(result.Result) == "" {
			result.Result = streamedText
		}
		if strings.TrimSpace(result.Result) == "" {
			result.Result = fallback
		}
		if result.SessionID != "" {
			c.mu.Lock()
			c.sessionID = result.SessionID
			c.mu.Unlock()
		}
		success := result.Subtype == "success" || (result.Subtype == "" && !result.IsError && result.Error == "")
		if ok && success && strings.TrimSpace(result.Result) != "" {
			e := runtimeEvent(c.cfg.Actor, model.RuntimeFinal)
			e.TurnID = item.turnID
			e.CorrelationID = item.input.MessageID
			e.Text = result.Result
			e.Data = append(json.RawMessage(nil), line...)
			c.sink(e)
		}
		if ok {
			kind, state := claudeResultState(result.Subtype, success)
			detail := result.Error
			if detail == "" && !success {
				detail = result.Subtype
			}
			c.emitInputState(item, kind, state, detail)
		}
		completed := runtimeEvent(c.cfg.Actor, model.RuntimeTurnCompleted)
		if ok {
			completed.TurnID = item.turnID
			completed.CorrelationID = item.input.MessageID
		}
		completed.Name = result.Subtype
		completed.Data = append(json.RawMessage(nil), line...)
		c.sink(completed)
		if result.CostUSD != 0 || result.Duration != 0 || len(result.Usage) > 0 {
			usage := runtimeEvent(c.cfg.Actor, model.RuntimeUsageUpdated)
			if ok {
				usage.TurnID = item.turnID
				usage.CorrelationID = item.input.MessageID
			}
			usage.Data = append(json.RawMessage(nil), line...)
			c.sink(usage)
		}
		if !success {
			e := runtimeEvent(c.cfg.Actor, model.RuntimeError)
			if ok {
				e.TurnID = item.turnID
				e.CorrelationID = item.input.MessageID
			}
			e.Text = result.Error
			if e.Text == "" {
				e.Text = "Claude turn ended with " + result.Subtype
			}
			c.sink(e)
		}
		if next != nil {
			c.emitTurnStarted(*next)
			c.emitInputState(*next, model.RuntimeInputProcessing, model.ProcessingWorking, "started after queue wait")
			c.setState(model.StateWorking, "")
		} else if success {
			c.setState(model.StateIdle, "")
		} else {
			c.setState(model.StateError, result.Error)
		}

	case "tool_progress", "hook_started", "hook_progress", "hook_response", "status", "rate_limit_event":
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		e.Name = typ
		if hasPending {
			e.TurnID = pending.turnID
			e.CorrelationID = pending.input.MessageID
		}
		e.Data = append(json.RawMessage(nil), line...)
		c.sink(e)
	}
}

func (c *ClaudeAdapter) updateRuntimeInfoFromInit(line []byte) {
	var init struct {
		Model          string          `json:"model"`
		Version        string          `json:"version"`
		PermissionMode string          `json:"permissionMode"`
		Capabilities   json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(line, &init); err != nil {
		return
	}
	c.mu.Lock()
	info := c.runtimeInfo
	if init.Model != "" {
		info.Model = init.Model
	}
	if init.Version != "" {
		info.Version = extractSemanticVersion(init.Version)
		if info.Version == "" {
			info.Version = init.Version
		}
	}
	if init.PermissionMode != "" {
		info.PermissionMode = init.PermissionMode
	}
	info.Capabilities = mergeUniqueStrings(info.Capabilities, capabilityNames(init.Capabilities))
	info.Warnings = mergeUniqueStrings(info.Warnings, diagnosticStrings(line,
		"plugin_errors", "pluginErrors", "mcp_server_errors", "mcpServerErrors"))
	info.Available = true
	info.Data, _ = json.Marshal(map[string]any{
		"version":         info.Version,
		"model":           info.Model,
		"permission_mode": info.PermissionMode,
		"capabilities":    info.Capabilities,
		"warnings":        info.Warnings,
	})
	info.ProbedAt = time.Now().UTC()
	c.runtimeInfo = info
	c.mu.Unlock()
	emitRuntimeInfo(c.sink, c.cfg.Actor, info)
}

func claudeResultState(subtype string, success bool) (string, model.ProcessingState) {
	if success {
		return model.RuntimeInputCompleted, model.ProcessingCompleted
	}
	lower := strings.ToLower(subtype)
	if strings.Contains(lower, "interrupt") || strings.Contains(lower, "cancel") || strings.Contains(lower, "abort") {
		return model.RuntimeInputCancelled, model.ProcessingCancelled
	}
	return model.RuntimeInputFailed, model.ProcessingFailed
}

func (c *ClaudeAdapter) emitClaudeAssistantItems(line []byte, pending claudePending, hasPending bool) {
	var message struct {
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
				Text  string          `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &message); err != nil {
		return
	}
	var text strings.Builder
	for _, block := range message.Message.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			e := runtimeEvent(c.cfg.Actor, model.RuntimeToolStarted)
			if hasPending {
				e.TurnID = pending.turnID
				e.CorrelationID = pending.input.MessageID
			}
			e.ItemID = block.ID
			e.Name = block.Name
			e.Data = append(json.RawMessage(nil), block.Input...)
			c.sink(e)
		}
	}
	if value := strings.TrimSpace(text.String()); value != "" {
		c.mu.Lock()
		c.fallback = value
		c.mu.Unlock()
	}
}

func (c *ClaudeAdapter) emitClaudeToolResults(line []byte, pending claudePending, hasPending bool) {
	var message struct {
		Message struct {
			Content []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
				IsError   bool            `json:"is_error"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &message); err != nil {
		return
	}
	for _, block := range message.Message.Content {
		if block.Type != "tool_result" {
			continue
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimeToolCompleted)
		if hasPending {
			e.TurnID = pending.turnID
			e.CorrelationID = pending.input.MessageID
		}
		e.ItemID = block.ToolUseID
		if block.IsError {
			e.Name = "error"
		}
		e.Data = append(json.RawMessage(nil), block.Content...)
		c.sink(e)
	}
}
