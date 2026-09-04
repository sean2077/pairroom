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

type grokRPCReply struct {
	result json.RawMessage
	err    error
}

type grokRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e grokRPCError) Error() string {
	if e.Code == 0 {
		return e.Message
	}
	return fmt.Sprintf("grok ACP error %d: %s", e.Code, e.Message)
}

type grokTurn struct {
	turnID string
	inputs []model.AgentInput
	final  strings.Builder
}

type grokCapabilities struct {
	loadSession bool
	close       bool
	promptImage bool
}

type grokPermissionOption struct {
	ID   string `json:"optionId"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type grokPendingApproval struct {
	rawID    json.RawMessage
	kind     string
	options  []grokPermissionOption
	approval model.Approval
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
	role      model.ParticipantRole
	sessionID string
	// sessionEngaged distinguishes a durable/existing binding from a lazy
	// session/new ID that was allocated before the first PairRoom prompt. The
	// latter must not be forced through session/load after a pre-prompt crash.
	sessionEngaged   bool
	sessionOpened    bool
	bootstrapPending bool
	capabilities     grokCapabilities
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
		cfg.Actor = model.ActorClaude
	}
	if cfg.Command == "" {
		cfg.Command = model.RuntimeGrok.DefaultCommand()
	}
	cfg.Runtime = model.RuntimeGrok
	return &GrokAdapter{
		cfg: cfg, sink: sink, state: model.StateStopped, role: model.RoleDriver,
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
		Provider: g.cfg.Provider, Model: g.cfg.Model, Effort: g.cfg.Effort,
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
	go g.readStdout(stdout)
	go g.readStderr(stderr)
	go g.waitProcess(cmd, done)

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
	case "auto", "yolo", "bypass", "bypasspermissions", "always-approve", "always_approve":
		return []string{"--always-approve"}
	case "":
		return nil
	default:
		return []string{"--permission-mode", mode}
	}
}

func (g *GrokAdapter) ensureSession(ctx context.Context) error {
	g.mu.Lock()
	if g.sessionOpened {
		g.mu.Unlock()
		return nil
	}
	required := strings.TrimSpace(g.sessionID)
	repo := g.cfg.Repo
	role := g.role
	capabilities := g.capabilities
	g.mu.Unlock()

	loaded := required != "" && (strings.TrimSpace(g.cfg.SessionID) != "" || g.sessionEngaged)
	if !loaded {
		required = ""
	}
	if loaded {
		if !capabilities.loadSession {
			return fmt.Errorf("load exact Grok session %q: runtime did not advertise session/load", required)
		}
		result, err := g.call(ctx, "session/load", map[string]any{
			"sessionId": required, "cwd": repo, "mcpServers": []any{},
			"_meta": map[string]any{"noReplay": true, "startupHints": grokStartupHints()},
		})
		if err != nil {
			return fmt.Errorf("load exact Grok session %q: %w", required, err)
		}
		var response struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(result, &response); err == nil && strings.TrimSpace(response.SessionID) != "" && strings.TrimSpace(response.SessionID) != required {
			return fmt.Errorf("Grok session/load returned %q instead of required session %q", response.SessionID, required)
		}
	} else {
		result, err := g.call(ctx, "session/new", map[string]any{
			"cwd": repo, "mcpServers": []any{},
			"_meta": map[string]any{"rules": collaborationPrompt(g.cfg), "sessionKind": "headless", "startupHints": grokStartupHints()},
		})
		if err != nil {
			return fmt.Errorf("create Grok session: %w", err)
		}
		var response struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(result, &response); err != nil || strings.TrimSpace(response.SessionID) == "" {
			if err == nil {
				err = errors.New("missing sessionId")
			}
			return fmt.Errorf("decode Grok session: %w", err)
		}
		required = strings.TrimSpace(response.SessionID)
	}

	// Retain a newly allocated ID across a recoverable setup failure, but do not
	// publish or mark the session open until its role mode is accepted.
	g.mu.Lock()
	if g.sessionOpened {
		g.mu.Unlock()
		return nil
	}
	g.sessionID = required
	g.mu.Unlock()
	if err := g.applyRoleMode(ctx, required, role); err != nil {
		return err
	}
	g.mu.Lock()
	if g.sessionOpened {
		g.mu.Unlock()
		return nil
	}
	g.sessionOpened = true
	g.bootstrapPending = loaded
	g.mu.Unlock()

	session := runtimeEvent(g.cfg.Actor, model.RuntimeSession)
	session.SessionID = required
	g.sink(session)
	return nil
}

func grokStartupHints() map[string]any {
	return map[string]any{"nonInteractive": true, "skipGitStatus": true, "skipProjectLayout": true}
}

func (g *GrokAdapter) applyRoleMode(ctx context.Context, sessionID string, role model.ParticipantRole) error {
	mode := "default"
	if role == model.RoleReviewer {
		mode = "plan"
	}
	_, err := g.call(ctx, "session/set_mode", map[string]any{"sessionId": sessionID, "modeId": mode})
	if err != nil {
		return fmt.Errorf("set Grok session mode %s: %w", mode, err)
	}
	return nil
}

func (g *GrokAdapter) StartTurn(ctx context.Context, input model.AgentInput) error {
	g.submitMu.Lock()
	defer g.submitMu.Unlock()
	g.mu.Lock()
	state := g.state
	running := g.cmd != nil && g.stdin != nil
	g.mu.Unlock()
	if !running || state == model.StateStopped || state == model.StateError {
		if err := g.Start(ctx); err != nil {
			return err
		}
	}
	if err := g.ensureSession(ctx); err != nil {
		return err
	}

	g.mu.Lock()
	if g.turn != nil {
		g.mu.Unlock()
		return errors.New("Grok Build already has an active prompt")
	}
	sessionID := g.sessionID
	bootstrap := g.bootstrapPending
	g.mu.Unlock()

	text := prompt.Envelope(input)
	if bootstrap {
		text = collaborationPrompt(g.cfg) + "\n\n" + text
	}
	g.mu.Lock()
	promptImage := g.capabilities.promptImage
	g.mu.Unlock()
	content, err := grokContent(text, input.Attachments, promptImage)
	if err != nil {
		return err
	}
	requestID := g.nextRequestID.Add(1)
	reply := make(chan grokRPCReply, 1)
	turn := &grokTurn{
		turnID: model.NewID("grok-turn"), inputs: []model.AgentInput{input},
	}
	g.mu.Lock()
	g.pending[requestID] = reply
	g.turn = turn
	g.mu.Unlock()
	if err := g.send(map[string]any{
		"jsonrpc": "2.0", "id": requestID, "method": "session/prompt",
		"params": map[string]any{"sessionId": sessionID, "prompt": content, "_meta": map[string]any{"screenMode": "headless"}},
	}); err != nil {
		g.mu.Lock()
		delete(g.pending, requestID)
		if g.turn == turn {
			g.turn = nil
		}
		g.mu.Unlock()
		return err
	}
	// The prompt request has crossed the native ACP boundary. If this is a
	// newly allocated binding, retain its session ID across a later process
	// restart; before this point the ID was only an uncommitted session/new
	// allocation and may safely be discarded.
	g.mu.Lock()
	g.sessionEngaged = true
	g.mu.Unlock()
	if bootstrap {
		g.mu.Lock()
		g.bootstrapPending = false
		g.mu.Unlock()
	}

	g.setState(model.StateWorking, "")
	started := runtimeEvent(g.cfg.Actor, model.RuntimeTurnStarted)
	started.TurnID = turn.turnID
	started.CorrelationID = input.MessageID
	g.sink(started)
	processing := runtimeEvent(g.cfg.Actor, model.RuntimeInputProcessing)
	processing.TurnID = turn.turnID
	processing.CorrelationID = input.MessageID
	processing.Name = string(model.ProcessingWorking)
	processing.Text = "accepted by Grok ACP"
	g.sink(processing)
	go g.awaitPrompt(turn, reply)
	return nil
}

func grokContent(text string, attachments []model.AgentAttachment, allowImages bool) ([]map[string]any, error) {
	content := []map[string]any{{"type": "text", "text": text}}
	if !allowImages {
		if len(attachments) > 0 {
			return nil, errors.New("Grok Build does not advertise image prompt support")
		}
		return content, nil
	}
	for _, attachment := range attachments {
		if attachment.Kind != "image" || !strings.HasPrefix(strings.ToLower(attachment.MediaType), "image/") {
			return nil, fmt.Errorf("attachment %q is not a Grok image", attachment.Name)
		}
		data, err := os.ReadFile(attachment.Path)
		if err != nil {
			return nil, fmt.Errorf("read Grok image %q: %w", attachment.Name, err)
		}
		if len(data) == 0 || (attachment.Size > 0 && int64(len(data)) != attachment.Size) {
			return nil, fmt.Errorf("Grok image %q changed after attachment validation", attachment.Name)
		}
		content = append(content, map[string]any{
			"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": attachment.MediaType,
		})
	}
	return content, nil
}

func (g *GrokAdapter) Steer(ctx context.Context, input model.AgentInput) SteerOutcome {
	g.submitMu.Lock()
	defer g.submitMu.Unlock()
	g.mu.Lock()
	turn := g.turn
	sessionID := g.sessionID
	g.mu.Unlock()
	if turn == nil || sessionID == "" {
		return SteerOutcome{State: SteerUnavailable, Detail: "Grok Build has no active prompt"}
	}
	text := prompt.Envelope(input)
	g.mu.Lock()
	promptImage := g.capabilities.promptImage
	g.mu.Unlock()
	content, err := grokContent(text, input.Attachments, promptImage)
	if err != nil {
		return SteerOutcome{State: SteerRejected, Detail: err.Error()}
	}
	interjectParams := map[string]any{
		"sessionId": sessionID, "text": text, "interjectionId": input.MessageID, "content": content,
	}
	interjectMethod, acknowledgement, err := g.callGrokInterject(ctx, interjectParams)
	if err != nil {
		var rpcErr grokRPCError
		if errors.As(err, &rpcErr) {
			if rpcErr.Code == -32601 {
				return SteerOutcome{State: SteerUnavailable, Detail: "Grok Build does not expose an interject extension: " + err.Error()}
			}
			return SteerOutcome{State: SteerRejected, Detail: err.Error()}
		}
		return SteerOutcome{State: SteerUnknown, Detail: err.Error()}
	}
	ackState, ackDetail := classifyGrokInterjectAcknowledgement(acknowledgement)
	if ackState != SteerAccepted {
		return SteerOutcome{State: ackState, Detail: ackDetail}
	}
	g.mu.Lock()
	if g.turn != turn {
		g.mu.Unlock()
		// A successful ACP extension response is the native acceptance receipt;
		// the prompt may legitimately finish before this client observes the
		// acknowledgement. Do not downgrade that receipt to an automatic FIFO
		// retry, which could execute the same interjection twice.
		return SteerOutcome{State: SteerAccepted, Detail: "accepted by Grok " + interjectMethod + " before the prompt completed"}
	}
	turn.inputs = append(turn.inputs, input)
	g.mu.Unlock()
	processing := runtimeEvent(g.cfg.Actor, model.RuntimeInputProcessing)
	processing.TurnID = turn.turnID
	processing.CorrelationID = input.MessageID
	processing.Name = string(model.ProcessingWorking)
	processing.Text = "injected through Grok " + interjectMethod
	g.sink(processing)
	return SteerOutcome{State: SteerAccepted, Detail: "accepted by Grok " + interjectMethod}
}

// callGrokInterject handles the extension spelling transition in Grok Build.
// The private `_x.ai/interject` spelling is retained as the first attempt for
// the PairRoom v5 wire contract. A method-not-found response is the only case
// that permits trying the current public `x.ai/interject` spelling: all other
// failures are returned unchanged so an uncertain native write is never
// silently duplicated.
func (g *GrokAdapter) callGrokInterject(ctx context.Context, params map[string]any) (string, json.RawMessage, error) {
	methods := []string{"_x.ai/interject", "x.ai/interject"}
	var last error
	for index, method := range methods {
		result, err := g.call(ctx, method, params)
		if err == nil {
			return method, result, nil
		}
		last = err
		var rpcErr grokRPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 || index == len(methods)-1 {
			return method, nil, err
		}
	}
	return methods[len(methods)-1], nil, last
}

func classifyGrokInterjectAcknowledgement(raw json.RawMessage) (SteerState, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return SteerUnknown, "Grok interject returned no acknowledgement; explicit retry required"
	}
	var response struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return SteerUnknown, "decode Grok interject acknowledgement: " + err.Error()
	}
	status := strings.ToLower(strings.TrimSpace(response.Status))
	detail := firstNonEmpty(response.Reason, response.Error, status)
	switch status {
	case "queued", "accepted", "ok", "success", "pending":
		return SteerAccepted, "Grok interject acknowledged as " + status
	case "unsupported", "unavailable", "not_supported", "not-supported":
		return SteerUnavailable, "Grok interject is unavailable: " + detail
	case "rejected", "denied", "declined", "cancelled", "canceled", "failed", "error":
		return SteerRejected, "Grok interject was rejected: " + detail
	default:
		return SteerUnknown, "Grok interject returned unknown status " + detail + "; explicit retry required"
	}
}

func (g *GrokAdapter) awaitPrompt(turn *grokTurn, reply <-chan grokRPCReply) {
	result := <-reply

	// Serialize turn finalization with StartTurn/Steer. ACP may deliver the
	// session/prompt response and a queued interject acknowledgement back to
	// back; holding submitMu lets a steer that crossed the wire first append its
	// input before we snapshot the completed turn's correlation list.
	g.submitMu.Lock()
	g.mu.Lock()
	if g.turn != turn {
		g.mu.Unlock()
		g.submitMu.Unlock()
		return
	}
	g.turn = nil
	inputs := append([]model.AgentInput(nil), turn.inputs...)
	text := turn.final.String()
	sessionID := g.sessionID
	g.mu.Unlock()
	g.submitMu.Unlock()

	correlationID := ""
	if len(inputs) > 0 {
		correlationID = inputs[len(inputs)-1].MessageID
	}
	terminalKind := model.RuntimeInputCompleted
	terminalState := model.ProcessingCompleted
	status := "completed"
	detail := "completed by Grok ACP"
	if result.err != nil {
		terminalKind = model.RuntimeInputFailed
		terminalState = model.ProcessingFailed
		status = "failed"
		detail = result.err.Error()
		errorEvent := runtimeEvent(g.cfg.Actor, model.RuntimeError)
		errorEvent.TurnID = turn.turnID
		errorEvent.CorrelationID = correlationID
		errorEvent.Text = detail
		g.sink(errorEvent)
		g.setState(model.StateError, detail)
	} else {
		var response struct {
			StopReason string `json:"stopReason"`
		}
		_ = json.Unmarshal(result.result, &response)
		status = strings.TrimSpace(response.StopReason)
		if status == "" {
			status = "completed"
		}
		if status == "cancelled" || status == "canceled" || status == "interrupted" {
			terminalKind = model.RuntimeInputCancelled
			terminalState = model.ProcessingCancelled
			detail = "Grok prompt was " + status
		}
		g.setState(model.StateIdle, "")
	}
	if result.err == nil && terminalKind == model.RuntimeInputCompleted && strings.TrimSpace(text) != "" {
		final := runtimeEvent(g.cfg.Actor, model.RuntimeFinal)
		final.TurnID = turn.turnID
		final.CorrelationID = correlationID
		final.Text = text
		g.sink(final)
	}
	for _, input := range inputs {
		event := runtimeEvent(g.cfg.Actor, terminalKind)
		event.TurnID = turn.turnID
		event.CorrelationID = input.MessageID
		event.Name = string(terminalState)
		event.Text = detail
		g.sink(event)
	}
	completed := runtimeEvent(g.cfg.Actor, model.RuntimeTurnCompleted)
	completed.TurnID = turn.turnID
	completed.CorrelationID = correlationID
	completed.SessionID = sessionID
	completed.Name = status
	g.sink(completed)
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
		<-done
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
	return ctx.Err()
}

func (g *GrokAdapter) ResolveApproval(ctx context.Context, approvalID string, resolution model.ApprovalResolution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	pending, ok := g.approvals[approvalID]
	if ok {
		delete(g.approvals, approvalID)
	}
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown Grok approval %q", approvalID)
	}
	var result any
	if pending.kind == "plan" {
		outcome := "cancelled"
		if resolution.Decision == "accept" || resolution.Decision == "acceptForSession" {
			outcome = "approved"
		}
		result = map[string]any{"outcome": outcome}
	} else {
		optionID := selectGrokPermissionOption(pending.options, resolution.Decision)
		if optionID == "" {
			result = map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
		} else {
			result = map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}}
		}
	}
	if err := g.sendRawResponse(pending.rawID, result, nil); err != nil {
		return err
	}
	g.setState(model.StateWorking, "")
	return nil
}

func (g *GrokAdapter) cancelPendingInteractions() error {
	g.mu.Lock()
	pending := make([]grokPendingApproval, 0, len(g.approvals))
	for id, approval := range g.approvals {
		pending = append(pending, approval)
		delete(g.approvals, id)
	}
	g.mu.Unlock()
	var result error
	for _, approval := range pending {
		response := any(map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
		if approval.kind == "plan" {
			response = map[string]any{"outcome": "cancelled"}
		}
		if err := g.sendRawResponse(approval.rawID, response, nil); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func selectGrokPermissionOption(options []grokPermissionOption, decision string) string {
	wants := []string{"reject_once", "reject_always"}
	switch decision {
	case "acceptForSession":
		wants = []string{"allow_always", "allow_once"}
	case "accept":
		wants = []string{"allow_once", "allow_always"}
	case "decline", "cancel":
	default:
		return ""
	}
	for _, want := range wants {
		for _, option := range options {
			if strings.EqualFold(option.Kind, want) {
				return option.ID
			}
		}
	}
	return ""
}

func (g *GrokAdapter) SetRole(ctx context.Context, role model.ParticipantRole) error {
	if !role.Valid() {
		return fmt.Errorf("invalid Grok role %q", role)
	}
	g.mu.Lock()
	if g.turn != nil {
		g.mu.Unlock()
		return errors.New("interrupt or stop Grok before changing its role")
	}
	oldRole := g.role
	g.role = role
	opened := g.sessionOpened
	sessionID := g.sessionID
	g.mu.Unlock()
	if opened {
		if err := g.applyRoleMode(ctx, sessionID, role); err != nil {
			g.mu.Lock()
			if g.role == role {
				g.role = oldRole
			}
			g.mu.Unlock()
			return err
		}
	}
	return nil
}

func (g *GrokAdapter) SetWorkspace(_ context.Context, path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return errors.New("Grok workspace is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat Grok workspace: %w", err)
	}
	if !info.IsDir() {
		return errors.New("Grok workspace is not a directory")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.turn != nil || g.state == model.StateWorking || g.state == model.StateWaiting || g.state == model.StateStarting {
		return errors.New("interrupt or stop Grok before changing its workspace")
	}
	if g.cmd != nil && filepath.Clean(g.cfg.Repo) != path {
		return errors.New("stop Grok before changing its workspace")
	}
	g.cfg.Repo = path
	return nil
}

func (g *GrokAdapter) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := g.nextRequestID.Add(1)
	reply := make(chan grokRPCReply, 1)
	g.mu.Lock()
	g.pending[id] = reply
	g.mu.Unlock()
	if err := g.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		g.mu.Lock()
		delete(g.pending, id)
		g.mu.Unlock()
		return nil, err
	}
	select {
	case response := <-reply:
		return response.result, g.redactError(response.err)
	case <-ctx.Done():
		g.mu.Lock()
		delete(g.pending, id)
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (g *GrokAdapter) redactError(err error) error {
	if err == nil {
		return nil
	}
	var rpcErr grokRPCError
	if errors.As(err, &rpcErr) {
		rpcErr.Message = redactRuntimeSecrets(rpcErr.Message, g.cfg.Env)
		return rpcErr
	}
	return errors.New(redactRuntimeSecrets(err.Error(), g.cfg.Env))
}

func (g *GrokAdapter) redactRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(redactRuntimeSecrets(string(raw), g.cfg.Env))
}

func (g *GrokAdapter) sendNotification(method string, params any) error {
	return g.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (g *GrokAdapter) sendRawResponse(id json.RawMessage, result any, rpcErr *grokRPCError) error {
	return g.send(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result,omitempty"`
		Error   *grokRPCError   `json:"error,omitempty"`
	}{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
}

