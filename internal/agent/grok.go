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

	"github.com/sean2077/pairroom/internal/execx"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

type GrokAdapter struct {
	cfg  Config
	sink EventSink

	startMu   sync.Mutex
	submitMu  sync.Mutex
	mu        sync.Mutex
	state     model.AgentState
	sessionID string
	cmd       *exec.Cmd
	role      model.ParticipantRole
}

func NewGrok(cfg Config, sink EventSink) *GrokAdapter {
	if !cfg.Actor.ValidParticipant() {
		cfg.Actor = model.ActorClaude
	}
	if cfg.Command == "" {
		cfg.Command = model.RuntimeGrok.DefaultCommand()
	}
	cfg.Runtime = model.RuntimeGrok
	return &GrokAdapter{
		cfg: cfg, sink: sink, state: model.StateStopped,
		sessionID: strings.TrimSpace(cfg.SessionID), role: model.RoleDriver,
	}
}

func (g *GrokAdapter) Actor() model.ActorID { return g.cfg.Actor }

func (g *GrokAdapter) State() model.AgentState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

func (g *GrokAdapter) SessionID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sessionID
}

func (g *GrokAdapter) setState(state model.AgentState, detail string) {
	g.mu.Lock()
	changed := g.state != state
	g.state = state
	g.mu.Unlock()
	if !changed && detail == "" {
		return
	}
	e := runtimeEvent(g.cfg.Actor, model.RuntimeState)
	e.State = state
	e.Text = detail
	g.sink(e)
}

func (g *GrokAdapter) Start(ctx context.Context) error {
	g.startMu.Lock()
	defer g.startMu.Unlock()

	g.mu.Lock()
	if g.state != model.StateStopped && g.state != model.StateError {
		g.mu.Unlock()
		return nil
	}
	g.state = model.StateStarting
	g.mu.Unlock()

	probe, probeErr := ProbeRuntime(ctx, g.cfg)
	info := model.RuntimeInfo{
		Available: false, Command: g.cfg.Command, Protocol: "grok-streaming-json",
		RuntimeKind: model.RuntimeGrok, Provider: g.cfg.Provider, Model: g.cfg.Model,
		Effort: g.cfg.Effort, PermissionMode: g.cfg.PermissionMode, Sandbox: g.cfg.Sandbox,
		ProbedAt: time.Now().UTC(),
	}
	if probeErr == nil {
		info = probe.RuntimeInfo(g.cfg)
	} else {
		info.Warnings = []string{probeErr.Error()}
	}
	emitRuntimeInfo(g.sink, g.cfg.Actor, info)
	if probeErr != nil {
		g.setState(model.StateError, probeErr.Error())
		return probeErr
	}
	g.mu.Lock()
	if g.sessionID == "" {
		g.sessionID = strings.TrimSpace(g.cfg.SessionID)
	}
	sessionID := g.sessionID
	g.mu.Unlock()
	if sessionID != "" {
		session := runtimeEvent(g.cfg.Actor, model.RuntimeSession)
		session.SessionID = sessionID
		g.sink(session)
	}
	g.setState(model.StateIdle, "")
	return nil
}

func (g *GrokAdapter) Submit(ctx context.Context, input model.AgentInput) (model.DeliveryState, error) {
	g.submitMu.Lock()
	defer g.submitMu.Unlock()
	if g.State() == model.StateStopped || g.State() == model.StateError {
		if err := g.Start(ctx); err != nil {
			return model.DeliveryFailed, err
		}
	}
	g.mu.Lock()
	if g.cmd != nil && g.cmd.Process != nil {
		g.mu.Unlock()
		return model.DeliveryQueued, nil
	}
	g.mu.Unlock()

	body := prompt.Envelope(input)
	promptPath, err := g.writePromptFile(body)
	if err != nil {
		return model.DeliveryFailed, err
	}
	g.mu.Lock()
	resumeID := g.sessionID
	if resumeID == "" {
		resumeID = strings.TrimSpace(g.cfg.SessionID)
	}
	g.mu.Unlock()
	args := g.buildArgs(promptPath, resumeID)
	cmd := exec.Command(g.cfg.Command, args...)
	execx.NoConsole(cmd)
	cmd.Dir = g.cfg.Repo
	cmd.Env = mergeRuntimeEnv(envWithout(), g.cfg.Env)
	cmd.Env = mergeRuntimeEnv(cmd.Env, map[string]string{"GROK_DISABLE_AUTOUPDATER": "1"})
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return model.DeliveryFailed, fmt.Errorf("grok stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return model.DeliveryFailed, fmt.Errorf("grok stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return model.DeliveryFailed, fmt.Errorf("start grok: %w", err)
	}
	turnID := model.NewID("turn")
	g.mu.Lock()
	g.cmd = cmd
	g.mu.Unlock()
	g.setState(model.StateWorking, "")
	started := runtimeEvent(g.cfg.Actor, model.RuntimeTurnStarted)
	started.TurnID = turnID
	started.CorrelationID = input.MessageID
	g.sink(started)

	go g.readStderr(stderr, turnID, input.MessageID)
	err = g.readStdout(stdout, turnID, input.MessageID)
	waitErr := cmd.Wait()
	g.mu.Lock()
	g.cmd = nil
	sessionID := g.sessionID
	g.mu.Unlock()
	if err != nil && !errors.Is(err, io.EOF) {
		g.setState(model.StateIdle, err.Error())
		failed := runtimeEvent(g.cfg.Actor, model.RuntimeError)
		failed.TurnID = turnID
		failed.CorrelationID = input.MessageID
		failed.Text = err.Error()
		g.sink(failed)
	} else if waitErr != nil {
		g.setState(model.StateIdle, waitErr.Error())
		failed := runtimeEvent(g.cfg.Actor, model.RuntimeError)
		failed.TurnID = turnID
		failed.CorrelationID = input.MessageID
		failed.Text = waitErr.Error()
		g.sink(failed)
	} else {
		g.setState(model.StateIdle, "")
	}
	if sessionID != "" {
		session := runtimeEvent(g.cfg.Actor, model.RuntimeSession)
		session.SessionID = sessionID
		g.sink(session)
	}
	completed := runtimeEvent(g.cfg.Actor, model.RuntimeTurnCompleted)
	completed.TurnID = turnID
	completed.CorrelationID = input.MessageID
	completed.SessionID = sessionID
	g.sink(completed)
	return model.DeliveryStarted, nil
}

