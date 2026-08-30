package agent

import (
	"bufio"
	"context"
	"encoding/base64"
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

type claudeApprovalRequest struct {
	requestID   string
	toolName    string
	input       json.RawMessage
	suggestions json.RawMessage
	approval    model.Approval
}

type claudeControlResult struct {
	response json.RawMessage
	err      error
}

type ClaudeAdapter struct {
	cfg  Config
	sink EventSink

	startMu      sync.Mutex
	submitMu     sync.Mutex
	mu           sync.Mutex
	writeMu      sync.Mutex
	controlMu    sync.Mutex
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
	role         model.ParticipantRole
	baseMode     string
	approvals    map[string]claudeApprovalRequest
	control      map[string]chan claudeControlResult
	controlReady bool
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
		sessionID: sessionID, resume: resume, role: model.RoleDriver,
		baseMode: cfg.PermissionMode, approvals: make(map[string]claudeApprovalRequest),
		control: make(map[string]chan claudeControlResult),
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
	c.controlMu.Lock()
	c.controlReady = false
	c.controlMu.Unlock()

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

	args := append([]string(nil), c.cfg.CommandArgs...)
	args = append(args, "-p", "--input-format", "stream-json", "--output-format", "stream-json")
	if flags["--verbose"] {
		args = append(args, "--verbose")
	}
	args = appendClaudeStreamFlags(args, flags, c.cfg.RequireExactSession)
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
	if flags["--add-dir"] && c.cfg.DataDir != "" {
		attachmentDir := filepath.Join(c.cfg.DataDir, "attachments")
		if info, err := os.Stat(attachmentDir); err == nil && info.IsDir() {
			args = append(args, "--add-dir", attachmentDir)
		}
	}
	// The official Claude Agent SDK enables canUseTool by routing permission
	// prompts over the stream-json control channel. PairRoom follows the same
	// contract so the CLI emits can_use_tool requests instead of falling back
	// to an interactive terminal prompt that a headless process cannot show.
	if flags["--permission-prompt-tool"] {
		args = append(args, "--permission-prompt-tool", "stdio")
	}
	if flags["--permission-mode"] && c.cfg.PermissionMode != "" {
		args = append(args, "--permission-mode", c.cfg.PermissionMode)
	}
	c.mu.Lock()
	role := c.role
	c.mu.Unlock()
	if role == model.RoleReviewer && flags["--disallowedTools"] {
		// Plan mode prevents direct execution, while explicit deny rules remove
		// the native write tools and ExitPlanMode from the reviewer context. Bash
		// remains available behind Claude's permission flow so the human can allow
		// a genuinely read-only inspection command when useful.
		args = append(args, "--disallowedTools", strings.Join([]string{"Edit", "Write", "NotebookEdit", "ExitPlanMode"}, ","))
	}
	if flags["--model"] && c.cfg.Model != "" {
		args = append(args, "--model", c.cfg.Model)
	}

	c.mu.Lock()
	expectedSession := c.sessionID
	strictResume := c.cfg.RequireExactSession && c.resume && expectedSession != ""
	if c.resume && flags["--resume"] {
		args = append(args, "--resume="+c.sessionID)
	} else if !c.resume && flags["--session-id"] {
		args = append(args, "--session-id="+c.sessionID)
	} else if c.resume && !flags["--resume"] {
		if strictResume {
			c.mu.Unlock()
			err := fmt.Errorf("Claude Code cannot resume required session %q because this CLI does not expose --resume", expectedSession)
			c.setState(model.StateError, err.Error())
			return err
		}
		// The legacy single-Room command retains its historical fallback. Service
		// runtimes set RequireExactSession and therefore fail closed above.
		c.sessionID = newUUID()
		c.resume = false
	}
	c.mu.Unlock()

	cmd := exec.Command(c.cfg.Command, args...)
	cmd.Dir = c.cfg.Repo
	cmd.Env = mergeRuntimeEnv(envWithout("CLAUDECODE"), c.cfg.Env)
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

	go c.readStdout(stdout)
	go c.readStderr(stderr)
	go c.waitProcess(cmd)

	initCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	initErr := c.initializeControl(initCtx)
	cancel()
	if initErr != nil {
		detail := "initialize Claude control protocol: " + initErr.Error()
		c.mu.Lock()
		if c.cmd == cmd {
			c.intentional = true
			c.cmd = nil
			c.stdin = nil
		}
		c.mu.Unlock()
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		c.failControlWaiters(errors.New(detail))
		c.setState(model.StateError, detail)
		return errors.New(detail)
	}

	if strictResume {
		actualSession := c.SessionID()
		if actualSession != expectedSession {
			_ = c.Stop(context.Background())
			err := fmt.Errorf("Claude Code resumed session %q instead of required session %q", actualSession, expectedSession)
			c.setState(model.StateError, err.Error())
			return err
		}
	}
	c.setState(model.StateIdle, "")
	session := runtimeEvent(model.ActorClaude, model.RuntimeSession)
	session.SessionID = c.SessionID()
	c.sink(session)
	return nil
}

func appendClaudeStreamFlags(args []string, flags map[string]bool, strictResume bool) []string {
	for _, optional := range []string{
		"--include-partial-messages",
		"--replay-user-messages",
		"--forward-subagent-text",
		"--include-hook-events",
	} {
		// Service-owned Rooms restore the vendor context but deliberately do not
		// import or expose messages that predate the PairRoom binding boundary.
		// Claude's replay flag would stream those prior user messages back into
		// the adapter, so it is never enabled for an exact durable binding.
		if optional == "--replay-user-messages" && strictResume {
			continue
		}
		if flags[optional] {
			args = append(args, optional)
		}
	}
	return args
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

	c.mu.Lock()
	protocolSent := c.protocolSent
	c.mu.Unlock()

	text := prompt.Envelope(input)
	if !protocolSent {
		systemPrompt := c.cfg.SystemPrompt
		if systemPrompt == "" {
			systemPrompt = prompt.SystemPrompt(model.ActorClaude, c.cfg.RoomName, c.cfg.Repo)
		}
		text = systemPrompt + "\n\n" + text
	}
	content, err := claudeInputContent(text, input.Attachments)
	if err != nil {
		return model.DeliveryFailed, err
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
			"content": content,
		},
		"parent_tool_use_id": nil,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return model.DeliveryFailed, fmt.Errorf("encode claude input: %w", err)
	}

	entry := claudePending{input: input, turnID: model.NewID("claude-turn")}
	c.mu.Lock()
	status := model.DeliveryStarted
	if c.state == model.StateWorking || len(c.pending) > 0 {
		status = model.DeliveryQueued
	}
	c.pending = append(c.pending, entry)
	c.mu.Unlock()

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

const (
	maxClaudeImageBytes      int64 = 5 << 20
	maxClaudeImagesPerInput        = 8
	maxClaudeTotalImageBytes int64 = 20 << 20
)

var claudeImageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// claudeInputContent produces the native Claude streaming-input shape. Text-only
// messages remain strings for the smallest wire representation; multimodal
// messages use standard text and base64 image content blocks. PairRoom reads
// only attachment-store files that were resolved by the room boundary.
func claudeInputContent(text string, attachments []model.AgentAttachment) (any, error) {
	if len(attachments) == 0 {
		return text, nil
	}
	if len(attachments) > maxClaudeImagesPerInput {
		return nil, fmt.Errorf("Claude input includes %d images; limit is %d", len(attachments), maxClaudeImagesPerInput)
	}
	// Anthropic recommends placing images before the text query when possible.
	blocks := make([]any, 0, len(attachments)+1)
	var total int64
	for _, value := range attachments {
		if value.Kind != "image" || !claudeImageMediaTypes[value.MediaType] {
			return nil, fmt.Errorf("attachment %q has unsupported Claude image type %q", value.Name, value.MediaType)
		}
		if strings.TrimSpace(value.Path) == "" {
			return nil, fmt.Errorf("attachment %q is missing its native image path", value.Name)
		}
		file, err := os.Open(value.Path)
		if err != nil {
			return nil, fmt.Errorf("open Claude image %q: %w", value.Name, err)
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("inspect Claude image %q: %w", value.Name, statErr)
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxClaudeImageBytes {
			_ = file.Close()
			return nil, fmt.Errorf("Claude image %q is not a regular image within the %d MiB limit", value.Name, maxClaudeImageBytes>>20)
		}
		if value.Size > 0 && info.Size() != value.Size {
			_ = file.Close()
			return nil, fmt.Errorf("Claude image %q changed after attachment validation", value.Name)
		}
		total += info.Size()
		if total > maxClaudeTotalImageBytes {
			_ = file.Close()
			return nil, fmt.Errorf("Claude images exceed the %d MiB total input limit", maxClaudeTotalImageBytes>>20)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxClaudeImageBytes+1))
		closeErr := file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read Claude image %q: %w", value.Name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close Claude image %q: %w", value.Name, closeErr)
		}
		if int64(len(data)) != info.Size() {
			return nil, fmt.Errorf("Claude image %q changed while it was being read", value.Name)
		}
		blocks = append(blocks, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": value.MediaType,
				"data":       base64.StdEncoding.EncodeToString(data),
			},
		})
	}
	blocks = append(blocks, map[string]any{"type": "text", "text": text})
	return blocks, nil
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