func (g *GrokAdapter) send(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode Grok ACP message: %w", err)
	}
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	g.mu.Lock()
	stdin := g.stdin
	g.mu.Unlock()
	if stdin == nil {
		return errors.New("Grok ACP stdin is not available")
	}
	if _, err := stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write Grok ACP message: %w", err)
	}
	return nil
}

func (g *GrokAdapter) readStdout(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		g.handleRPCLine(append([]byte(nil), scanner.Bytes()...))
	}
	if err := scanner.Err(); err != nil {
		e := runtimeEvent(g.cfg.Actor, model.RuntimeError)
		e.Text = "read Grok ACP stream: " + err.Error()
		g.sink(e)
	}
}

func (g *GrokAdapter) readStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16*1024), 1024*1024)
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		e := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		e.Name = "grok.stderr"
		e.Text = redactRuntimeSecrets(text, g.cfg.Env)
		g.sink(e)
	}
}

func (g *GrokAdapter) handleRPCLine(line []byte) {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *grokRPCError   `json:"error"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		e := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		e.Name = "grok.stdout"
		e.Text = redactRuntimeSecrets(string(line), g.cfg.Env)
		g.sink(e)
		return
	}
	if envelope.Method != "" {
		if len(envelope.ID) > 0 && string(envelope.ID) != "null" {
			g.handleServerRequest(envelope.ID, envelope.Method, envelope.Params)
			return
		}
		g.handleNotification(envelope.Method, envelope.Params)
		return
	}
	id, err := strconv.ParseInt(strings.Trim(string(envelope.ID), `"`), 10, 64)
	if err != nil {
		return
	}
	g.mu.Lock()
	reply := g.pending[id]
	delete(g.pending, id)
	g.mu.Unlock()
	if reply == nil {
		return
	}
	if envelope.Error != nil {
		reply <- grokRPCReply{err: *envelope.Error}
	} else {
		reply <- grokRPCReply{result: append(json.RawMessage(nil), envelope.Result...)}
	}
}

