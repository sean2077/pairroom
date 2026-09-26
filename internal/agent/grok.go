package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sean2077/pairroom/internal/execx"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/version"
)

type grokCapabilities struct {
	loadSession bool
	close       bool
	promptImage bool
}

// GrokAdapter hosts one long-lived official Grok Build ACP process. PairRoom
// owns message queuing; this adapter owns only the active native prompt and
// request/response correlation on its stdio connection.
type GrokAdapter struct {
	cfg  Config
	sink EventSink

	startMu  sync.Mutex
	submitMu sync.Mutex
	mu       sync.Mutex
	writeMu  sync.Mutex

	state     model.AgentState
	access    model.NativeAccess
	sessionID string
	// sessionEngaged distinguishes a durable/existing binding from a lazy
	// session/new ID that was allocated before the first PairRoom prompt. The
	// latter must not be forced through session/load after a pre-prompt crash.
	sessionEngaged   bool
	sessionOpened    bool
	bootstrapPending bool
	capabilities     grokCapabilities
	runtimeInfo      model.RuntimeInfo
	cmd              *exec.Cmd
	stdin            io.WriteCloser
	done             chan struct{}
	intentional      bool
	pending          map[int64]chan grokRPCReply
	approvals        map[string]grokPendingApproval
	turn             *grokTurn
	nextRequestID    atomic.Int64
}

