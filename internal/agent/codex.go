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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sean2077/pairroom/internal/execx"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
	"github.com/sean2077/pairroom/internal/version"
)

type rpcReply struct {
	result json.RawMessage
	err    error
}

type pendingApproval struct {
	rawID         json.RawMessage
	method        string
	params        json.RawMessage
	approval      model.Approval
	turnID        string
	correlationID string
}

// codexTurnTerminal remembers a completion that raced the turn/start or
// turn/steer response. App Server notifications and RPC responses are
// independent JSON-RPC messages, so the completion can legitimately arrive
// first. Keeping the accepted input IDs lets the late response acknowledge
// the already-settled message without resurrecting the native turn.
type codexTurnTerminal struct {
	status   string
	inputIDs map[string]struct{}
}

type CodexAdapter struct {
	cfg  Config
	sink EventSink

	startMu     sync.Mutex
	submitMu    sync.Mutex
	mu          sync.Mutex
	writeMu     sync.Mutex
	state       model.AgentState
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	threadID    string
	currentTurn string
	// threadEngaged records whether any turn has started on threadID. Codex
	// only persists a rollout once a turn is accepted, so a thread/start that
	// never starts a turn has no durable rollout. It is consulted on process
	// exit to decide whether the in-memory thread ID is safe to drop.
	threadEngaged bool
	intentional   bool
	pending       map[int64]chan rpcReply
	approvals     map[string]pendingApproval
	turnInputs    map[string][]model.AgentInput
	// wireInputs holds inputs keyed by Codex's documented
	// clientUserMessageId while a turn/start or turn/steer request is in flight.
	// The matching userMessage item echoes this value as clientId, allowing
	// notifications that arrive before the RPC response to retain exact room
	// message correlation.
	wireInputs     map[string]model.AgentInput
	wireInputOrder []string
	startingInput  *model.AgentInput
	startingTurnID string
	turnBuffers    map[string]*strings.Builder
	turnFinal      map[string]string
	terminalTurns  map[string]codexTurnTerminal
	startedTurns   map[string]struct{}
	// pendingCompletions holds a terminal notification that arrived before the
	// turn/start response exposed its ID. It is keyed by the opaque native turn
	// ID and consumed only when that exact response arrives; unrelated stale
	// completions never manufacture a Room boundary.
	pendingCompletions map[string]json.RawMessage
	nextRequestID      atomic.Int64
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
	if !cfg.Actor.ValidParticipant() {
		cfg.Actor = model.ActorCodex
	}
	if cfg.Command == "" {
		cfg.Command = "codex"
	}
	cfg.ApprovalPolicy = normalizeCodexApprovalPolicy(cfg.ApprovalPolicy)
	adapter := &CodexAdapter{
		cfg: cfg, sink: sink, state: model.StateStopped, threadID: cfg.SessionID,
		pending: make(map[int64]chan rpcReply), approvals: make(map[string]pendingApproval),
		turnInputs:    make(map[string][]model.AgentInput),
		wireInputs:    make(map[string]model.AgentInput),
		terminalTurns: make(map[string]codexTurnTerminal), startedTurns: make(map[string]struct{}), pendingCompletions: make(map[string]json.RawMessage),
		turnBuffers: make(map[string]*strings.Builder),
		turnFinal:   make(map[string]string),
	}
	adapter.nextRequestID.Store(100)
	return adapter
}