func (g *GrokAdapter) Interrupt(ctx context.Context) error {
	g.mu.Lock()
	cmd := g.cmd
	g.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = ctx
	return cmd.Process.Kill()
}

func (g *GrokAdapter) Stop(ctx context.Context) error {
	_ = g.Interrupt(ctx)
	g.setState(model.StateStopped, "")
	return nil
}

func (g *GrokAdapter) ResolveApproval(context.Context, string, model.ApprovalResolution) error {
	return ErrApprovalUnsupported
}

func (g *GrokAdapter) SetRole(_ context.Context, role model.ParticipantRole) error {
	if !role.Valid() {
		return fmt.Errorf("invalid Grok Build role %q", role)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == model.StateStarting || g.state == model.StateWorking || g.state == model.StateWaiting ||
		(g.cmd != nil && g.cmd.Process != nil) {
		return errors.New("interrupt or stop Grok Build before changing its role")
	}
	g.role = role
	return nil
}

func (g *GrokAdapter) SetWorkspace(_ context.Context, path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return errors.New("Grok Build workspace is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat Grok Build workspace: %w", err)
	}
	if !info.IsDir() {
		return errors.New("Grok Build workspace is not a directory")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if filepath.Clean(g.cfg.Repo) == path {
		return nil
	}
	if g.state == model.StateStarting || g.state == model.StateWorking || g.state == model.StateWaiting ||
		(g.cmd != nil && g.cmd.Process != nil) {
		return errors.New("interrupt or stop Grok Build before changing its workspace")
	}
	g.cfg.Repo = path
	return nil
}

func (g *GrokAdapter) buildArgs(promptPath, resumeID string) []string {
	args := append([]string(nil), g.cfg.CommandArgs...)
	args = append(args, "--prompt-file", promptPath, "--output-format", "streaming-json")
	if g.cfg.Repo != "" {
		args = append(args, "--cwd", g.cfg.Repo)
	}
	if model := strings.TrimSpace(g.cfg.Model); model != "" {
		args = append(args, "--model", model)
	}
	if effort := strings.TrimSpace(g.cfg.Effort); effort != "" {
		args = append(args, "--effort", effort)
	}
	g.mu.Lock()
	role := g.role
	g.mu.Unlock()
	if role == model.RoleReviewer {
		// Compiled plan/review/audit stages and enforced ordinary Reviewer
		// turns must override any broader Service default. Appending last keeps
		// the fail-closed policy authoritative even when a command template
		// contains an earlier permission flag.
		args = append(args, "--permission-mode", "plan", "--sandbox", "read-only")
	} else {
		args = append(args, grokPermissionArgs(g.cfg.PermissionMode)...)
		if sandbox := strings.TrimSpace(g.cfg.Sandbox); sandbox != "" {
			args = append(args, "--sandbox", grokSandbox(sandbox))
		}
	}
	if strings.TrimSpace(resumeID) != "" {
		args = append(args, "--resume", resumeID)
	}
	return args
}

func grokSandbox(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "workspace-write", "workspacewrite", "workspace_write":
		return "workspace"
	case "danger-full-access", "dangerfullaccess", "danger_full_access":
		return "off"
	default:
		return strings.TrimSpace(value)
	}
}

