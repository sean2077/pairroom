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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
	"github.com/sean2077/pairroom/internal/version"
)

type rpcReply struct {
	result json.RawMessage
	err    error
}

type pendingApproval struct {
	rawID    json.RawMessage
	method   string
	params   json.RawMessage
	approval model.Approval
}

type CodexAdapter struct {
	cfg  Config
	sink EventSink

	startMu      sync.Mutex
	submitMu     sync.Mutex
	mu           sync.Mutex
	writeMu      sync.Mutex
	state        model.AgentState
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	threadID     string
	currentTurn  string
	protocolSent bool
	intentional  bool
	pending      map[int64]chan rpcReply
	approvals    map[string]pendingApproval
	turnInputs   map[string][]model.AgentInput
	// wireInputs holds inputs keyed by Codex's documented
	// clientUserMessageId while a turn/start or turn/steer request is in flight.
	// The matching userMessage item echoes this value as clientId, allowing
	// notifications that arrive before the RPC response to retain exact room
	// message correlation.
	wireInputs    map[string]model.AgentInput
	startingInput *model.AgentInput
	turnBuffers   map[string]*strings.Builder
	turnFinal     map[string]string
	queued        []model.AgentInput
	nextRequestID atomic.Int64
}

type codexRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e codexRPCError) Error() string {
	if e.Code == 0 {
		return e.Message
	}
	return fmt.Sprintf("codex rpc error %d: %s", e.Code, e.Message)
}

func NewCodex(cfg Config, sink EventSink) *CodexAdapter {
	if cfg.Command == "" {
		cfg.Command = "codex"
	}
	if cfg.ApprovalPolicy == "" {
		cfg.ApprovalPolicy = "unlessTrusted"
	}
	if cfg.Sandbox == "" {
		cfg.Sandbox = "workspaceWrite"
	}
	adapter := &CodexAdapter{
		cfg: cfg, sink: sink, state: model.StateStopped, threadID: cfg.SessionID,
		pending: make(map[int64]chan rpcReply), approvals: make(map[string]pendingApproval),
		turnInputs:  make(map[string][]model.AgentInput),
		wireInputs:  make(map[string]model.AgentInput),
		turnBuffers: make(map[string]*strings.Builder),
		turnFinal:   make(map[string]string),
	}
	adapter.nextRequestID.Store(100)
	return adapter
}

func (c *CodexAdapter) Actor() model.ActorID { return model.ActorCodex }

func (c *CodexAdapter) State() model.AgentState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *CodexAdapter) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.threadID
}

func (c *CodexAdapter) setState(state model.AgentState, detail string) {
	c.mu.Lock()
	changed := c.state != state
	c.state = state
	c.mu.Unlock()
	if !changed && detail == "" {
		return
	}
	e := runtimeEvent(model.ActorCodex, model.RuntimeState)
	e.State = state
	e.Text = detail
	c.sink(e)
}