func (c *CodexAdapter) Actor() model.ActorID { return c.cfg.Actor }

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
	e := runtimeEvent(c.cfg.Actor, model.RuntimeState)
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
		Actor: c.cfg.Actor, Command: c.cfg.Command, Model: c.cfg.Model,
		Runtime: c.cfg.Runtime, ApprovalPolicy: c.cfg.ApprovalPolicy, Sandbox: c.cfg.Sandbox,
	})
	if probeErr != nil {
		info := model.RuntimeInfo{
			Available: false, Command: c.cfg.Command, Protocol: "codex-app-server-jsonrpc",
			Model: c.cfg.Model, ApprovalPolicy: c.cfg.ApprovalPolicy, Sandbox: c.cfg.Sandbox,
			Warnings: []string{probeErr.Error()}, ProbedAt: time.Now().UTC(),
		}
		emitRuntimeInfo(c.sink, c.cfg.Actor, info)
		c.setState(model.StateError, probeErr.Error())
		return probeErr
	} else {
		emitRuntimeInfo(c.sink, c.cfg.Actor, probe.RuntimeInfo(c.cfg))
	}

	args := append([]string(nil), c.cfg.CommandArgs...)
	args = append(args, "app-server")
	cmd := exec.Command(c.cfg.Command, args...)
	execx.NoConsole(cmd)
	cmd.Dir = c.cfg.Repo
	cmd.Env = mergeRuntimeEnv(envWithout("CODEX_INTERNAL_ORIGINATOR"), c.cfg.Env)
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
	requiredThread := existingThread
	strictResume := c.cfg.RequireExactSession && requiredThread != ""
	var result json.RawMessage
	if existingThread != "" {
		result, err = c.call(handshakeCtx, "thread/resume", c.threadResumeParams(existingThread))
		if err != nil {
			if strictResume {
				_ = c.Stop(context.Background())
				return fmt.Errorf("resume required Codex thread %q: %w", requiredThread, err)
			}
			logEvent := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
			logEvent.Name = "thread.resume"
			logEvent.Text = "resume failed; creating a new Codex thread: " + err.Error()
			c.sink(logEvent)
			existingThread = ""
		}
	}
	if existingThread == "" {
		result, err = c.call(handshakeCtx, "thread/start", c.threadStartParams())
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
	if strictResume && threadResult.Thread.ID != requiredThread {
		_ = c.Stop(context.Background())
		return fmt.Errorf("Codex resumed thread %q instead of required thread %q", threadResult.Thread.ID, requiredThread)
	}
	c.mu.Lock()
	c.threadID = threadResult.Thread.ID
	c.mu.Unlock()
	c.setState(model.StateIdle, "")
	session := runtimeEvent(c.cfg.Actor, model.RuntimeSession)
	session.SessionID = threadResult.Thread.ID
	c.sink(session)
	return nil
}

func (c *CodexAdapter) StartTurn(ctx context.Context, input model.AgentInput) error {
	// A Codex thread accepts one active turn. PairRoom reserves the Room owner;
	// this lock closes the smaller native start/steer observation race.
	c.submitMu.Lock()
	defer c.submitMu.Unlock()

	if err := c.Start(ctx); err != nil {
		return err
	}

	text := prompt.Envelope(input)
	c.mu.Lock()
	threadID := c.threadID
	turnID := c.currentTurn
	active := turnID != "" && (c.state == model.StateWorking || c.state == model.StateWaiting)
	c.mu.Unlock()

	if active {
		return errors.New("Codex already has an active turn")
	}

	params := c.turnStartParams(threadID, text, input)
	// turn/started can arrive before the turn/start response. Keep the input in
	// a temporary slot so either ordering receives the correct correlation ID.
	starting := input
	c.mu.Lock()
	c.startingInput = &starting
	c.startingTurnID = ""
	c.mu.Unlock()
	c.stageWireInput(input)
	result, err := c.call(ctx, "turn/start", params)
	c.unstageWireInput(input.MessageID)
	if err != nil {
		c.mu.Lock()
		c.startingInput = nil
		c.startingTurnID = ""
		c.pendingCompletions = make(map[string]json.RawMessage)
		c.mu.Unlock()
		return err
	}
	var turnResult struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &turnResult); err != nil || turnResult.Turn.ID == "" {
		c.mu.Lock()
		c.startingInput = nil
		c.startingTurnID = ""
		c.pendingCompletions = make(map[string]json.RawMessage)
		c.mu.Unlock()
		if err == nil {
			err = errors.New("missing turn id")
		}
		return fmt.Errorf("decode codex turn: %w", err)
	}
	c.mu.Lock()
	if _, completed := c.terminalTurns[turnResult.Turn.ID]; completed {
		c.startingInput = nil
		c.startingTurnID = ""
		c.mu.Unlock()
		// The native completion already emitted input terminal events and the
		// Room boundary. Do not recreate currentTurn or emit a second start.
		return nil
	}
	pendingCompletion := append(json.RawMessage(nil), c.pendingCompletions[turnResult.Turn.ID]...)
	// There is only one turn/start request in flight under submitMu. Any other
	// completion held while its opaque ID was unknown is therefore stale (or
	// belongs to a native turn PairRoom never owned); discard it at this exact
	// response boundary instead of retaining unbounded connection-local state.
	c.pendingCompletions = make(map[string]json.RawMessage)
	c.currentTurn = turnResult.Turn.ID
	c.threadEngaged = true
	_, startedNotificationSeen := c.startedTurns[turnResult.Turn.ID]
	if !startedNotificationSeen {
		c.startedTurns[turnResult.Turn.ID] = struct{}{}
	}
	if c.turnBuffers[turnResult.Turn.ID] == nil {
		c.turnBuffers[turnResult.Turn.ID] = &strings.Builder{}
	}
	c.mu.Unlock()
	if len(pendingCompletion) > 0 {
		// The terminal notification won the wire race. Let the normal completion
		// path settle the staged input and emit exactly one boundary. If no native
		// turn/started notification arrived, synthesize the lifecycle start before
		// the terminal event so observers never see a completion without a start.
		if !startedNotificationSeen {
			c.setState(model.StateWorking, "")
			started := runtimeEvent(c.cfg.Actor, model.RuntimeTurnStarted)
			started.TurnID = turnResult.Turn.ID
			started.CorrelationID = input.MessageID
			c.sink(started)
		}
		c.mu.Lock()
		// Preserve the staged input for handleTurnCompleted; call() already
		// removed its wire correlation before decoding the turn/start response.
		c.startingInput = &starting
		c.startingTurnID = turnResult.Turn.ID
		c.mu.Unlock()
		c.handleTurnCompleted(pendingCompletion)
		return nil
	}
	c.mu.Lock()
	c.startingInput = nil
	c.startingTurnID = ""
	c.mu.Unlock()
	if !startedNotificationSeen {
		started := runtimeEvent(c.cfg.Actor, model.RuntimeTurnStarted)
		started.TurnID = turnResult.Turn.ID
		started.CorrelationID = input.MessageID
		c.sink(started)
	}
	if c.bindTurnInput(turnResult.Turn.ID, input) {
		c.emitInputProcessing(turnResult.Turn.ID, input, "started Codex turn")
	}
	c.setState(model.StateWorking, "")
	return nil
}