func NewGrok(cfg Config, sink EventSink) *GrokAdapter {
	if !cfg.Actor.ValidParticipant() {
		cfg.Actor = model.ActorSlot1
	}
	if cfg.Command == "" {
		cfg.Command = model.RuntimeGrok.DefaultCommand()
	}
	cfg.Runtime = model.RuntimeGrok
	return &GrokAdapter{
		cfg: cfg, sink: sink, state: model.StateStopped,
		sessionID: strings.TrimSpace(cfg.SessionID), sessionEngaged: strings.TrimSpace(cfg.SessionID) != "",
		pending:   make(map[int64]chan grokRPCReply),
		approvals: make(map[string]grokPendingApproval),
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
	if g.cmd != nil && g.cmd.Process != nil {
		g.mu.Unlock()
		return nil
	}
	g.state = model.StateStarting
	g.intentional = false
	if strings.TrimSpace(g.cfg.SessionID) == "" && !g.sessionEngaged {
		// A previous session/new may have allocated an ID but never accepted a
		// PairRoom prompt. It is ephemeral and must not be resumed exactly.
		g.sessionID = ""
	}
	g.mu.Unlock()

	probe, probeErr := ProbeRuntime(ctx, g.cfg)
	info := model.RuntimeInfo{
		Available: false, Command: g.cfg.Command, Protocol: "grok-acp-v1", RuntimeKind: model.RuntimeGrok,
		Provider: g.cfg.Provider, ProviderName: g.cfg.ProviderName, Model: g.cfg.Model, Effort: g.cfg.Effort,
		PermissionMode: g.cfg.PermissionMode, Sandbox: g.cfg.Sandbox, ProbedAt: time.Now().UTC(),
	}
	if probeErr == nil {
		info = probe.RuntimeInfo(g.cfg)
		info.Protocol = "grok-acp-v1"
		// Grok Build shipped the interjection extension under the private
		// `_x.ai/interject` name in earlier ACP builds and under the public
		// `x.ai/interject` name in current builds. Keep both in the diagnostic
		// projection; Steer probes the private spelling first for the protocol
		// contract and falls back only when the server reports method-not-found.
		info.Capabilities = append(info.Capabilities, "session/prompt", "session/cancel", "session/request_permission", "_x.ai/interject", "x.ai/interject")
	} else {
		info.Warnings = []string{probeErr.Error()}
	}
	g.mu.Lock()
	g.runtimeInfo = info
	g.mu.Unlock()
	emitRuntimeInfo(g.sink, g.cfg.Actor, info)
	if probeErr != nil {
		g.setState(model.StateError, probeErr.Error())
		return probeErr
	}

	cmd := exec.Command(g.cfg.Command, g.buildACPArgs()...)
	execx.NoConsole(cmd)
	cmd.Dir = g.cfg.Repo
	cmd.Env = mergeRuntimeEnv(envWithout(), g.cfg.Env)
	cmd.Env = mergeRuntimeEnv(cmd.Env, map[string]string{"GROK_DISABLE_AUTOUPDATER": "1"})
	stdin, err := cmd.StdinPipe()
	if err != nil {
		g.setState(model.StateError, err.Error())
		return fmt.Errorf("grok ACP stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		g.setState(model.StateError, err.Error())
		return fmt.Errorf("grok ACP stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		g.setState(model.StateError, err.Error())
		return fmt.Errorf("grok ACP stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		g.setState(model.StateError, err.Error())
		return fmt.Errorf("start grok ACP: %w", err)
	}

	done := make(chan struct{})
	g.mu.Lock()
	g.cmd = cmd
	g.stdin = stdin
	g.done = done
	g.pending = make(map[int64]chan grokRPCReply)
	g.approvals = make(map[string]grokPendingApproval)
	g.mu.Unlock()
	// cmd.Wait closes the pipes; both readers must finish draining before Wait
	// so a final stdout record is never lost to the race.
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); g.readStdout(stdout) }()
	go func() { defer readers.Done(); g.readStderr(stderr) }()
	go func() { readers.Wait(); g.waitProcess(cmd, done) }()

	clientVersion := strings.TrimSpace(g.cfg.ClientVersion)
	if clientVersion == "" {
		clientVersion = version.Current
	}
	initParams := map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": "pairroom", "title": "PairRoom", "version": clientVersion},
		"_meta": map[string]any{
			"clientType": "pairroom", "clientVersion": clientVersion,
			"startupHints": map[string]any{"nonInteractive": true, "skipGitStatus": true, "skipProjectLayout": true},
		},
	}
	result, err := g.call(ctx, "initialize", initParams)
	if err != nil {
		g.abortStart(cmd)
		return fmt.Errorf("initialize grok ACP: %w", err)
	}
	method, err := selectGrokAuthMethod(result)
	if err != nil {
		g.abortStart(cmd)
		return err
	}
	if method != "" {
		if _, err := g.call(ctx, "authenticate", map[string]any{"methodId": method, "_meta": map[string]any{"headless": true}}); err != nil {
			g.abortStart(cmd)
			return fmt.Errorf("authenticate grok ACP: %w", err)
		}
	}
	capabilities, err := parseGrokCapabilities(result)
	if err != nil {
		g.abortStart(cmd)
		return err
	}
	g.mu.Lock()
	g.capabilities = capabilities
	hasSession := strings.TrimSpace(g.sessionID) != "" && (strings.TrimSpace(g.cfg.SessionID) != "" || g.sessionEngaged)
	g.mu.Unlock()
	// Existing bindings must prove exact session/load during startup validation.
	// A genuinely new binding remains lazy and allocates no vendor identity
	// until PairRoom has a real Turn to submit.
	if hasSession {
		if err := g.ensureSession(ctx); err != nil {
			g.abortStart(cmd)
			return err
		}
	}
	g.setState(model.StateIdle, "")
	return nil
}

func parseGrokCapabilities(raw json.RawMessage) (grokCapabilities, error) {
	var response struct {
		AgentCapabilities struct {
			LoadSession        bool `json:"loadSession"`
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
			SessionCapabilities struct {
				Close json.RawMessage `json:"close"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return grokCapabilities{}, fmt.Errorf("decode Grok ACP capabilities: %w", err)
	}
	return grokCapabilities{
		loadSession: response.AgentCapabilities.LoadSession,
		close:       grokCapabilityEnabled(response.AgentCapabilities.SessionCapabilities.Close),
		promptImage: response.AgentCapabilities.PromptCapabilities.Image,
	}, nil
}

func grokCapabilityEnabled(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err == nil {
		return enabled
	}
	// ACP versions have represented supported method capabilities as either an
	// empty object or a boolean. Any non-null structured descriptor means the
	// method is available; malformed scalar data fails closed.
	var descriptor map[string]any
	return json.Unmarshal(raw, &descriptor) == nil && descriptor != nil
}

func selectGrokAuthMethod(raw json.RawMessage) (string, error) {
	var response struct {
		AuthMethods []struct {
			ID string `json:"id"`
		} `json:"authMethods"`
		Meta map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("decode grok initialize response: %w", err)
	}
	if len(response.AuthMethods) == 0 {
		return "", nil
	}
	available := make(map[string]bool, len(response.AuthMethods))
	for _, method := range response.AuthMethods {
		available[method.ID] = true
	}
	if value, _ := response.Meta["defaultAuthMethodId"].(string); available[value] {
		return value, nil
	}
	for _, candidate := range []string{"cached_token", "xai.api_key"} {
		if available[candidate] {
			return candidate, nil
		}
	}
	return "", errors.New("Grok Build has no non-interactive authentication method; run `grok login` or set XAI_API_KEY")
}

func (g *GrokAdapter) abortStart(cmd *exec.Cmd) {
	g.mu.Lock()
	g.intentional = true
	g.mu.Unlock()
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	g.setState(model.StateError, "Grok ACP startup failed")
}

func (g *GrokAdapter) buildACPArgs() []string {
	args := append([]string(nil), g.cfg.CommandArgs...)
	args = append(args, "--no-auto-update")
	if repo := strings.TrimSpace(g.cfg.Repo); repo != "" {
		args = append(args, "--cwd", repo)
	}
	if value := strings.TrimSpace(g.cfg.Model); value != "" {
		args = append(args, "--model", value)
	}
	if value := strings.TrimSpace(g.cfg.Effort); value != "" {
		args = append(args, "--reasoning-effort", value)
	}
	args = append(args, grokPermissionArgs(g.cfg.PermissionMode)...)
	if value := strings.TrimSpace(g.cfg.Sandbox); value != "" {
		args = append(args, "--sandbox", value)
	}
	return append(args, "agent", "stdio")
}

func (g *GrokAdapter) Interrupt(ctx context.Context) error {
	g.mu.Lock()
	active := g.turn != nil
	sessionID := g.sessionID
	g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	result := g.cancelPendingInteractions()
	if active && sessionID != "" {
		result = errors.Join(result, g.sendNotification("session/cancel", map[string]any{"sessionId": sessionID}))
	}
	return result
}

func (g *GrokAdapter) Stop(ctx context.Context) error {
	g.startMu.Lock()
	defer g.startMu.Unlock()
	g.mu.Lock()
	cmd := g.cmd
	done := g.done
	sessionID := g.sessionID
	opened := g.sessionOpened
	canClose := g.capabilities.close
	engaged := g.sessionEngaged
	g.intentional = true
	g.mu.Unlock()
	if cmd == nil {
		g.mu.Lock()
		if strings.TrimSpace(g.cfg.SessionID) == "" && !engaged {
			g.sessionID = ""
		}
		g.sessionOpened = false
		g.bootstrapPending = false
		g.capabilities = grokCapabilities{}
		g.turn = nil
		g.pending = make(map[int64]chan grokRPCReply)
		g.approvals = make(map[string]grokPendingApproval)
		g.mu.Unlock()
		g.setState(model.StateStopped, "")
		return nil
	}
	_ = g.cancelPendingInteractions()
	if opened && sessionID != "" && canClose {
		closeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, _ = g.call(closeCtx, "session/close", map[string]any{"sessionId": sessionID})
		cancel()
	}
	g.mu.Lock()
	stdin := g.stdin
	g.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	select {
	case <-done:
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		// Readers reach EOF only once every inherited descriptor holder exits;
		// a descendant that kept the pipes open must not hang the whole
		// shutdown chain after the direct process was killed. Report the
		// uncertain close honestly instead.
		killWait := time.NewTimer(5 * time.Second)
		select {
		case <-done:
			killWait.Stop()
		case <-killWait.C:
			g.setState(model.StateStopped, "")
			return errors.New("Grok process was killed but its output pipes remain open (a descendant may hold them); close state is uncertain")
		}
	}
	g.mu.Lock()
	if strings.TrimSpace(g.cfg.SessionID) == "" && !engaged {
		g.sessionID = ""
	}
	g.sessionOpened = false
	g.bootstrapPending = false
	g.capabilities = grokCapabilities{}
	g.turn = nil
	g.mu.Unlock()
	g.setState(model.StateStopped, "")
	// The process was confirmed stopped (<-done) even on the ctx-timeout kill
	// path; reporting the expired context here would mark a completed stop as
	// an uncertain close and strand the runtime's capacity slot.
	return nil
}

func (g *GrokAdapter) waitProcess(cmd *exec.Cmd, done chan struct{}) {
	err := cmd.Wait()
	g.mu.Lock()
	if g.cmd != cmd {
		g.mu.Unlock()
		close(done)
		return
	}
	intentional := g.intentional
	engaged := g.sessionEngaged
	g.cmd = nil
	g.stdin = nil
	g.done = nil
	g.sessionOpened = false
	g.capabilities = grokCapabilities{}
	activeTurn := g.turn
	g.turn = nil
	sessionID := g.sessionID
	pending := g.pending
	g.pending = make(map[int64]chan grokRPCReply)
	g.approvals = make(map[string]grokPendingApproval)
	if strings.TrimSpace(g.cfg.SessionID) == "" && !engaged {
		g.sessionID = ""
	}
	g.mu.Unlock()
	detail := "Grok ACP process exited"
	if err != nil {
		detail += ": " + err.Error()
	}
	failure := g.redactError(errors.New(detail))
	detail = failure.Error()
	for _, reply := range pending {
		reply <- grokRPCReply{err: failure}
	}
	if !intentional {
		if activeTurn != nil {
			for _, input := range activeTurn.inputs {
				e := runtimeEvent(g.cfg.Actor, model.RuntimeInputFailed)
				e.TurnID = activeTurn.turnID
				e.CorrelationID = input.MessageID
				e.Name = string(model.ProcessingFailed)
				e.Text = detail
				g.sink(e)
			}
			completed := runtimeEvent(g.cfg.Actor, model.RuntimeTurnCompleted)
			completed.TurnID = activeTurn.turnID
			completed.CorrelationID = lastGrokInputID(activeTurn.inputs)
			completed.SessionID = sessionID
			completed.Name = "process_exited"
			g.sink(completed)
		}
		e := runtimeEvent(g.cfg.Actor, model.RuntimeError)
		e.Name = "adapter.process_exited"
		e.Text = detail
		g.sink(e)
		g.setState(model.StateError, detail)
	}
	close(done)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