func (c *CodexAdapter) Start(ctx context.Context) error {
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

	probe, probeErr := ProbeRuntime(ctx, Config{
		Actor: model.ActorCodex, Command: c.cfg.Command, Model: c.cfg.Model,
		ApprovalPolicy: c.cfg.ApprovalPolicy, Sandbox: c.cfg.Sandbox,
	})
	if probeErr != nil {
		info := model.RuntimeInfo{
			Available: false, Command: c.cfg.Command, Protocol: "codex-app-server-jsonrpc",
			Model: c.cfg.Model, ApprovalPolicy: c.cfg.ApprovalPolicy, Sandbox: c.cfg.Sandbox,
			Warnings: []string{probeErr.Error()}, ProbedAt: time.Now().UTC(),
		}
		emitRuntimeInfo(c.sink, model.ActorCodex, info)
		c.setState(model.StateError, probeErr.Error())
		return probeErr
	} else {
		emitRuntimeInfo(c.sink, model.ActorCodex, probe.RuntimeInfo(c.cfg))
	}

	cmd := exec.Command(c.cfg.Command, "app-server")
	cmd.Dir = c.cfg.Repo
	cmd.Env = envWithout("CODEX_INTERNAL_ORIGINATOR")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		c.setState(model.StateError, err.Error())
		return fmt.Errorf("codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		c.setState(model.StateError, err.Error())
		return fmt.Errorf("codex stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		c.setState(model.StateError, err.Error())
		return fmt.Errorf("codex stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		c.setState(model.StateError, err.Error())
		return fmt.Errorf("start codex app-server: %w", err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.mu.Unlock()
	go c.readStdout(stdout)
	go c.readStderr(stderr)
	go c.waitProcess(cmd)

	handshakeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	clientVersion := c.cfg.ClientVersion
	if clientVersion == "" {
		clientVersion = version.Current
	}
	initializeResult, err := c.call(handshakeCtx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name": "pairroom", "title": "PairRoom", "version": clientVersion,
		},
	})
	if err != nil {
		_ = c.Stop(context.Background())
		return fmt.Errorf("initialize codex app-server: %w", err)
	}
	c.emitInitializeRuntimeInfo(initializeResult, probe, probeErr)
	if err := c.notify("initialized", map[string]any{}); err != nil {
		_ = c.Stop(context.Background())
		return fmt.Errorf("acknowledge codex initialization: %w", err)
	}

	c.mu.Lock()
	existingThread := c.threadID
	c.mu.Unlock()
	var result json.RawMessage
	if existingThread != "" {
		result, err = c.call(handshakeCtx, "thread/resume", map[string]any{
			"threadId": existingThread,
			"cwd":      c.cfg.Repo,
		})
		if err != nil {
			logEvent := runtimeEvent(model.ActorCodex, model.RuntimeLog)
			logEvent.Name = "thread.resume"
			logEvent.Text = "resume failed; creating a new Codex thread: " + err.Error()
			c.sink(logEvent)
			existingThread = ""
			// The collaboration protocol may already have been sent to the old
			// thread. A replacement thread has no such context, so make the next
			// user turn include it again.
			c.mu.Lock()
			c.protocolSent = false
			c.mu.Unlock()
		}
	}
	if existingThread == "" {
		params := map[string]any{
			"cwd":            c.cfg.Repo,
			"approvalPolicy": c.cfg.ApprovalPolicy,
			"sandbox":        c.legacySandbox(),
			"serviceName":    "pairroom",
		}
		if c.cfg.Model != "" {
			params["model"] = c.cfg.Model
		}
		result, err = c.call(handshakeCtx, "thread/start", params)
	}
	if err != nil {
		_ = c.Stop(context.Background())
		return fmt.Errorf("start/resume codex thread: %w", err)
	}
	var threadResult struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &threadResult); err != nil || threadResult.Thread.ID == "" {
		_ = c.Stop(context.Background())
		if err == nil {
			err = errors.New("missing thread id")
		}
		return fmt.Errorf("decode codex thread: %w", err)
	}
	c.mu.Lock()
	c.threadID = threadResult.Thread.ID
	c.mu.Unlock()
	c.setState(model.StateIdle, "")
	session := runtimeEvent(model.ActorCodex, model.RuntimeSession)
	session.SessionID = threadResult.Thread.ID
	c.sink(session)
	return nil
}

func (c *CodexAdapter) Submit(ctx context.Context, input model.AgentInput) (model.DeliveryState, error) {
	// A Codex thread accepts one active turn. Serialize submissions so two room
	// deliveries cannot both observe an idle thread and race turn/start.
	c.submitMu.Lock()
	defer c.submitMu.Unlock()

	if err := c.Start(ctx); err != nil {
		return model.DeliveryFailed, err
	}

	text := prompt.Envelope(input)
	c.mu.Lock()
	includeProtocol := !c.protocolSent
	if includeProtocol {
		protocolText := c.cfg.SystemPrompt
		if protocolText == "" {
			protocolText = prompt.SystemPrompt(model.ActorCodex, c.cfg.RoomName, c.cfg.Repo)
		}
		text = protocolText + "\n\n" + text
	}
	threadID := c.threadID
	turnID := c.currentTurn
	active := turnID != "" && (c.state == model.StateWorking || c.state == model.StateWaiting)
	c.mu.Unlock()

	if active {
		c.stageWireInput(input)
		result, err := c.call(ctx, "turn/steer", codexTurnSteerParams(threadID, turnID, text, input))
		c.unstageWireInput(input.MessageID)
		if err == nil {
			_ = result
			c.mu.Lock()
			if includeProtocol {
				c.protocolSent = true
			}
			c.mu.Unlock()
			if c.bindTurnInput(turnID, input) {
				c.emitInputProcessing(turnID, input, "injected into active Codex turn")
			}
			return model.DeliveryInjected, nil
		}
		// Review/compaction turns and a narrow completion race can reject
		// steering. Preserve the user's intervention and start it at the next
		// safe turn boundary instead of dropping it.
		c.mu.Lock()
		c.queued = append(c.queued, input)
		c.mu.Unlock()
		logEvent := runtimeEvent(model.ActorCodex, model.RuntimeLog)
		logEvent.Name = "turn.steer.queued"
		logEvent.CorrelationID = input.MessageID
		logEvent.Text = err.Error()
		c.sink(logEvent)
		waiting := runtimeEvent(model.ActorCodex, model.RuntimeInputProcessing)
		waiting.CorrelationID = input.MessageID
		waiting.Name = string(model.ProcessingWaiting)
		waiting.Text = "queued after Codex rejected active-turn steering: " + err.Error()
		c.sink(waiting)
		go c.tryStartQueued()
		return model.DeliveryQueued, nil
	}

	params := c.turnStartParams(threadID, text, input)
	// turn/started can arrive before the turn/start response. Keep the input in
	// a temporary slot so either ordering receives the correct correlation ID.
	starting := input
	c.mu.Lock()
	c.startingInput = &starting
	c.wireInputs[input.MessageID] = input
	c.mu.Unlock()
	result, err := c.call(ctx, "turn/start", params)
	c.unstageWireInput(input.MessageID)
	if err != nil {
		c.mu.Lock()
		c.startingInput = nil
		c.mu.Unlock()
		return model.DeliveryFailed, err
	}
	var turnResult struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &turnResult); err != nil || turnResult.Turn.ID == "" {
		c.mu.Lock()
		c.startingInput = nil
		c.mu.Unlock()
		if err == nil {
			err = errors.New("missing turn id")
		}
		return model.DeliveryFailed, fmt.Errorf("decode codex turn: %w", err)
	}
	c.mu.Lock()
	if includeProtocol {
		c.protocolSent = true
	}
	c.currentTurn = turnResult.Turn.ID
	c.startingInput = nil
	if c.turnBuffers[turnResult.Turn.ID] == nil {
		c.turnBuffers[turnResult.Turn.ID] = &strings.Builder{}
	}
	c.mu.Unlock()
	if c.bindTurnInput(turnResult.Turn.ID, input) {
		c.emitInputProcessing(turnResult.Turn.ID, input, "started Codex turn")
	}
	c.setState(model.StateWorking, "")
	return model.DeliveryStarted, nil
}