func (c *CodexAdapter) Steer(ctx context.Context, input model.AgentInput) SteerOutcome {
	c.submitMu.Lock()
	defer c.submitMu.Unlock()

	c.mu.Lock()
	threadID := c.threadID
	turnID := c.currentTurn
	active := turnID != "" && (c.state == model.StateWorking || c.state == model.StateWaiting)
	c.mu.Unlock()
	if !active {
		return SteerOutcome{State: SteerUnavailable, Detail: "Codex has no active turn"}
	}

	text := prompt.Envelope(input)
	c.stageWireInput(input)
	defer c.unstageWireInput(input.MessageID)
	result, err := c.call(ctx, "turn/steer", codexTurnSteerParams(threadID, turnID, text, input))
	if err != nil {
		var rpcErr codexRPCError
		if errors.As(err, &rpcErr) {
			if rpcErr.Code == -32601 {
				return SteerOutcome{State: SteerUnavailable, Detail: err.Error()}
			}
			return SteerOutcome{State: SteerRejected, Detail: err.Error()}
		}
		return SteerOutcome{State: SteerUnknown, Detail: err.Error()}
	}
	var response struct {
		TurnID string `json:"turnId"`
	}
	if err := json.Unmarshal(result, &response); err != nil || response.TurnID != turnID {
		if err == nil {
			err = fmt.Errorf("returned turn %q instead of %q", response.TurnID, turnID)
		}
		return SteerOutcome{State: SteerUnknown, Detail: "decode Codex turn/steer acknowledgement: " + err.Error()}
	}
	c.mu.Lock()
	terminal, completed := c.terminalTurns[turnID]
	current := c.currentTurn
	c.mu.Unlock()
	if completed {
		if _, accepted := terminal.inputIDs[input.MessageID]; accepted {
			// The completion path included this staged input and already emitted
			// its terminal event. The RPC response arrived late; report accepted
			// without resurrecting the turn or duplicating lifecycle events.
			return SteerOutcome{State: SteerAccepted, Detail: "accepted by Codex turn/steer (turn completed before acknowledgement)"}
		}
		return SteerOutcome{State: SteerUnknown, Detail: "Codex turn completed before the steered input was correlated; explicit retry required"}
	}
	if current != turnID {
		return SteerOutcome{State: SteerUnknown, Detail: "Codex active turn ended before steer acknowledgement; explicit retry required"}
	}
	if c.bindTurnInput(turnID, input) {
		c.emitInputProcessing(turnID, input, "injected into active Codex turn")
	}
	return SteerOutcome{State: SteerAccepted, Detail: "accepted by Codex turn/steer"}
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

func normalizeCodexApprovalPolicy(value string) string {
	if value == "unlessTrusted" {
		return "untrusted"
	}
	return value
}

func (c *CodexAdapter) developerInstructions() string {
	return collaborationPrompt(c.cfg)
}

func (c *CodexAdapter) threadResumeParams(threadID string) map[string]any {
	return map[string]any{
		"threadId":              threadID,
		"cwd":                   c.cfg.Repo,
		"developerInstructions": c.developerInstructions(),
	}
}

func (c *CodexAdapter) threadStartParams() map[string]any {
	params := map[string]any{
		"cwd":                   c.cfg.Repo,
		"serviceName":           "pairroom",
		"developerInstructions": c.developerInstructions(),
	}
	if c.cfg.ApprovalPolicy != "" {
		params["approvalPolicy"] = c.cfg.ApprovalPolicy
	}
	if c.cfg.Sandbox != "" {
		params["sandbox"] = c.legacySandbox()
	}
	if c.cfg.Model != "" {
		params["model"] = c.cfg.Model
	}
	return params
}

func (c *CodexAdapter) turnStartParams(threadID, text string, input model.AgentInput) map[string]any {
	params := map[string]any{
		"threadId": threadID,
		"input":    codexInputItems(text, input.Attachments),
		"cwd":      c.cfg.Repo,
	}
	if c.cfg.ApprovalPolicy != "" {
		params["approvalPolicy"] = c.cfg.ApprovalPolicy
	}
	effectiveRole := input.Role
	if input.Role == model.RoleReviewer && c.cfg.OrdinaryReviewerPolicy == model.ReviewerExplicit {
		// The Room keeps Reviewer as the durable role and workspace boundary,
		// while the explicit policy opts this ordinary Reviewer turn into the
		// selected native permission/sandbox profile.
		effectiveRole = model.RoleDriver
	}
	if input.Role == model.RoleReviewer && c.cfg.OrdinaryReviewerPolicy != model.ReviewerExplicit {
		params["sandboxPolicy"] = map[string]any{"type": "readOnly"}
	} else if c.cfg.Sandbox != "" {
		params["sandboxPolicy"] = c.sandboxPolicy(effectiveRole)
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
	// The legacy thread/start sandbox enum uses CLI-style kebab-case, unlike
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
	if _, exists := c.wireInputs[input.MessageID]; !exists {
		c.wireInputOrder = append(c.wireInputOrder, input.MessageID)
	}
	c.wireInputs[input.MessageID] = input
	c.mu.Unlock()
}

func (c *CodexAdapter) unstageWireInput(messageID string) {
	if messageID == "" {
		return
	}
	c.mu.Lock()
	delete(c.wireInputs, messageID)
	for index, value := range c.wireInputOrder {
		if value != messageID {
			continue
		}
		copy(c.wireInputOrder[index:], c.wireInputOrder[index+1:])
		c.wireInputOrder = c.wireInputOrder[:len(c.wireInputOrder)-1]
		break
	}
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

// knownTurnLocked is the transcript boundary for turn-scoped App Server
// notifications. A resumed Codex thread can replay events for native work
// that PairRoom did not submit; those events must remain diagnostics and must
// never become the current Room turn or acquire a message correlation.
// c.mu must be held by the caller.
func (c *CodexAdapter) knownTurnLocked(turnID string) bool {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return false
	}
	if c.currentTurn == turnID || c.startingTurnID == turnID {
		return true
	}
	if _, ok := c.turnInputs[turnID]; ok {
		return true
	}
	if _, ok := c.startedTurns[turnID]; ok {
		return true
	}
	return false
}

func (c *CodexAdapter) emitInputProcessing(turnID string, input model.AgentInput, detail string) {
	e := runtimeEvent(c.cfg.Actor, model.RuntimeInputProcessing)
	e.TurnID = turnID
	e.CorrelationID = input.MessageID
	e.Name = string(model.ProcessingWorking)
	e.Text = detail
	c.sink(e)
}

func (c *CodexAdapter) emitInputTerminal(turnID string, input model.AgentInput, kind, detail string) {
	e := runtimeEvent(c.cfg.Actor, kind)
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
	emitRuntimeInfo(c.sink, c.cfg.Actor, info)
}

func (c *CodexAdapter) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		result, err := c.callOnce(ctx, method, params)
		var rpcErr codexRPCError
		if err == nil || !errors.As(err, &rpcErr) || rpcErr.Code != -32001 || attempt >= 4 {
			return result, err
		}
		delay := time.Duration(100*(1<<attempt))*time.Millisecond + time.Duration(time.Now().UnixNano()%75)*time.Millisecond
		logEvent := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
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
		e := runtimeEvent(c.cfg.Actor, model.RuntimeError)
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
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
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
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
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

func (c *CodexAdapter) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &p)
		if strings.TrimSpace(p.Turn.ID) == "" {
			return
		}
		newlyBound := false
		emitStarted := false
		c.mu.Lock()
		if _, completed := c.terminalTurns[p.Turn.ID]; completed {
			// A late notification for a turn whose completion already won the
			// race must not resurrect currentTurn or reopen its input lifecycle.
			c.mu.Unlock()
			return
		}
		// A notification for an unrelated/resumed native turn must never take
		// ownership of the Room. Only the turn/start request currently staged by
		// PairRoom, or the already-recorded current turn, is admissible.
		if c.currentTurn != "" && c.currentTurn != p.Turn.ID {
			c.mu.Unlock()
			return
		}
		if c.startingTurnID != "" && c.startingTurnID != p.Turn.ID {
			c.mu.Unlock()
			return
		}
		if c.currentTurn == "" && c.startingInput == nil {
			c.mu.Unlock()
			return
		}
		if _, already := c.startedTurns[p.Turn.ID]; !already {
			c.startedTurns[p.Turn.ID] = struct{}{}
			emitStarted = true
		}
		c.currentTurn = p.Turn.ID
		c.threadEngaged = true
		if c.startingInput != nil && c.startingTurnID == "" {
			c.startingTurnID = p.Turn.ID
		}
		if c.turnBuffers[p.Turn.ID] == nil {
			c.turnBuffers[p.Turn.ID] = &strings.Builder{}
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
		if emitStarted {
			e := runtimeEvent(c.cfg.Actor, model.RuntimeTurnStarted)
			e.TurnID = p.Turn.ID
			e.CorrelationID = input.MessageID
			c.sink(e)
		}
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
		if strings.TrimSpace(p.TurnID) == "" {
			return
		}
		c.mu.Lock()
		if !c.knownTurnLocked(p.TurnID) {
			c.mu.Unlock()
			return
		}
		builder := c.turnBuffers[p.TurnID]
		if builder == nil {
			builder = &strings.Builder{}
			c.turnBuffers[p.TurnID] = builder
		}
		builder.WriteString(p.Delta)
		input := c.latestTurnInputLocked(p.TurnID)
		c.mu.Unlock()
		e := runtimeEvent(c.cfg.Actor, model.RuntimeTextDelta)
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
		if strings.TrimSpace(p.TurnID) == "" {
			return
		}
		c.mu.Lock()
		known := c.knownTurnLocked(p.TurnID)
		correlationID := c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		if !known {
			return
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimeCommandOutput)
		e.TurnID, e.ItemID, e.Text = p.TurnID, p.ItemID, p.Delta
		e.CorrelationID = correlationID
		c.sink(e)

	case "turn/diff/updated":
		var p struct {
			TurnID string `json:"turnId"`
			Diff   string `json:"diff"`
		}
		_ = json.Unmarshal(params, &p)
		if strings.TrimSpace(p.TurnID) == "" {
			return
		}
		c.mu.Lock()
		known := c.knownTurnLocked(p.TurnID)
		correlationID := c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		if !known {
			return
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimeDiffUpdated)
		e.TurnID, e.Text = p.TurnID, p.Diff
		e.CorrelationID = correlationID
		c.sink(e)

	case "item/plan/delta":
		var p struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}
		_ = json.Unmarshal(params, &p)
		if strings.TrimSpace(p.TurnID) == "" {
			return
		}
		c.mu.Lock()
		known := c.knownTurnLocked(p.TurnID)
		correlationID := c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		if !known {
			return
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimePlanUpdated)
		e.TurnID, e.ItemID, e.Text = p.TurnID, p.ItemID, p.Delta
		e.CorrelationID = correlationID
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)

	case "turn/plan/updated":
		// Older app-server releases emitted a whole-plan notification. Keep this
		// compatibility path while preferring the current item/plan/delta stream.
		var p struct {
			TurnID string `json:"turnId"`
		}
		_ = json.Unmarshal(params, &p)
		if strings.TrimSpace(p.TurnID) == "" {
			return
		}
		c.mu.Lock()
		known := c.knownTurnLocked(p.TurnID)
		correlationID := c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		if !known {
			return
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimePlanUpdated)
		e.TurnID = p.TurnID
		e.CorrelationID = correlationID
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)

	case "thread/tokenUsage/updated":
		e := runtimeEvent(c.cfg.Actor, model.RuntimeUsageUpdated)
		c.mu.Lock()
		e.TurnID = c.currentTurn
		e.CorrelationID = c.latestTurnInputLocked(c.currentTurn).MessageID
		if e.CorrelationID == "" && c.startingInput != nil {
			e.CorrelationID = c.startingInput.MessageID
		}
		c.mu.Unlock()
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)

	case "turn/completed":
		c.handleTurnCompleted(params)

	case "serverRequest/resolved":
		c.handleServerRequestResolved(params)

	case "error", "warning", "configWarning":
		// App Server `error` notifications are diagnostics and may arrive while
		// the native Turn continues. Preserve their error classification for the
		// inspector, but Room scheduling must not treat RuntimeError as a terminal
		// boundary; only turn/completed, confirmed process exit, or explicit abort
		// and stop signals may release ownership.
		kind := model.RuntimeLog
		if method == "error" {
			kind = model.RuntimeError
		}
		e := runtimeEvent(c.cfg.Actor, kind)
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
		c.mu.Lock()
		turnID := p.TurnID
		if strings.TrimSpace(turnID) != "" && !c.knownTurnLocked(turnID) {
			c.mu.Unlock()
			return
		}
		if turnID == "" && method == "error" {
			turnID = c.currentTurn
		}
		e.TurnID = turnID
		e.CorrelationID = c.latestTurnInputLocked(turnID).MessageID
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
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
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
	if strings.TrimSpace(p.TurnID) == "" {
		return
	}
	if p.Item.Type == "userMessage" {
		// App-server echoes turn/start or turn/steer's optional
		// clientUserMessageId as userMessage.clientId. Use that documented
		// correlation surface to bind notifications that can race the RPC reply.
		if p.Item.ClientID != "" {
			c.mu.Lock()
			if _, completed := c.terminalTurns[p.TurnID]; completed {
				c.mu.Unlock()
				return
			}
			input, ok := c.wireInputs[p.Item.ClientID]
			c.mu.Unlock()
			if ok && c.bindTurnInput(p.TurnID, input) {
				c.emitInputProcessing(p.TurnID, input, "acknowledged by Codex app-server")
			}
		}
		// A userMessage is transport activity, not a tool invocation.
		return
	}
	c.mu.Lock()
	known := c.knownTurnLocked(p.TurnID)
	correlationID := c.latestTurnInputLocked(p.TurnID).MessageID
	c.mu.Unlock()
	if !known {
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
	e := runtimeEvent(c.cfg.Actor, kind)
	e.TurnID = p.TurnID
	e.ItemID = p.Item.ID
	e.Name = p.Item.Type
	e.CorrelationID = correlationID
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
			Items []struct {
				Type   string `json:"type"`
				Phase  string `json:"phase"`
				Text   string `json:"text"`
				Review string `json:"review"`
			} `json:"items"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if strings.TrimSpace(p.Turn.ID) == "" {
		return
	}
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
	c.mu.Lock()
	wasCurrent := c.currentTurn == p.Turn.ID
	inputs := append([]model.AgentInput(nil), c.turnInputs[p.Turn.ID]...)
	if _, completed := c.terminalTurns[p.Turn.ID]; completed {
		c.mu.Unlock()
		return
	}
	knownTurn := wasCurrent || len(inputs) > 0
	if !knownTurn && c.startingInput != nil {
		knownTurn = c.startingTurnID == "" || c.startingTurnID == p.Turn.ID
	}
	if !knownTurn {
		if c.startingInput != nil && c.startingTurnID == "" {
			// We cannot correlate an arbitrary completion to the in-flight
			// turn/start until its response reveals the ID. Hold it briefly and let
			// StartTurn consume only an exact ID match.
			if c.pendingCompletions == nil {
				c.pendingCompletions = make(map[string]json.RawMessage)
			}
			c.pendingCompletions[p.Turn.ID] = append(json.RawMessage(nil), params...)
		}
		// A connection can receive a late notification for a turn that this
		// adapter never owned (for example, a stale subscription event). Do not
		// manufacture a Room boundary or release another active owner for it.
		c.mu.Unlock()
		return
	}
	seenInputs := make(map[string]struct{}, len(inputs))
	for _, value := range inputs {
		if value.MessageID != "" {
			seenInputs[value.MessageID] = struct{}{}
		}
	}
	addInput := func(value model.AgentInput) {
		if value.MessageID == "" {
			return
		}
		if _, exists := seenInputs[value.MessageID]; exists {
			return
		}
		seenInputs[value.MessageID] = struct{}{}
		inputs = append(inputs, value)
	}
	// A completion can overtake the turn/start response. Include the input that
	// is still staged for that request, plus any turn/steer input whose
	// userMessage echo has not arrived yet, so every accepted message is
	// settled exactly once.
	if c.currentTurn == p.Turn.ID || (c.currentTurn == "" && c.startingInput != nil && (c.startingTurnID == "" || c.startingTurnID == p.Turn.ID)) {
		if c.startingInput != nil {
			addInput(*c.startingInput)
		}
		for _, messageID := range c.wireInputOrder {
			if value, ok := c.wireInputs[messageID]; ok {
				addInput(value)
			}
		}
	}
	input := model.AgentInput{}
	if len(inputs) > 0 {
		input = inputs[len(inputs)-1]
	}
	text := c.turnFinal[p.Turn.ID]
	if text == "" {
		// Current App Server v2 includes the final agentMessage in the completed
		// turn as a summary fallback when item notifications were suppressed or
		// raced the terminal event. Prefer that authoritative item over a partial
		// delta buffer; exitedReviewMode is the equivalent final text for an
		// inline review turn.
		for _, item := range p.Turn.Items {
			if item.Type == "agentMessage" && strings.TrimSpace(item.Text) != "" {
				if item.Phase == "final_answer" || text == "" {
					text = item.Text
				}
			}
			if item.Type == "exitedReviewMode" && strings.TrimSpace(item.Review) != "" {
				text = item.Review
			}
		}
	}
	if text == "" && c.turnBuffers[p.Turn.ID] != nil {
		text = c.turnBuffers[p.Turn.ID].String()
	}
	delete(c.turnInputs, p.Turn.ID)
	delete(c.turnFinal, p.Turn.ID)
	delete(c.turnBuffers, p.Turn.ID)
	delete(c.startedTurns, p.Turn.ID)
	if wasCurrent {
		c.currentTurn = ""
	}
	if _, duplicate := c.terminalTurns[p.Turn.ID]; duplicate {
		c.mu.Unlock()
		return
	}
	inputIDs := make(map[string]struct{}, len(inputs))
	for _, value := range inputs {
		if value.MessageID != "" {
			inputIDs[value.MessageID] = struct{}{}
		}
	}
	c.terminalTurns[p.Turn.ID] = codexTurnTerminal{status: p.Turn.Status, inputIDs: inputIDs}
	if len(c.terminalTurns) > 256 {
		// Turn IDs are opaque and unique. Evicting an arbitrary old tombstone
		// only bounds memory; late responses are expected within the current
		// request lifetime and are consumed before this limit is reached.
		for turnID := range c.terminalTurns {
			if turnID != p.Turn.ID {
				delete(c.terminalTurns, turnID)
				break
			}
		}
	}
	if wasCurrent {
		c.startingInput = nil
		c.startingTurnID = ""
		c.wireInputs = make(map[string]model.AgentInput)
		c.wireInputOrder = nil
	}
	c.mu.Unlock()
	for _, item := range inputs {
		c.emitInputTerminal(p.Turn.ID, item, terminalKind, detail)
	}
	if terminalKind == model.RuntimeInputCompleted && strings.TrimSpace(text) != "" {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeFinal)
		e.TurnID = p.Turn.ID
		e.CorrelationID = input.MessageID
		e.Text = text
		c.sink(e)
	}
	completed := runtimeEvent(c.cfg.Actor, model.RuntimeTurnCompleted)
	completed.TurnID = p.Turn.ID
	completed.CorrelationID = input.MessageID
	completed.Name = p.Turn.Status
	completed.Data = append(json.RawMessage(nil), params...)
	c.sink(completed)
	if p.Turn.Error != nil && p.Turn.Error.Message != "" {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeError)
		e.TurnID = p.Turn.ID
		e.CorrelationID = input.MessageID
		e.Text = p.Turn.Error.Message
		c.sink(e)
		c.setState(model.StateError, p.Turn.Error.Message)
		return
	}
	if terminalKind == model.RuntimeInputFailed {
		c.setState(model.StateError, detail)
		return
	} else {
		c.setState(model.StateIdle, "")
	}
}

type codexOutstanding struct {
	inputs        []model.AgentInput
	inputTurns    map[string]string
	turnID        string
	correlationID string
}

func (c *CodexAdapter) takeOutstanding() codexOutstanding {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := codexOutstanding{turnID: c.currentTurn, inputTurns: make(map[string]string)}
	if input := c.latestTurnInputLocked(c.currentTurn); input.MessageID != "" {
		result.correlationID = input.MessageID
	} else if c.startingInput != nil {
		result.correlationID = c.startingInput.MessageID
	}
	seen := make(map[string]struct{})
	add := func(input model.AgentInput, turnID string) {
		if input.MessageID == "" {
			return
		}
		if _, exists := seen[input.MessageID]; exists {
			if result.inputTurns[input.MessageID] == "" && turnID != "" {
				result.inputTurns[input.MessageID] = turnID
			}
			return
		}
		seen[input.MessageID] = struct{}{}
		result.inputs = append(result.inputs, input)
		result.inputTurns[input.MessageID] = turnID
		if result.correlationID == "" {
			result.correlationID = input.MessageID
		}
	}
	for turnID, values := range c.turnInputs {
		for _, input := range values {
			add(input, turnID)
		}
	}
	if c.startingInput != nil {
		add(*c.startingInput, c.currentTurn)
	}
	for _, messageID := range c.wireInputOrder {
		if input, ok := c.wireInputs[messageID]; ok {
			add(input, c.currentTurn)
		}
	}
	c.turnInputs = make(map[string][]model.AgentInput)
	c.wireInputs = make(map[string]model.AgentInput)
	c.wireInputOrder = nil
	c.terminalTurns = make(map[string]codexTurnTerminal)
	c.startedTurns = make(map[string]struct{})
	c.pendingCompletions = make(map[string]json.RawMessage)
	c.startingInput = nil
	c.startingTurnID = ""
	c.turnBuffers = make(map[string]*strings.Builder)
	c.turnFinal = make(map[string]string)
	c.currentTurn = ""
	// thread/start creates a Codex thread in memory, but Codex only persists a
	// rollout once a turn is accepted. If the app-server process exits before
	// the first turn starts on a thread/start-only ID, that ID has no durable
	// rollout; strict-resuming it across a process restart hard-fails forever
	// ("no rollout found"). A pending new binding (no durable cfg.SessionID)
	// whose thread was never engaged has no identity to honor, so drop the
	// ephemeral ID and let the next Start create a fresh thread that the first
	// accepted turn atomically materializes. An existing/materialized binding
	// keeps its ID so the next Start resumes exactly; a missing rollout there
	// is a real binding inconsistency that must surface, not be replaced.
	if c.cfg.SessionID == "" && !c.threadEngaged {
		c.threadID = ""
	}
	return result
}

func (c *CodexAdapter) takeOutstandingInputs() []model.AgentInput {
	return c.takeOutstanding().inputs
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
		c.currentTurn != "" || c.startingInput != nil || len(c.wireInputs) > 0 || len(c.approvals) > 0 {
		return errors.New("interrupt or stop Codex before changing its role")
	}
	return nil
}

// SetWorkspace updates both the app-server process directory and the cwd sent
// to thread/turn requests. Like role changes, it is rejected while any input
// can still be in flight so a turn is never relabelled after it started.
func (c *CodexAdapter) SetWorkspace(ctx context.Context, workspace string) error {
	workspace = filepath.Clean(strings.TrimSpace(workspace))
	if workspace == "." || workspace == "" {
		return errors.New("Codex workspace is required")
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return fmt.Errorf("stat Codex workspace: %w", err)
	}
	if !info.IsDir() {
		return errors.New("Codex workspace is not a directory")
	}

	c.mu.Lock()
	if filepath.Clean(c.cfg.Repo) == workspace {
		c.mu.Unlock()
		return nil
	}
	if c.state == model.StateStarting || c.state == model.StateWorking || c.state == model.StateWaiting ||
		c.currentTurn != "" || c.startingInput != nil || len(c.wireInputs) > 0 || len(c.approvals) > 0 {
		c.mu.Unlock()
		return errors.New("interrupt or stop Codex before changing its workspace")
	}
	wasRunning := c.cmd != nil && c.cmd.Process != nil
	old := c.cfg.Repo
	c.cfg.Repo = workspace
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
		return fmt.Errorf("restart Codex in reviewer workspace: %w", err)
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
	c.handleUnexpectedProcessExit(err)
}

func (c *CodexAdapter) handleUnexpectedProcessExit(err error) {
	detail := "Codex app-server exited"
	if err != nil {
		detail += ": " + err.Error()
	}
	outstanding := c.takeOutstanding()
	for _, input := range outstanding.inputs {
		c.emitInputTerminal(outstanding.inputTurns[input.MessageID], input, model.RuntimeInputFailed, detail)
	}
	if outstanding.turnID != "" || len(outstanding.inputs) > 0 {
		completed := runtimeEvent(c.cfg.Actor, model.RuntimeTurnCompleted)
		completed.TurnID = outstanding.turnID
		completed.CorrelationID = outstanding.correlationID
		completed.Name = "process_exited"
		c.sink(completed)
	}
	if err != nil || outstanding.turnID != "" || len(outstanding.inputs) > 0 {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeError)
		e.Text = detail
		c.sink(e)
		c.setState(model.StateError, detail)
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