func grokPermissionArgs(mode string) []string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "yolo", "bypass", "bypasspermissions", "always-approve", "always_approve":
		return []string{"--yolo"}
	case "":
		return nil
	default:
		return []string{"--permission-mode", mode}
	}
}

func (g *GrokAdapter) writePromptFile(body string) (string, error) {
	dir := g.cfg.DataDir
	if dir == "" {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "runtime", string(g.cfg.Actor))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create grok prompt directory: %w", err)
	}
	path := filepath.Join(dir, "prompt.txt")
	text := collaborationPrompt(g.cfg)
	if strings.TrimSpace(text) != "" {
		text += "\n\n"
	}
	text += body
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", fmt.Errorf("write grok prompt file: %w", err)
	}
	return path, nil
}

func (g *GrokAdapter) readStdout(r io.Reader, turnID, correlationID string) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var final strings.Builder
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			logEvent := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
			logEvent.TurnID = turnID
			logEvent.CorrelationID = correlationID
			logEvent.Name = "grok.stdout"
			logEvent.Text = line
			g.sink(logEvent)
			continue
		}
		g.handleStreamEvent(event, turnID, correlationID, &final)
	}
	return scanner.Err()
}

func (g *GrokAdapter) handleStreamEvent(event map[string]any, turnID, correlationID string, final *strings.Builder) {
	kind, _ := event["type"].(string)
	encoded, _ := json.Marshal(event)
	switch kind {
	case "text":
		text := grokEventText(event)
		if text == "" {
			return
		}
		final.WriteString(text)
		delta := runtimeEvent(g.cfg.Actor, model.RuntimeTextDelta)
		delta.TurnID = turnID
		delta.CorrelationID = correlationID
		delta.Text = text
		g.sink(delta)
	case "thought":
		logEvent := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		logEvent.TurnID = turnID
		logEvent.CorrelationID = correlationID
		logEvent.Name = "thought"
		logEvent.Text = grokEventText(event)
		logEvent.Data = encoded
		g.sink(logEvent)
	case "tool_call":
		started := runtimeEvent(g.cfg.Actor, model.RuntimeToolStarted)
		started.TurnID = turnID
		started.CorrelationID = correlationID
		started.ItemID = grokString(event, "toolCallId")
		started.Name = grokString(event, "toolName")
		if started.Name == "" {
			started.Name = grokString(event, "title")
		}
		started.Data = encoded
		g.sink(started)
	case "tool_call_update":
		completed := runtimeEvent(g.cfg.Actor, model.RuntimeToolCompleted)
		completed.TurnID = turnID
		completed.CorrelationID = correlationID
		completed.ItemID = grokString(event, "toolCallId")
		completed.Name = grokString(event, "status")
		completed.Data = encoded
		g.sink(completed)
	case "plan":
		updated := runtimeEvent(g.cfg.Actor, model.RuntimePlanUpdated)
		updated.TurnID = turnID
		updated.CorrelationID = correlationID
		updated.Data = encoded
		g.sink(updated)
	case "usage":
		usage := runtimeEvent(g.cfg.Actor, model.RuntimeUsageUpdated)
		usage.TurnID = turnID
		usage.CorrelationID = correlationID
		usage.Data = encoded
		g.sink(usage)
	case "end":
		if sessionID := grokString(event, "sessionId"); sessionID != "" {
			g.mu.Lock()
			g.sessionID = sessionID
			g.mu.Unlock()
		}
		text := grokString(event, "result")
		if text == "" {
			text = final.String()
		}
		if text != "" {
			done := runtimeEvent(g.cfg.Actor, model.RuntimeFinal)
			done.TurnID = turnID
			done.CorrelationID = correlationID
			done.Text = text
			done.Data = encoded
			g.sink(done)
		}
	case "error":
		failed := runtimeEvent(g.cfg.Actor, model.RuntimeError)
		failed.TurnID = turnID
		failed.CorrelationID = correlationID
		failed.Text = grokString(event, "message")
		if failed.Text == "" {
			failed.Text = grokEventText(event)
		}
		failed.Data = encoded
		g.sink(failed)
	default:
		logEvent := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		logEvent.TurnID = turnID
		logEvent.CorrelationID = correlationID
		logEvent.Name = kind
		logEvent.Data = encoded
		g.sink(logEvent)
	}
}

func (g *GrokAdapter) readStderr(r io.Reader, turnID, correlationID string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		logEvent := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		logEvent.TurnID = turnID
		logEvent.CorrelationID = correlationID
		logEvent.Name = "grok.stderr"
		logEvent.Text = line
		g.sink(logEvent)
	}
}

func grokEventText(event map[string]any) string {
	if text := grokString(event, "data"); text != "" {
		return text
	}
	return grokString(event, "text")
}

func grokString(event map[string]any, key string) string {
	value, _ := event[key].(string)
	return strings.TrimSpace(value)
}