func codexTurnSteerParams(threadID, turnID, text string, input model.AgentInput) map[string]any {
	params := map[string]any{
		"threadId":       threadID,
		"expectedTurnId": turnID,
		"input":          codexInputItems(text, input.Attachments),
	}
	if input.MessageID != "" {
		params["clientUserMessageId"] = input.MessageID
	}
	return params
}

func (c *CodexAdapter) turnStartParams(threadID, text string, input model.AgentInput) map[string]any {
	params := map[string]any{
		"threadId":       threadID,
		"input":          codexInputItems(text, input.Attachments),
		"cwd":            c.cfg.Repo,
		"approvalPolicy": c.cfg.ApprovalPolicy,
		"sandboxPolicy":  c.sandboxPolicy(input.Role),
	}
	if input.MessageID != "" {
		params["clientUserMessageId"] = input.MessageID
	}
	if c.cfg.Model != "" {
		params["model"] = c.cfg.Model
	}
	if c.cfg.Effort != "" {
		params["effort"] = c.cfg.Effort
	}
	return params
}

func codexInputItems(text string, attachments []model.AgentAttachment) []any {
	items := make([]any, 0, 1+len(attachments))
	items = append(items, map[string]any{"type": "text", "text": text})
	for _, value := range attachments {
		if value.Path == "" || !strings.HasPrefix(strings.ToLower(value.MediaType), "image/") {
			continue
		}
		items = append(items, map[string]any{"type": "localImage", "path": value.Path})
	}
	return items
}

func (c *CodexAdapter) legacySandbox() string {
	switch strings.ToLower(c.cfg.Sandbox) {
	case "readonly", "read_only", "read-only":
		return "readOnly"
	case "dangerfullaccess", "danger_full_access", "danger-full-access", "full", "fullaccess", "full_access", "full-access":
		return "dangerFullAccess"
	default:
		return "workspaceWrite"
	}
}

