package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sean2077/pairroom/internal/execx"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/version"
)

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

	startMu  sync.Mutex
	submitMu sync.Mutex
	mu       sync.Mutex
	writeMu  sync.Mutex
	state    model.AgentState
	cmd      *exec.Cmd
	tree     *execx.Tree
	stdin    io.WriteCloser
	// procDone is closed once the process exited and waitProcess finished; it
	// gives Stop a bounded graceful window between closing stdin and Kill.
	procDone    chan struct{}
	threadID    string
	currentTurn string
	// threadEngaged records whether any turn has started on threadID. Codex
	// only persists a rollout once a turn is accepted, so a thread/start that
	// never starts a turn has no durable rollout. It is consulted on process
	// exit to decide whether the in-memory thread ID is safe to drop.
	threadEngaged bool
	intentional   bool
	// streamFailure records why the adapter killed the process after its
	// stdout became unreadable; waitProcess reports it with the exit.
	streamFailure string
	// access records the last successfully asserted native access so the
	// per-submission same-access assertion is a no-op instead of re-running
	// the turn-boundary gate (and failing on stale state).
	access     model.NativeAccess
	pending    map[int64]chan rpcReply
	approvals  map[string]pendingApproval
	turnInputs map[string][]model.AgentInput
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

func NewCodex(cfg Config, sink EventSink) *CodexAdapter {
	if !cfg.Actor.ValidParticipant() {
		cfg.Actor = model.ActorSlot2
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
	c.streamFailure = ""
	c.mu.Unlock()

	probe, probeErr := ProbeRuntime(ctx, Config{
		Actor: c.cfg.Actor, Command: c.cfg.Command, Model: c.cfg.Model,
		Runtime: c.cfg.Runtime, ApprovalPolicy: c.cfg.ApprovalPolicy, Sandbox: c.cfg.Sandbox,
	})
	if probeErr != nil {
		info := model.RuntimeInfo{
			Available: false, Command: c.cfg.Command, Protocol: "codex-app-server-jsonrpc",
			RuntimeKind: c.cfg.Runtime.CanonicalForSlot(c.cfg.Actor),
			Provider:    c.cfg.Provider, ProviderName: c.cfg.ProviderName,
			Model: c.cfg.Model, Effort: c.cfg.Effort, ApprovalPolicy: c.cfg.ApprovalPolicy, Sandbox: c.cfg.Sandbox,
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
	if isBatchLauncher(goruntime.GOOS, probe.Path) {
		// CC Switch provider arguments and template args reach cmd.exe here.
		if err := checkBatchLauncherArgs("Codex", probe.Path, args); err != nil {
			c.setState(model.StateError, err.Error())
			return err
		}
	}
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
	tree, err := execx.StartTree(cmd)
	if err != nil {
		_ = stdin.Close()
		c.setState(model.StateError, err.Error())
		return fmt.Errorf("start codex app-server: %w", err)
	}

	procDone := make(chan struct{})
	c.mu.Lock()
	c.cmd = cmd
	c.tree = tree
	c.stdin = stdin
	c.procDone = procDone
	c.mu.Unlock()
	// cmd.Wait closes the pipes; both readers must finish draining before Wait
	// so a final stdout record (turn/completed JSON) is never lost to the race.
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); c.readStdout(stdout) }()
	go func() { defer readers.Done(); c.readStderr(stderr) }()
	go func() { readers.Wait(); c.waitProcess(cmd); tree.Release(); close(procDone) }()

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
	runtimeInfo := c.emitInitializeRuntimeInfo(initializeResult, probe, probeErr)
	if err := c.notify("initialized", map[string]any{}); err != nil {
		_ = c.Stop(context.Background())
		return fmt.Errorf("acknowledge codex initialization: %w", err)
	}

	c.mu.Lock()
	existingThread := c.threadID
	c.mu.Unlock()
	requiredThread := existingThread
	strictResume := requiredThread != ""
	var result json.RawMessage
	if existingThread != "" {
		result, err = c.call(handshakeCtx, "thread/resume", c.threadResumeParams(existingThread))
		if err != nil {
			_ = c.Stop(context.Background())
			return fmt.Errorf("resume required Codex thread %q: %w", requiredThread, err)
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
	if runtimeInfo.SessionName != "" {
		runtimeInfo = c.syncSessionName(ctx, runtimeInfo, threadResult.Thread.ID)
		emitRuntimeInfo(c.sink, c.cfg.Actor, runtimeInfo)
	}
	c.setState(model.StateIdle, "")
	session := runtimeEvent(c.cfg.Actor, model.RuntimeSession)
	session.SessionID = threadResult.Thread.ID
	c.sink(session)
	return nil
}

func (c *CodexAdapter) emitInitializeRuntimeInfo(result json.RawMessage, probe ProbeResult, probeErr error) model.RuntimeInfo {
	info := model.RuntimeInfo{
		Available: true, Command: c.cfg.Command, Protocol: "codex-app-server-jsonrpc",
		RuntimeKind: c.cfg.Runtime.CanonicalForSlot(c.cfg.Actor),
		Provider:    c.cfg.Provider, ProviderName: c.cfg.ProviderName,
		Model: c.cfg.Model, Effort: c.cfg.Effort, ApprovalPolicy: c.cfg.ApprovalPolicy, Sandbox: c.cfg.Sandbox,
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
	return info
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

func (c *CodexAdapter) Stop(ctx context.Context) error {
	c.mu.Lock()
	cmd := c.cmd
	tree := c.tree
	stdin := c.stdin
	procDone := c.procDone
	c.intentional = true
	c.stdin = nil
	c.mu.Unlock()
	c.failPendingRPCs("Codex was stopped")
	if stdin != nil {
		_ = stdin.Close()
	}
	// Bounded graceful window so app-server can flush its rollout before the
	// hard kill; a strict resume later then still finds a complete record.
	waitGracefulExit(ctx, procDone)
	if cmd != nil {
		// The process stays recorded until its whole tree has exited, so a
		// failed stop can be retried instead of freeing capacity while the real
		// CLI behind a launcher shim keeps running. Inputs are cancelled only
		// after that exit evidence.
		if err := stopProcessTree(tree, procDone, "Codex app-server"); err != nil {
			return err
		}
	}
	c.mu.Lock()
	if c.cmd == cmd {
		c.cmd, c.tree, c.procDone = nil, nil, nil
	}
	c.mu.Unlock()
	for _, input := range c.takeOutstandingInputs() {
		c.emitInputTerminal("", input, model.RuntimeInputCancelled, "Codex was stopped")
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
		c.tree = nil
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

// failStream stops an app-server whose stdout can no longer be read. The
// exit is then reported through waitProcess like any unexpected exit, so
// outstanding input fails and the Turn owner is released on real exit.
func (c *CodexAdapter) failStream(reason string) {
	c.mu.Lock()
	if c.streamFailure == "" {
		c.streamFailure = reason
	}
	tree := c.tree
	c.mu.Unlock()
	e := runtimeEvent(c.cfg.Actor, model.RuntimeError)
	e.Name = "adapter.stream_error"
	e.Text = reason
	c.sink(e)
	_ = tree.Kill()
}

func (c *CodexAdapter) handleUnexpectedProcessExit(err error) {
	c.mu.Lock()
	streamFailure := c.streamFailure
	c.streamFailure = ""
	c.mu.Unlock()
	detail := "Codex app-server exited"
	if streamFailure != "" {
		detail = streamFailure
	} else if err != nil {
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
	if err != nil || streamFailure != "" || outstanding.turnID != "" || len(outstanding.inputs) > 0 {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeError)
		e.Name = "adapter.process_exited"
		e.Text = detail
		c.sink(e)
		c.setState(model.StateError, detail)
		return
	}
	c.setState(model.StateStopped, "")
}
