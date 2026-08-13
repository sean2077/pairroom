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

	startMu     sync.Mutex
	submitMu    sync.Mutex
	mu          sync.Mutex
	writeMu     sync.Mutex
	state       model.AgentState
	sessionID   string
	resume      bool
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	pending     []claudePending
	output      strings.Builder
	intentional bool
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

func (c *ClaudeAdapter) Start(context.Context) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	c.mu.Lock()
	if c.cmd != nil && c.cmd.Process != nil {
		c.mu.Unlock()
		return nil
	}
	c.state = model.StateStarting
	c.intentional = false
	c.mu.Unlock()

	promptPath, err := c.ensurePromptFile()
	if err != nil {
		c.setState(model.StateError, err.Error())
		return err
	}

	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--replay-user-messages",
		"--forward-subagent-text",
		"--append-system-prompt-file", promptPath,
		"--permission-mode", c.cfg.PermissionMode,
	}
	if c.cfg.Model != "" {
		args = append(args, "--model", c.cfg.Model)
	}
	c.mu.Lock()
	if c.resume {
		args = append(args, "--resume", c.sessionID)
	} else {
		args = append(args, "--session-id", c.sessionID)
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

func (c *ClaudeAdapter) ensurePromptFile() (string, error) {
	dir := filepath.Join(c.cfg.DataDir, "runtime")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create claude runtime directory: %w", err)
	}
	path := filepath.Join(dir, "claude-pairroom-prompt.md")
	content := c.cfg.SystemPrompt
	if content == "" {
		content = prompt.SystemPrompt(model.ActorClaude, c.cfg.RoomName, c.cfg.Repo)
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write claude system prompt: %w", err)
	}
	return path, nil
}

func (c *ClaudeAdapter) Submit(ctx context.Context, input model.AgentInput) (model.DeliveryState, error) {
	// Claude processes streamed user messages in arrival order. Serialize the
	// pending-queue mutation with the write so correlation IDs always match the
	// order seen by the long-lived CLI process.
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
	c.pending = append(c.pending, entry)
	c.mu.Unlock()

	payload := map[string]any{
		"type":       "user",
		"uuid":       model.NewID("msg"),
		"session_id": c.SessionID(),
		"message": map[string]any{
			"role":    "user",
			"content": prompt.Envelope(input),
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

	started := runtimeEvent(model.ActorClaude, model.RuntimeTurnStarted)
	started.TurnID = entry.turnID
	started.CorrelationID = input.MessageID
	started.Text = string(status)
	c.sink(started)
	c.setState(model.StateWorking, "")
	return status, nil
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

func (c *ClaudeAdapter) popPending() (claudePending, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return claudePending{}, false, false
	}
	item := c.pending[0]
	c.pending = c.pending[1:]
	return item, true, len(c.pending) > 0
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
			return
		}
		e := runtimeEvent(model.ActorClaude, model.RuntimeLog)
		e.Name = "system." + subtype
		e.Data = append(json.RawMessage(nil), line...)
		c.sink(e)

	case "stream_event":
		var envelope struct {
			Event struct {
				Type  string `json:"type"`
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

	case "result":
		var result struct {
			Subtype   string  `json:"subtype"`
			Result    string  `json:"result"`
			SessionID string  `json:"session_id"`
			CostUSD   float64 `json:"total_cost_usd"`
			Duration  int64   `json:"duration_ms"`
			Error     string  `json:"error"`
		}
		_ = json.Unmarshal(line, &result)
		item, ok, more := c.popPending()
		c.mu.Lock()
		streamedText := c.output.String()
		c.output.Reset()
		c.mu.Unlock()
		if strings.TrimSpace(result.Result) == "" {
			result.Result = streamedText
		}
		if result.SessionID != "" {
			c.mu.Lock()
			c.sessionID = result.SessionID
			c.mu.Unlock()
		}
		if ok && strings.TrimSpace(result.Result) != "" {
			e := runtimeEvent(model.ActorClaude, model.RuntimeFinal)
			e.TurnID = item.turnID
			e.CorrelationID = item.input.MessageID
			e.Text = result.Result
			e.Data = append(json.RawMessage(nil), line...)
			c.sink(e)
		}
		completed := runtimeEvent(model.ActorClaude, model.RuntimeTurnCompleted)
		if ok {
			completed.TurnID = item.turnID
			completed.CorrelationID = item.input.MessageID
		}
		completed.Name = result.Subtype
		completed.Data = append(json.RawMessage(nil), line...)
		c.sink(completed)
		if result.Subtype != "success" && result.Error != "" {
			e := runtimeEvent(model.ActorClaude, model.RuntimeError)
			e.Text = result.Error
			c.sink(e)
		}
		if more {
			c.setState(model.StateWorking, "")
		} else {
			c.setState(model.StateIdle, "")
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
	for _, block := range message.Message.Content {
		if block.Type != "tool_use" {
			continue
		}
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

func (c *ClaudeAdapter) waitProcess(cmd *exec.Cmd) {
	err := cmd.Wait()
	c.mu.Lock()
	active := c.cmd == cmd
	intentional := c.intentional
	if active {
		c.cmd = nil
		c.stdin = nil
	}
	pending := len(c.pending)
	c.mu.Unlock()
	if !active {
		return
	}
	if intentional {
		c.setState(model.StateStopped, "")
		return
	}
	if err != nil {
		e := runtimeEvent(model.ActorClaude, model.RuntimeError)
		e.Text = "Claude process exited: " + err.Error()
		c.sink(e)
		c.setState(model.StateError, err.Error())
		return
	}
	if pending > 0 {
		c.setState(model.StateError, "Claude process exited with queued messages")
	} else {
		c.setState(model.StateStopped, "")
	}
}

func (c *ClaudeAdapter) Interrupt(context.Context) error {
	c.mu.Lock()
	cmd := c.cmd
	stdin := c.stdin
	c.cmd = nil
	c.stdin = nil
	c.intentional = true
	c.pending = nil
	c.output.Reset()
	c.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		_ = cmd.Process.Kill()
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
	c.pending = nil
	c.output.Reset()
	c.mu.Unlock()
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