func (g *GrokAdapter) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "session/update", "prompt/update":
		g.handleSessionUpdate(params)
	default:
		e := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		e.Name = method
		e.Data = g.redactRaw(params)
		g.sink(e)
	}
}

func (g *GrokAdapter) handleSessionUpdate(raw json.RawMessage) {
	var params struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind    string `json:"sessionUpdate"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Status     string `json:"status"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return
	}
	g.mu.Lock()
	turn := g.turn
	rootSession := g.sessionID
	if turn == nil || params.SessionID != rootSession {
		g.mu.Unlock()
		return
	}
	turnID := turn.turnID
	correlationID := turn.inputs[len(turn.inputs)-1].MessageID
	if params.Update.Kind == "agent_message_chunk" && params.Update.Content.Type == "text" {
		turn.final.WriteString(redactRuntimeSecrets(params.Update.Content.Text, g.cfg.Env))
	}
	g.mu.Unlock()

	event := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
	event.TurnID = turnID
	event.CorrelationID = correlationID
	event.Name = params.Update.Kind
	event.Data = g.redactRaw(raw)
	switch params.Update.Kind {
	case "agent_message_chunk":
		if params.Update.Content.Type != "text" || params.Update.Content.Text == "" {
			return
		}
		event.Kind = model.RuntimeTextDelta
		event.Text = redactRuntimeSecrets(params.Update.Content.Text, g.cfg.Env)
	case "tool_call":
		event.Kind = model.RuntimeToolStarted
		event.ItemID = params.Update.ToolCallID
		event.Name = params.Update.Title
	case "tool_call_update":
		event.Kind = model.RuntimeToolCompleted
		event.ItemID = params.Update.ToolCallID
		event.Name = params.Update.Status
	case "plan":
		event.Kind = model.RuntimePlanUpdated
	default:
		if strings.Contains(params.Update.Kind, "usage") {
			event.Kind = model.RuntimeUsageUpdated
		}
	}
	g.sink(event)
}