func (c *ClaudeAdapter) clearApprovals() {
	c.mu.Lock()
	c.approvals = make(map[string]claudeApprovalRequest)
	c.mu.Unlock()
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
	emitRuntimeInfo(c.sink, model.ActorClaude, runtimeInfo)

	e := runtimeEvent(model.ActorClaude, model.RuntimeLog)
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
		e := runtimeEvent(model.ActorClaude, model.RuntimeLog)
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
		e := runtimeEvent(model.ActorClaude, model.RuntimeLog)
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
		e := runtimeEvent(model.ActorClaude, model.RuntimeLog)
		e.Name = "control_request.invalid"
		e.Data = append(json.RawMessage(nil), line...)
		c.sink(e)
		return
	}
	if envelope.Request.Subtype != "can_use_tool" {
		_ = c.writeControlError(envelope.RequestID, "PairRoom does not implement Claude control request subtype "+envelope.Request.Subtype)
		e := runtimeEvent(model.ActorClaude, model.RuntimeLog)
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
		e := runtimeEvent(model.ActorClaude, model.RuntimeError)
		e.Text = "Claude emitted a control request outside a PairRoom-authored turn"
		c.sink(e)
		return
	}

	c.mu.Lock()
	role := c.role
	c.mu.Unlock()
	if role == model.RoleReviewer {
		switch envelope.Request.ToolName {
		case "Edit", "Write", "NotebookEdit", "ExitPlanMode":
			_ = c.writeControlResponse(envelope.RequestID, map[string]any{
				"behavior": "deny",
				"message":  "PairRoom reviewer role cannot use " + envelope.Request.ToolName + "; change the participant role first",
			})
			e := runtimeEvent(model.ActorClaude, model.RuntimeLog)
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
		title = "Approve Claude " + envelope.Request.ToolName
	}
	kind := "claude.toolApproval"
	if envelope.Request.ToolName == "AskUserQuestion" {
		kind = "claude.userQuestion"
		title = "Claude asks for input"
	}
	approval := model.Approval{
		ID: model.NewID("approval"), Agent: model.ActorClaude, Kind: kind,
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
	e := runtimeEvent(model.ActorClaude, model.RuntimeApprovalRequested)
	if hasPending {
		e.TurnID = pending.turnID
		e.CorrelationID = pending.input.MessageID
	}
	e.Approval = &approval
	e.Data = append(json.RawMessage(nil), line...)
	c.sink(e)
	c.setState(model.StateWaiting, "waiting for approval")
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
	c.failControlWaiters(errors.New("Claude process exited"))
	if intentional {
		c.clearApprovals()
		c.setState(model.StateStopped, "")
		return
	}

	c.clearApprovals()
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
	c.clearApprovals()
	c.failControlWaiters(errors.New("Claude Code was interrupted"))
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
	c.clearApprovals()
	c.failControlWaiters(errors.New("Claude Code was stopped"))
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

func (c *ClaudeAdapter) ResolveApproval(_ context.Context, approvalID string, resolution model.ApprovalResolution) error {
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
			if len(resolution.Answers) == 0 {
				return errors.New("Claude question approval requires answers")
			}
			if _, ok := updatedInput["questions"]; !ok {
				return errors.New("Claude question request omitted questions")
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

// SetRole maps PairRoom's reviewer role to Claude Code's native plan mode.
// Role changes are rejected while a turn or permission prompt is active; this
// avoids silently changing a harness policy midway through execution.
func (c *ClaudeAdapter) SetRole(ctx context.Context, role model.ParticipantRole) error {
	if !role.Valid() {
		return fmt.Errorf("invalid Claude role %q", role)
	}
	desiredMode := c.baseMode
	if desiredMode == "" {
		desiredMode = "auto"
	}
	if role == model.RoleReviewer {
		desiredMode = "plan"
	}

	c.mu.Lock()
	if c.role == role && c.cfg.PermissionMode == desiredMode {
		c.mu.Unlock()
		return nil
	}
	state := c.state
	if state == model.StateWorking || state == model.StateWaiting || state == model.StateStarting || len(c.pending) > 0 || len(c.approvals) > 0 {
		c.mu.Unlock()
		return errors.New("interrupt or stop Claude before changing its role")
	}
	wasRunning := c.cmd != nil && c.cmd.Process != nil
	oldMode, oldRole := c.cfg.PermissionMode, c.role
	c.cfg.PermissionMode = desiredMode
	c.role = role
	c.mu.Unlock()

	if !wasRunning {
		return nil
	}
	if err := c.Stop(ctx); err != nil {
		c.mu.Lock()
		c.cfg.PermissionMode, c.role = oldMode, oldRole
		c.mu.Unlock()
		return err
	}
	if err := c.Start(ctx); err != nil {
		c.mu.Lock()
		c.cfg.PermissionMode, c.role = oldMode, oldRole
		c.mu.Unlock()
		return fmt.Errorf("restart Claude with %s role: %w", role, err)
	}
	return nil
}

// SetWorkspace changes the process working directory only at a safe turn
// boundary. A running idle process is restarted so Claude Code reloads the
// correct project instructions, hooks, skills and Git context from that path.
func (c *ClaudeAdapter) SetWorkspace(ctx context.Context, workspace string) error {
	workspace = filepath.Clean(strings.TrimSpace(workspace))
	if workspace == "." || workspace == "" {
		return errors.New("Claude workspace is required")
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return fmt.Errorf("stat Claude workspace: %w", err)
	}
	if !info.IsDir() {
		return errors.New("Claude workspace is not a directory")
	}

	c.mu.Lock()
	if filepath.Clean(c.cfg.Repo) == workspace {
		c.mu.Unlock()
		return nil
	}
	if c.state == model.StateWorking || c.state == model.StateWaiting || c.state == model.StateStarting || len(c.pending) > 0 || len(c.approvals) > 0 {
		c.mu.Unlock()
		return errors.New("interrupt or stop Claude before changing its workspace")
	}
	wasRunning := c.cmd != nil && c.cmd.Process != nil
	old := c.cfg.Repo
	c.cfg.Repo = workspace
	c.protocolSent = false
	c.mu.Unlock()

	if !wasRunning {
		return nil
	}
	if err := c.Stop(ctx); err != nil {
		c.mu.Lock()
		c.cfg.Repo = old
		c.mu.Unlock()
		return err
	}
	if err := c.Start(ctx); err != nil {
		c.mu.Lock()
		c.cfg.Repo = old
		c.mu.Unlock()
		return fmt.Errorf("restart Claude in reviewer workspace: %w", err)
	}
	return nil
}
