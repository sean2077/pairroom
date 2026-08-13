package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

type claudePending struct {
	input  model.AgentInput
	turnID string
}

type ClaudeAdapter struct {
	cfg  Config
	sink EventSink

	startMu      sync.Mutex
	submitMu     sync.Mutex
	mu           sync.Mutex
	writeMu      sync.Mutex
	state        model.AgentState
	sessionID    string
	resume       bool
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	pending      []claudePending
	output       strings.Builder
	fallback     string
	flags        map[string]bool
	runtimeInfo  model.RuntimeInfo
	protocolSent bool
	intentional  bool
}

func NewClaude(cfg Config, sink EventSink) *ClaudeAdapter {
	if cfg.Command == "" {
		cfg.Command = "claude"
	}
	if cfg.PermissionMode == "" {
		cfg.PermissionMode = "auto"
	}
	resume := cfg.SessionID != ""
	sessionID := cfg.SessionID
	if sessionID == "" {
		sessionID = newUUID()
	}
	return &ClaudeAdapter{
		cfg: cfg, sink: sink, state: model.StateStopped,
		sessionID: sessionID, resume: resume,
	}
}

func (c *ClaudeAdapter) Actor() model.ActorID { return model.ActorClaude }

func (c *ClaudeAdapter) State() model.AgentState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *ClaudeAdapter) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *ClaudeAdapter) setState(state model.AgentState, detail string) {
	c.mu.Lock()
	changed := c.state != state
	c.state = state
	c.mu.Unlock()
	if !changed && detail == "" {
		return
	}
	e := runtimeEvent(model.ActorClaude, model.RuntimeState)
	e.State = state
	e.Text = detail
	c.sink(e)
}