func (c *CodexAdapter) sandboxPolicy(role model.ParticipantRole) map[string]any {
	if role == model.RoleReviewer {
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

func (c *CodexAdapter) stageWireInput(input model.AgentInput) {
	if input.MessageID == "" {
		return
	}
	c.mu.Lock()
	c.wireInputs[input.MessageID] = input
	c.mu.Unlock()
}

func (c *CodexAdapter) unstageWireInput(messageID string) {
	if messageID == "" {
		return
	}
	c.mu.Lock()
	delete(c.wireInputs, messageID)
	c.mu.Unlock()
}

func (c *CodexAdapter) bindTurnInput(turnID string, input model.AgentInput) bool {
	if turnID == "" || input.MessageID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, existing := range c.turnInputs[turnID] {
		if existing.MessageID == input.MessageID {
			return false
		}
	}
	c.turnInputs[turnID] = append(c.turnInputs[turnID], input)
	return true
}

func (c *CodexAdapter) latestTurnInputLocked(turnID string) model.AgentInput {
	inputs := c.turnInputs[turnID]
	if len(inputs) == 0 {
		return model.AgentInput{}
	}
	return inputs[len(inputs)-1]
}

func (c *CodexAdapter) emitInputProcessing(turnID string, input model.AgentInput, detail string) {
	e := runtimeEvent(model.ActorCodex, model.RuntimeInputProcessing)
	e.TurnID = turnID
	e.CorrelationID = input.MessageID
	e.Name = string(model.ProcessingWorking)
	e.Text = detail
	c.sink(e)
}

func (c *CodexAdapter) emitInputTerminal(turnID string, input model.AgentInput, kind, detail string) {
	e := runtimeEvent(model.ActorCodex, kind)
	e.TurnID = turnID
	e.CorrelationID = input.MessageID
	e.Text = detail
	c.sink(e)
}

func (c *CodexAdapter) emitInitializeRuntimeInfo(result json.RawMessage, probe ProbeResult, probeErr error) {
	info := model.RuntimeInfo{
		Available: true, Command: c.cfg.Command, Protocol: "codex-app-server-jsonrpc",
		Model: c.cfg.Model, ApprovalPolicy: c.cfg.ApprovalPolicy, Sandbox: c.cfg.Sandbox,
		ProbedAt: time.Now().UTC(),
	}
	if probeErr == nil {
		info = probe.RuntimeInfo(c.cfg)
		info.ProbedAt = time.Now().UTC()
	}
	var payload struct {
		UserAgent      string `json:"userAgent"`
		PlatformFamily string `json:"platformFamily"`
		PlatformOS     string `json:"platformOs"`
	}
	_ = json.Unmarshal(result, &payload)
	if version := extractSemanticVersion(payload.UserAgent); version != "" {
		info.Version = version
	}
	info.Data, _ = json.Marshal(map[string]any{
		"user_agent":      payload.UserAgent,
		"platform_family": payload.PlatformFamily,
		"platform_os":     payload.PlatformOS,
		"capabilities":    info.Capabilities,
	})
	emitRuntimeInfo(c.sink, model.ActorCodex, info)
}

func (c *CodexAdapter) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		result, err := c.callOnce(ctx, method, params)
		var rpcErr codexRPCError
		if err == nil || !errors.As(err, &rpcErr) || rpcErr.Code != -32001 || attempt >= 4 {
			return result, err
		}
		delay := time.Duration(100*(1<<attempt))*time.Millisecond + time.Duration(time.Now().UnixNano()%75)*time.Millisecond
		logEvent := runtimeEvent(model.ActorCodex, model.RuntimeLog)
		logEvent.Name = "app-server.overloaded.retry"
		logEvent.Text = fmt.Sprintf("%s rejected as overloaded; retrying in %s (attempt %d/5)", method, delay, attempt+2)
		c.sink(logEvent)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *CodexAdapter) callOnce(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextRequestID.Add(1)
	ch := make(chan rpcReply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply.result, reply.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *CodexAdapter) notify(method string, params any) error {
	return c.send(map[string]any{"method": method, "params": params})
}

func (c *CodexAdapter) send(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode codex rpc message: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("codex stdin is not available")
	}
	if _, err := stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write codex rpc message: %w", err)
	}
	return nil
}

func (c *CodexAdapter) readStdout(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		c.handleRPCLine(line)
	}
	if err := scanner.Err(); err != nil {
		e := runtimeEvent(model.ActorCodex, model.RuntimeError)
		e.Text = "read Codex stream: " + err.Error()
		c.sink(e)
	}
}

func (c *CodexAdapter) readStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16*1024), 1024*1024)
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		e := runtimeEvent(model.ActorCodex, model.RuntimeLog)
		e.Name = "stderr"
		e.Text = text
		c.sink(e)
	}
}