func (g *GrokAdapter) handleServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "session/request_permission":
		g.handlePermissionRequest(id, params)
	case "x.ai/exit_plan_mode", "_x.ai/exit_plan_mode":
		g.handlePlanExitRequest(id, params)
	case "x.ai/ask_user_question", "_x.ai/ask_user_question":
		e := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		e.Name = "server_request.unsupported"
		e.Text = method
		e.Data = g.redactRaw(unwrapGrokExtParams(params))
		g.attachCurrentTurn(&e)
		g.sink(e)
		_ = g.sendRawResponse(id, nil, &grokRPCError{Code: -32000, Message: "question surfaced in PairRoom"})
	default:
		e := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		e.Name = "server_request.unsupported"
		e.Text = method
		e.Data = g.redactRaw(params)
		g.attachCurrentTurn(&e)
		g.sink(e)
		_ = g.sendRawResponse(id, nil, &grokRPCError{Code: -32601, Message: "method not supported by PairRoom"})
	}
}

func unwrapGrokExtParams(raw json.RawMessage) json.RawMessage {
	var wrapped struct {
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && len(wrapped.Params) > 0 {
		return append(json.RawMessage(nil), wrapped.Params...)
	}
	return append(json.RawMessage(nil), raw...)
}

func (g *GrokAdapter) handlePermissionRequest(id json.RawMessage, raw json.RawMessage) {
	var params struct {
		Options  []grokPermissionOption `json:"options"`
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		_ = g.sendRawResponse(id, nil, &grokRPCError{Code: -32602, Message: "invalid permission request"})
		return
	}
	detail := map[string]any{}
	_ = json.Unmarshal(raw, &detail)
	detail["permission_suggestions"] = true
	detailRaw, _ := json.Marshal(detail)
	detailRaw = g.redactRaw(detailRaw)
	approval := model.Approval{
		ID: model.NewID("approval"), Agent: g.cfg.Actor, Kind: "grok.permission",
		Title:  firstNonEmpty(params.ToolCall.Title, params.ToolCall.Kind, configuredParticipantName(g.cfg)+" permission request"),
		Detail: detailRaw, Status: "pending", RequestedAt: time.Now().UTC(),
	}
	pending := grokPendingApproval{rawID: append(json.RawMessage(nil), id...), options: params.Options, approval: approval}
	g.mu.Lock()
	g.approvals[approval.ID] = pending
	g.mu.Unlock()
	e := runtimeEvent(g.cfg.Actor, model.RuntimeApprovalRequested)
	e.Approval = &approval
	g.attachCurrentTurn(&e)
	g.sink(e)
	g.setState(model.StateWaiting, "")
}

func (g *GrokAdapter) handlePlanExitRequest(id json.RawMessage, raw json.RawMessage) {
	detail := g.redactRaw(unwrapGrokExtParams(raw))
	approval := model.Approval{
		ID: model.NewID("approval"), Agent: g.cfg.Actor, Kind: "grok.planExit",
		Title: configuredParticipantName(g.cfg) + " requests permission to leave plan mode", Detail: detail,
		Status: "pending", RequestedAt: time.Now().UTC(),
	}
	g.mu.Lock()
	g.approvals[approval.ID] = grokPendingApproval{rawID: append(json.RawMessage(nil), id...), kind: "plan", approval: approval}
	g.mu.Unlock()
	e := runtimeEvent(g.cfg.Actor, model.RuntimeApprovalRequested)
	e.Approval = &approval
	g.attachCurrentTurn(&e)
	g.sink(e)
	g.setState(model.StateWaiting, "")
}

func (g *GrokAdapter) attachCurrentTurn(event *model.RuntimeEvent) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.turn == nil {
		return
	}
	event.TurnID = g.turn.turnID
	event.CorrelationID = g.turn.inputs[len(g.turn.inputs)-1].MessageID
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
		e.Text = detail
		g.sink(e)
		g.setState(model.StateError, detail)
	}
	close(done)
}

func lastGrokInputID(inputs []model.AgentInput) string {
	if len(inputs) == 0 {
		return ""
	}
	return inputs[len(inputs)-1].MessageID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