func (c *ClaudeAdapter) Start(ctx context.Context) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()

	c.mu.Lock()
	if c.cmd != nil && c.cmd.Process != nil {
		c.mu.Unlock()
		return nil
	}
	c.state = model.StateStarting
	c.intentional = false
	c.protocolSent = false
	c.mu.Unlock()

	probe, probeErr := ProbeRuntime(ctx, Config{
		Actor: model.ActorClaude, Command: c.cfg.Command, Model: c.cfg.Model,
		PermissionMode: c.cfg.PermissionMode,
	})
	info := model.RuntimeInfo{
		Available: false, Command: c.cfg.Command, Protocol: "claude-stream-json",
		Model: c.cfg.Model, PermissionMode: c.cfg.PermissionMode, ProbedAt: time.Now().UTC(),
	}
	flags := map[string]bool{}
	if probeErr == nil {
		info = probe.RuntimeInfo(c.cfg)
		flags = probe.SupportedFlags
	} else {
		info.Warnings = []string{probeErr.Error()}
	}
	c.mu.Lock()
	c.flags = flags
	c.runtimeInfo = info
	c.mu.Unlock()
	emitRuntimeInfo(c.sink, model.ActorClaude, info)
	if probeErr != nil {
		c.setState(model.StateError, probeErr.Error())
		return probeErr
	}

	systemPrompt := c.cfg.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = prompt.SystemPrompt(model.ActorClaude, c.cfg.RoomName, c.cfg.Repo)
	}

	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json"}
	if flags["--verbose"] {
		args = append(args, "--verbose")
	}
	for _, optional := range []string{
		"--include-partial-messages",
		"--replay-user-messages",
		"--forward-subagent-text",
		"--include-hook-events",
	} {
		if flags[optional] {
			args = append(args, optional)
		}
	}
	if flags["--append-system-prompt-file"] {
		promptPath, err := c.ensurePromptFile(systemPrompt)
		if err != nil {
			c.setState(model.StateError, err.Error())
			return err
		}
		args = append(args, "--append-system-prompt-file", promptPath)
		c.mu.Lock()
		c.protocolSent = true
		c.mu.Unlock()
	} else if flags["--append-system-prompt"] {
		args = append(args, "--append-system-prompt", systemPrompt)
		c.mu.Lock()
		c.protocolSent = true
		c.mu.Unlock()
	}
	if flags["--permission-mode"] && c.cfg.PermissionMode != "" {
		args = append(args, "--permission-mode", c.cfg.PermissionMode)
	}
	if flags["--model"] && c.cfg.Model != "" {
		args = append(args, "--model", c.cfg.Model)
	}

	c.mu.Lock()
	if c.resume && flags["--resume"] {
		args = append(args, "--resume", c.sessionID)
	} else if !c.resume && flags["--session-id"] {
		args = append(args, "--session-id", c.sessionID)
	} else if c.resume && !flags["--resume"] {
		// A legacy CLI cannot reopen the previous native session. Start a fresh
		// session and replace the durable ID when the init event arrives.
		c.sessionID = newUUID()
		c.resume = false
	}
	c.mu.Unlock()

	cmd := exec.Command(c.cfg.Command, args...)
	cmd.Dir = c.cfg.Repo
	cmd.Env = envWithout("CLAUDECODE")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		c.setState(model.StateError, err.Error())
		return fmt.Errorf("claude stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		c.setState(model.StateError, err.Error())
		return fmt.Errorf("claude stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		c.setState(model.StateError, err.Error())
		return fmt.Errorf("claude stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		c.setState(model.StateError, err.Error())
		return fmt.Errorf("start claude: %w", err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.resume = true
	c.mu.Unlock()
	c.setState(model.StateIdle, "")

	session := runtimeEvent(model.ActorClaude, model.RuntimeSession)
	session.SessionID = c.SessionID()
	c.sink(session)

	go c.readStdout(stdout)
	go c.readStderr(stderr)
	go c.waitProcess(cmd)
	return nil
}

func (c *ClaudeAdapter) ensurePromptFile(content string) (string, error) {
	dir := filepath.Join(c.cfg.DataDir, "runtime")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create claude runtime directory: %w", err)
	}
	path := filepath.Join(dir, "claude-pairroom-prompt.md")
	if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write claude system prompt: %w", err)
	}
	return path, nil
}

func (c *ClaudeAdapter) Submit(ctx context.Context, input model.AgentInput) (model.DeliveryState, error) {
	// Claude processes streamed user messages in arrival order. Serialize queue
	// mutation with the stdin write so result events retain exact correlations.
	c.submitMu.Lock()
	defer c.submitMu.Unlock()

	if err := c.Start(ctx); err != nil {
		return model.DeliveryFailed, err
	}

	entry := claudePending{input: input, turnID: model.NewID("claude-turn")}
	c.mu.Lock()
	status := model.DeliveryStarted
	if c.state == model.StateWorking || len(c.pending) > 0 {
		status = model.DeliveryQueued
	}
	protocolSent := c.protocolSent
	c.pending = append(c.pending, entry)
	c.mu.Unlock()

	text := prompt.Envelope(input)
	if !protocolSent {
		systemPrompt := c.cfg.SystemPrompt
		if systemPrompt == "" {
			systemPrompt = prompt.SystemPrompt(model.ActorClaude, c.cfg.RoomName, c.cfg.Repo)
		}
		text = systemPrompt + "\n\n" + text
	}
	payload := map[string]any{
		"type": "user",
		// Claude's stream-json SDK input accepts an optional UUID. Use a real
		// RFC 4122 value rather than the room's human-readable message ID so the
		// native transcript remains valid across resume/replay operations.
		"uuid":       newUUID(),
		"session_id": c.SessionID(),
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
		"parent_tool_use_id": nil,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		c.removePending(input.MessageID)
		return model.DeliveryFailed, fmt.Errorf("encode claude input: %w", err)
	}

	c.writeMu.Lock()
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		c.writeMu.Unlock()
		c.removePending(input.MessageID)
		return model.DeliveryFailed, errors.New("claude stdin is not available")
	}
	_, err = stdin.Write(append(data, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		c.removePending(input.MessageID)
		c.setState(model.StateError, err.Error())
		return model.DeliveryFailed, fmt.Errorf("send claude input: %w", err)
	}
	if !protocolSent {
		c.mu.Lock()
		c.protocolSent = true
		c.mu.Unlock()
	}

	if status == model.DeliveryStarted {
		c.emitTurnStarted(entry)
		c.emitInputState(entry, model.RuntimeInputProcessing, model.ProcessingWorking, "accepted by Claude Code")
		c.setState(model.StateWorking, "")
	} else {
		c.emitInputState(entry, model.RuntimeInputProcessing, model.ProcessingWaiting, "queued by Claude Code")
	}
	return status, nil
}

func (c *ClaudeAdapter) emitTurnStarted(item claudePending) {
	e := runtimeEvent(model.ActorClaude, model.RuntimeTurnStarted)
	e.TurnID = item.turnID
	e.CorrelationID = item.input.MessageID
	c.sink(e)
}

func (c *ClaudeAdapter) emitInputState(item claudePending, kind string, state model.ProcessingState, detail string) {
	e := runtimeEvent(model.ActorClaude, kind)
	e.TurnID = item.turnID
	e.CorrelationID = item.input.MessageID
	e.Name = string(state)
	e.Text = detail
	c.sink(e)
}

func (c *ClaudeAdapter) removePending(messageID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, item := range c.pending {
		if item.input.MessageID == messageID {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			return
		}
	}
}

func (c *ClaudeAdapter) currentPending() (claudePending, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return claudePending{}, false
	}
	return c.pending[0], true
}

func (c *ClaudeAdapter) popPending() (claudePending, bool, *claudePending) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return claudePending{}, false, nil
	}
	item := c.pending[0]
	c.pending = c.pending[1:]
	if len(c.pending) == 0 {
		return item, true, nil
	}
	next := c.pending[0]
	return item, true, &next
}

func (c *ClaudeAdapter) takePending() []claudePending {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := append([]claudePending(nil), c.pending...)
	c.pending = nil
	c.output.Reset()
	c.fallback = ""
	return items
}

func (c *ClaudeAdapter) readStdout(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		c.handleLine(line)
	}
	if err := scanner.Err(); err != nil {
		e := runtimeEvent(model.ActorClaude, model.RuntimeError)
		e.Text = "read Claude stream: " + err.Error()
		c.sink(e)
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
		e := runtimeEvent(model.ActorClaude, model.RuntimeLog)
		e.Name = "stderr"
		e.Text = text
		c.sink(e)
	}
}