func (c *CodexAdapter) handleRPCLine(line []byte) {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *codexRPCError  `json:"error"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		e := runtimeEvent(model.ActorCodex, model.RuntimeLog)
		e.Name = "stdout"
		e.Text = string(line)
		c.sink(e)
		return
	}
	if envelope.Method != "" {
		if len(envelope.ID) > 0 && string(envelope.ID) != "null" {
			c.handleServerRequest(envelope.ID, envelope.Method, envelope.Params)
			return
		}
		c.handleNotification(envelope.Method, envelope.Params)
		return
	}
	if len(envelope.ID) == 0 {
		return
	}
	var id int64
	if err := json.Unmarshal(envelope.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch == nil {
		return
	}
	if envelope.Error != nil {
		ch <- rpcReply{err: *envelope.Error}
	} else {
		ch <- rpcReply{result: envelope.Result}
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
		e := runtimeEvent(model.ActorCodex, model.RuntimeLog)
		e.Name = "server_request.unsupported"
		e.Text = method
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)
		return
	}
	displayID := model.NewID("approval")
	title := "Approve Codex command"
	if fileApproval {
		title = "Approve Codex file change"
	} else if permissionApproval {
		title = "Grant Codex additional permissions"
	}
	approval := model.Approval{
		ID: displayID, Agent: model.ActorCodex, Kind: method, Title: title,
		Detail: append(json.RawMessage(nil), params...), Status: "pending", RequestedAt: time.Now().UTC(),
	}
	c.mu.Lock()
	c.approvals[displayID] = pendingApproval{
		rawID: append(json.RawMessage(nil), rawID...), method: method,
		params: append(json.RawMessage(nil), params...), approval: approval,
	}
	c.mu.Unlock()
	e := runtimeEvent(model.ActorCodex, model.RuntimeApprovalRequested)
	e.Approval = &approval
	e.Data = append(json.RawMessage(nil), params...)
	c.sink(e)
	c.setState(model.StateWaiting, "waiting for approval")
}

func (c *CodexAdapter) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &p)
		newlyBound := false
		c.mu.Lock()
		if p.Turn.ID != "" {
			c.currentTurn = p.Turn.ID
			if c.turnBuffers[p.Turn.ID] == nil {
				c.turnBuffers[p.Turn.ID] = &strings.Builder{}
			}
		}
		if len(c.turnInputs[p.Turn.ID]) == 0 && c.startingInput != nil {
			// App-server may notify turn/started before replying to turn/start.
			// Bind the in-flight input now so Inspector and final events keep the
			// same room-message correlation regardless of wire ordering.
			c.turnInputs[p.Turn.ID] = append(c.turnInputs[p.Turn.ID], *c.startingInput)
			newlyBound = true
		}
		input := c.latestTurnInputLocked(p.Turn.ID)
		c.mu.Unlock()
		c.setState(model.StateWorking, "")
		e := runtimeEvent(model.ActorCodex, model.RuntimeTurnStarted)
		e.TurnID = p.Turn.ID
		e.CorrelationID = input.MessageID
		c.sink(e)
		if newlyBound {
			c.emitInputProcessing(p.Turn.ID, input, "started Codex turn")
		}

	case "item/agentMessage/delta":
		var p struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}
		_ = json.Unmarshal(params, &p)
		c.mu.Lock()
		builder := c.turnBuffers[p.TurnID]
		if builder == nil {
			builder = &strings.Builder{}
			c.turnBuffers[p.TurnID] = builder
		}
		builder.WriteString(p.Delta)
		input := c.latestTurnInputLocked(p.TurnID)
		c.mu.Unlock()
		e := runtimeEvent(model.ActorCodex, model.RuntimeTextDelta)
		e.TurnID = p.TurnID
		e.ItemID = p.ItemID
		e.CorrelationID = input.MessageID
		e.Text = p.Delta
		c.sink(e)

	case "item/started", "item/completed":
		c.handleItem(method, params)

	case "item/commandExecution/outputDelta":
		var p struct {
			TurnID string `json:"turnId"`
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		_ = json.Unmarshal(params, &p)
		e := runtimeEvent(model.ActorCodex, model.RuntimeCommandOutput)
		e.TurnID, e.ItemID, e.Text = p.TurnID, p.ItemID, p.Delta
		c.mu.Lock()
		e.CorrelationID = c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		c.sink(e)

	case "turn/diff/updated":
		var p struct {
			TurnID string `json:"turnId"`
			Diff   string `json:"diff"`
		}
		_ = json.Unmarshal(params, &p)
		e := runtimeEvent(model.ActorCodex, model.RuntimeDiffUpdated)
		e.TurnID, e.Text = p.TurnID, p.Diff
		c.mu.Lock()
		e.CorrelationID = c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		c.sink(e)

	case "item/plan/delta":
		var p struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}
		_ = json.Unmarshal(params, &p)
		e := runtimeEvent(model.ActorCodex, model.RuntimePlanUpdated)
		e.TurnID, e.ItemID, e.Text = p.TurnID, p.ItemID, p.Delta
		c.mu.Lock()
		e.CorrelationID = c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)

	case "turn/plan/updated":
		// Older app-server releases emitted a whole-plan notification. Keep this
		// compatibility path while preferring the current item/plan/delta stream.
		var p struct {
			TurnID string `json:"turnId"`
		}
		_ = json.Unmarshal(params, &p)
		e := runtimeEvent(model.ActorCodex, model.RuntimePlanUpdated)
		e.TurnID = p.TurnID
		c.mu.Lock()
		e.CorrelationID = c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)

	case "thread/tokenUsage/updated":
		e := runtimeEvent(model.ActorCodex, model.RuntimeUsageUpdated)
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)

	case "turn/completed":
		c.handleTurnCompleted(params)

	case "serverRequest/resolved":
		c.handleServerRequestResolved(params)

	case "error", "warning", "configWarning":
		kind := model.RuntimeLog
		if method == "error" {
			kind = model.RuntimeError
		}
		e := runtimeEvent(model.ActorCodex, kind)
		e.Name = method
		e.Data = append(json.RawMessage(nil), params...)
		var p struct {
			TurnID  string `json:"turnId"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(params, &p)
		e.TurnID = p.TurnID
		c.mu.Lock()
		e.CorrelationID = c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		e.Text = p.Message
		if e.Text == "" {
			e.Text = p.Error.Message
		}
		c.sink(e)

	default:
		if strings.HasPrefix(method, "thread/") || strings.HasPrefix(method, "serverRequest/") {
			return
		}
		e := runtimeEvent(model.ActorCodex, model.RuntimeLog)
		e.Name = method
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)
	}
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
		delete(c.approvals, id)
		break
	}
	c.mu.Unlock()
	if cleared != nil {
		e := runtimeEvent(model.ActorCodex, model.RuntimeApprovalResolved)
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

func (c *CodexAdapter) handleItem(method string, params json.RawMessage) {
	var p struct {
		TurnID string `json:"turnId"`
		Item   struct {
			ID               string          `json:"id"`
			ClientID         string          `json:"clientId"`
			Type             string          `json:"type"`
			Phase            string          `json:"phase"`
			Text             string          `json:"text"`
			Command          json.RawMessage `json:"command"`
			Cwd              string          `json:"cwd"`
			Status           string          `json:"status"`
			AggregatedOutput string          `json:"aggregatedOutput"`
			Changes          json.RawMessage `json:"changes"`
		} `json:"item"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.Item.Type == "userMessage" {
		// App-server echoes turn/start or turn/steer's optional
		// clientUserMessageId as userMessage.clientId. Use that documented
		// correlation surface to bind notifications that can race the RPC reply.
		if p.Item.ClientID != "" {
			c.mu.Lock()
			input, ok := c.wireInputs[p.Item.ClientID]
			c.mu.Unlock()
			if ok && c.bindTurnInput(p.TurnID, input) {
				c.emitInputProcessing(p.TurnID, input, "acknowledged by Codex app-server")
			}
		}
		// A userMessage is transport activity, not a tool invocation.
		return
	}
	kind := model.RuntimeToolStarted
	if method == "item/completed" {
		kind = model.RuntimeToolCompleted
	}
	if p.Item.Type == "agentMessage" && method == "item/completed" {
		c.mu.Lock()
		// Prefer the authoritative final_answer item. Older app-server versions
		// may omit phase, in which case the latest completed message is retained.
		if p.Item.Phase == "final_answer" || p.Item.Phase == "" || c.turnFinal[p.TurnID] == "" {
			c.turnFinal[p.TurnID] = p.Item.Text
		}
		c.mu.Unlock()
		return
	}
	e := runtimeEvent(model.ActorCodex, kind)
	e.TurnID = p.TurnID
	e.ItemID = p.Item.ID
	e.Name = p.Item.Type
	c.mu.Lock()
	e.CorrelationID = c.latestTurnInputLocked(p.TurnID).MessageID
	c.mu.Unlock()
	e.Data = append(json.RawMessage(nil), params...)
	c.sink(e)
}

func (c *CodexAdapter) handleTurnCompleted(params json.RawMessage) {
	var p struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	c.mu.Lock()
	inputs := append([]model.AgentInput(nil), c.turnInputs[p.Turn.ID]...)
	input := model.AgentInput{}
	if len(inputs) > 0 {
		input = inputs[len(inputs)-1]
	}
	text := c.turnFinal[p.Turn.ID]
	if text == "" && c.turnBuffers[p.Turn.ID] != nil {
		text = c.turnBuffers[p.Turn.ID].String()
	}
	delete(c.turnInputs, p.Turn.ID)
	delete(c.turnFinal, p.Turn.ID)
	delete(c.turnBuffers, p.Turn.ID)
	if c.currentTurn == p.Turn.ID {
		c.currentTurn = ""
	}
	c.mu.Unlock()
	terminalKind := model.RuntimeInputFailed
	detail := p.Turn.Status
	switch strings.ToLower(p.Turn.Status) {
	case "completed", "success":
		terminalKind = model.RuntimeInputCompleted
	case "interrupted", "cancelled", "canceled", "aborted":
		terminalKind = model.RuntimeInputCancelled
	}
	if p.Turn.Error != nil && p.Turn.Error.Message != "" {
		detail = p.Turn.Error.Message
	}
	for _, item := range inputs {
		c.emitInputTerminal(p.Turn.ID, item, terminalKind, detail)
	}
	if terminalKind == model.RuntimeInputCompleted && strings.TrimSpace(text) != "" {
		e := runtimeEvent(model.ActorCodex, model.RuntimeFinal)
		e.TurnID = p.Turn.ID
		e.CorrelationID = input.MessageID
		e.Text = text
		c.sink(e)
	}
	completed := runtimeEvent(model.ActorCodex, model.RuntimeTurnCompleted)
	completed.TurnID = p.Turn.ID
	completed.CorrelationID = input.MessageID
	completed.Name = p.Turn.Status
	completed.Data = append(json.RawMessage(nil), params...)
	c.sink(completed)
	if p.Turn.Error != nil && p.Turn.Error.Message != "" {
		e := runtimeEvent(model.ActorCodex, model.RuntimeError)
		e.TurnID = p.Turn.ID
		e.CorrelationID = input.MessageID
		e.Text = p.Turn.Error.Message
		c.sink(e)
		c.setState(model.StateError, p.Turn.Error.Message)
		c.failQueued("previous Codex turn failed: " + p.Turn.Error.Message)
		return
	}
	if terminalKind == model.RuntimeInputFailed {
		c.setState(model.StateError, detail)
		c.failQueued("previous Codex turn ended with status " + detail)
		return
	} else {
		c.setState(model.StateIdle, "")
	}
	go c.tryStartQueued()
}

func (c *CodexAdapter) tryStartQueued() {
	// Let the terminal turn/completed state settle before attempting a queued
	// turn. Multiple callers are harmless: the lock pops at most one item.
	time.Sleep(25 * time.Millisecond)
	c.mu.Lock()
	if c.state == model.StateWorking || c.currentTurn != "" || len(c.queued) == 0 {
		c.mu.Unlock()
		return
	}
	input := c.queued[0]
	c.queued = c.queued[1:]
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	state, err := c.Submit(ctx, input)
	if err != nil {
		c.emitInputTerminal("", input, model.RuntimeInputFailed, "start queued Codex turn: "+err.Error())
		e := runtimeEvent(model.ActorCodex, model.RuntimeError)
		e.CorrelationID = input.MessageID
		e.Text = "start queued Codex turn: " + err.Error()
		c.sink(e)
		c.setState(model.StateError, e.Text)
		return
	}
	e := runtimeEvent(model.ActorCodex, model.RuntimeLog)
	e.Name = "queued.started"
	e.CorrelationID = input.MessageID
	e.Text = string(state)
	c.sink(e)
}

func (c *CodexAdapter) failQueued(detail string) {
	c.mu.Lock()
	queued := append([]model.AgentInput(nil), c.queued...)
	c.queued = nil
	c.mu.Unlock()
	for _, input := range queued {
		c.emitInputTerminal("", input, model.RuntimeInputFailed, detail)
	}
}

func (c *CodexAdapter) takeOutstandingInputs() []model.AgentInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := make(map[string]struct{})
	var inputs []model.AgentInput
	add := func(input model.AgentInput) {
		if input.MessageID == "" {
			return
		}
		if _, exists := seen[input.MessageID]; exists {
			return
		}
		seen[input.MessageID] = struct{}{}
		inputs = append(inputs, input)
	}
	for _, values := range c.turnInputs {
		for _, input := range values {
			add(input)
		}
	}
	if c.startingInput != nil {
		add(*c.startingInput)
	}
	for _, input := range c.wireInputs {
		add(input)
	}
	for _, input := range c.queued {
		add(input)
	}
	c.turnInputs = make(map[string][]model.AgentInput)
	c.wireInputs = make(map[string]model.AgentInput)
	c.startingInput = nil
	c.queued = nil
	c.turnBuffers = make(map[string]*strings.Builder)
	c.turnFinal = make(map[string]string)
	c.currentTurn = ""
	return inputs
}

func (c *CodexAdapter) sendRawResponse(id json.RawMessage, result any, rpcErr *codexRPCError) error {
	message := struct {
		ID     json.RawMessage `json:"id"`
		Result any             `json:"result,omitempty"`
		Error  *codexRPCError  `json:"error,omitempty"`
	}{ID: id, Result: result, Error: rpcErr}
	return c.send(message)
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
	c.mu.Unlock()
	// The room engine owns the user-facing approval projection after this call
	// succeeds. serverRequest/resolved remains available for server-side clears.
	c.setState(model.StateWorking, "")
	return nil
}

// Codex receives sandbox policy per turn, so a role change does not require an
// app-server restart. It must still happen at a safe turn boundary: already
// queued or in-flight inputs retain the role/policy captured when they were
// created and must not be relabelled midway through execution.
func (c *CodexAdapter) SetRole(_ context.Context, role model.ParticipantRole) error {
	if !role.Valid() {
		return fmt.Errorf("invalid Codex role %q", role)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == model.StateStarting || c.state == model.StateWorking || c.state == model.StateWaiting ||
		c.currentTurn != "" || c.startingInput != nil || len(c.wireInputs) > 0 || len(c.queued) > 0 || len(c.approvals) > 0 {
		return errors.New("interrupt or stop Codex before changing its role")
	}
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

func (c *CodexAdapter) Interrupt(ctx context.Context) error {
	c.mu.Lock()
	threadID, turnID := c.threadID, c.currentTurn
	c.mu.Unlock()
	if threadID == "" || turnID == "" {
		return nil
	}
	_, err := c.call(ctx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
	return err
}

func (c *CodexAdapter) failPendingRPCs(detail string) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan rpcReply)
	c.approvals = make(map[string]pendingApproval)
	c.mu.Unlock()

	err := errors.New(detail)
	for _, ch := range pending {
		select {
		case ch <- rpcReply{err: err}:
		default:
		}
	}
}

func (c *CodexAdapter) Stop(context.Context) error {
	c.mu.Lock()
	cmd := c.cmd
	stdin := c.stdin
	c.intentional = true
	c.cmd = nil
	c.stdin = nil
	c.mu.Unlock()
	c.failPendingRPCs("Codex was stopped")
	for _, input := range c.takeOutstandingInputs() {
		c.emitInputTerminal("", input, model.RuntimeInputCancelled, "Codex was stopped")
	}
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

func (c *CodexAdapter) waitProcess(cmd *exec.Cmd) {
	err := cmd.Wait()
	c.mu.Lock()
	active := c.cmd == cmd
	intentional := c.intentional
	var pending map[int64]chan rpcReply
	if active {
		c.cmd = nil
		c.stdin = nil
		pending = c.pending
		c.pending = make(map[int64]chan rpcReply)
		c.approvals = make(map[string]pendingApproval)
	}
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- rpcReply{err: errors.New("codex app-server exited")}:
		default:
		}
	}
	if !active {
		return
	}
	if intentional {
		c.setState(model.StateStopped, "")
		return
	}
	detail := "Codex app-server exited"
	if err != nil {
		detail += ": " + err.Error()
	}
	for _, input := range c.takeOutstandingInputs() {
		c.emitInputTerminal("", input, model.RuntimeInputFailed, detail)
	}
	if err != nil {
		e := runtimeEvent(model.ActorCodex, model.RuntimeError)
		e.Text = detail
		c.sink(e)
		c.setState(model.StateError, err.Error())
		return
	}
	c.setState(model.StateStopped, "")
}

// ParseCodexRequestID is kept small and exported only for protocol tests.
func ParseCodexRequestID(raw json.RawMessage) (int64, error) {
	var id int64
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}