func (c *ClaudeAdapter) handleLine(line []byte) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		e := runtimeEvent(model.ActorClaude, model.RuntimeLog)
		e.Name = "stdout"
		e.Text = string(line)
		c.sink(e)
		return
	}
	var typ string
	_ = json.Unmarshal(raw["type"], &typ)
	pending, hasPending := c.currentPending()

	switch typ {
	case "system":
		var subtype, sessionID string
		_ = json.Unmarshal(raw["subtype"], &subtype)
		_ = json.Unmarshal(raw["session_id"], &sessionID)
		if subtype == "init" && sessionID != "" {
			c.mu.Lock()
			c.sessionID = sessionID
			c.mu.Unlock()
			e := runtimeEvent(model.ActorClaude, model.RuntimeSession)
			e.SessionID = sessionID
			e.Data = append(json.RawMessage(nil), line...)
			c.sink(e)
			c.updateRuntimeInfoFromInit(line)
			return
		}
		e := runtimeEvent(model.ActorClaude, model.RuntimeLog)
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
			e := runtimeEvent(model.ActorClaude, model.RuntimeTextDelta)
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
			e := runtimeEvent(model.ActorClaude, model.RuntimeFinal)
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
		completed := runtimeEvent(model.ActorClaude, model.RuntimeTurnCompleted)
		if ok {
			completed.TurnID = item.turnID
			completed.CorrelationID = item.input.MessageID
		}
		completed.Name = result.Subtype
		completed.Data = append(json.RawMessage(nil), line...)
		c.sink(completed)
		if result.CostUSD != 0 || result.Duration != 0 || len(result.Usage) > 0 {
			usage := runtimeEvent(model.ActorClaude, model.RuntimeUsageUpdated)
			if ok {
				usage.TurnID = item.turnID
				usage.CorrelationID = item.input.MessageID
			}
			usage.Data = append(json.RawMessage(nil), line...)
			c.sink(usage)
		}
		if !success {
			e := runtimeEvent(model.ActorClaude, model.RuntimeError)
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
		e := runtimeEvent(model.ActorClaude, model.RuntimeLog)
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
	emitRuntimeInfo(c.sink, model.ActorClaude, info)
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
			e := runtimeEvent(model.ActorClaude, model.RuntimeToolStarted)
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
		e := runtimeEvent(model.ActorClaude, model.RuntimeToolCompleted)
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

func (c *ClaudeAdapter) waitProcess(cmd *exec.Cmd) {
	err := cmd.Wait()
	c.mu.Lock()
	active := c.cmd == cmd
	intentional := c.intentional
	if active {
		c.cmd = nil
		c.stdin = nil
	}
	c.mu.Unlock()
	if !active {
		return
	}
	if intentional {
		c.setState(model.StateStopped, "")
		return
	}

	pending := c.takePending()
	detail := "Claude process exited"
	if err != nil {
		detail += ": " + err.Error()
	}
	for _, item := range pending {
		c.emitInputState(item, model.RuntimeInputFailed, model.ProcessingFailed, detail)
		completed := runtimeEvent(model.ActorClaude, model.RuntimeTurnCompleted)
		completed.TurnID = item.turnID
		completed.CorrelationID = item.input.MessageID
		completed.Name = "process_exited"
		c.sink(completed)
	}
	if err != nil || len(pending) > 0 {
		e := runtimeEvent(model.ActorClaude, model.RuntimeError)
		e.Text = detail
		c.sink(e)
		c.setState(model.StateError, detail)
		return
	}
	c.setState(model.StateStopped, "")
}

func (c *ClaudeAdapter) cancelPending(kind, detail string) {
	for _, item := range c.takePending() {
		c.emitInputState(item, model.RuntimeInputCancelled, model.ProcessingCancelled, detail)
		completed := runtimeEvent(model.ActorClaude, model.RuntimeTurnCompleted)
		completed.TurnID = item.turnID
		completed.CorrelationID = item.input.MessageID
		completed.Name = kind
		c.sink(completed)
	}
}

func (c *ClaudeAdapter) Interrupt(context.Context) error {
	c.mu.Lock()
	cmd := c.cmd
	stdin := c.stdin
	c.cmd = nil
	c.stdin = nil
	c.intentional = true
	c.mu.Unlock()
	c.cancelPending("interrupted", "interrupted by user")
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			_ = cmd.Process.Kill()
		}
	}
	c.setState(model.StateStopped, "interrupted; next message resumes the Claude session")
	return nil
}

func (c *ClaudeAdapter) Stop(context.Context) error {
	c.mu.Lock()
	cmd := c.cmd
	stdin := c.stdin
	c.cmd = nil
	c.stdin = nil
	c.intentional = true
	c.mu.Unlock()
	c.cancelPending("stopped", "Claude Code was stopped")
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	c.setState(model.StateStopped, "")
	return nil
}

func (c *ClaudeAdapter) ResolveApproval(context.Context, string, string) error {
	return ErrApprovalUnsupported
}
