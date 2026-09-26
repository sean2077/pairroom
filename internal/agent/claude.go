package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/execx"
	"github.com/sean2077/pairroom/internal/model"
)

type ClaudeAdapter struct {
	cfg  Config
	sink EventSink

	startMu   sync.Mutex
	submitMu  sync.Mutex
	mu        sync.Mutex
	writeMu   sync.Mutex
	controlMu sync.Mutex
	state     model.AgentState
	sessionID string
	resume    bool
	cmd       *exec.Cmd
	tree      *execx.Tree
	stdin     io.WriteCloser
	// procDone is closed once the process exited and waitProcess finished; it
	// gives Stop a bounded graceful window between closing stdin and Kill.
	procDone     chan struct{}
	pending      []claudePending
	output       strings.Builder
	fallback     string
	flags        map[string]bool
	runtimeInfo  model.RuntimeInfo
	protocolSent bool
	intentional  bool
	// streamFailure records why the adapter killed the process after its
	// stdout became unreadable; waitProcess reports it with the exit.
	streamFailure string
	access        model.NativeAccess
	baseMode      string
	approvals     map[string]claudeApprovalRequest
	control       map[string]chan claudeControlResult
	controlReady  bool
}

func NewClaude(cfg Config, sink EventSink) *ClaudeAdapter {
	if !cfg.Actor.ValidParticipant() {
		cfg.Actor = model.ActorSlot1
	}
	if cfg.Command == "" {
		cfg.Command = "claude"
	}
	resume := cfg.SessionID != ""
	sessionID := cfg.SessionID
	if sessionID == "" {
		sessionID = newUUID()
	}
	return &ClaudeAdapter{
		cfg: cfg, sink: sink, state: model.StateStopped,
		sessionID: sessionID, resume: resume,
		baseMode: cfg.PermissionMode, approvals: make(map[string]claudeApprovalRequest),
		control: make(map[string]chan claudeControlResult),
	}
}

func (c *ClaudeAdapter) Actor() model.ActorID { return c.cfg.Actor }

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
	e := runtimeEvent(c.cfg.Actor, model.RuntimeState)
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
	c.streamFailure = ""
	c.protocolSent = false
	c.mu.Unlock()
	c.controlMu.Lock()
	c.controlReady = false
	c.controlMu.Unlock()

	probe, probeErr := ProbeRuntime(ctx, Config{
		Actor: c.cfg.Actor, Command: c.cfg.Command, Model: c.cfg.Model,
		Runtime: c.cfg.Runtime, PermissionMode: c.cfg.PermissionMode,
	})
	info := model.RuntimeInfo{
		Available: false, Command: c.cfg.Command, Protocol: "claude-stream-json",
		RuntimeKind: c.cfg.Runtime.CanonicalForSlot(c.cfg.Actor),
		Provider:    c.cfg.Provider, ProviderName: c.cfg.ProviderName,
		Model: c.cfg.Model, Effort: c.cfg.Effort, PermissionMode: c.cfg.PermissionMode, ProbedAt: time.Now().UTC(),
	}
	flags := map[string]bool{}
	batchLauncher := false
	if probeErr == nil {
		info = probe.RuntimeInfo(c.cfg)
		flags = probe.SupportedFlags
		batchLauncher = isBatchLauncher(goruntime.GOOS, probe.Path)
		if batchLauncher {
			// The session name is display metadata derived from the Room name;
			// neutralize it rather than refusing a Room called "R&D".
			info.SessionName = cmdSafeDisplayText(info.SessionName)
		}
	} else {
		info.Warnings = []string{probeErr.Error()}
	}
	if info.SessionName != "" {
		info.SessionNameStatus = "unsupported"
		if flags["--name"] {
			info.SessionNameStatus = "configured"
		}
	}
	c.mu.Lock()
	c.flags = flags
	c.runtimeInfo = info
	c.mu.Unlock()
	emitRuntimeInfo(c.sink, c.cfg.Actor, info)
	if probeErr != nil {
		c.setState(model.StateError, probeErr.Error())
		return probeErr
	}

	systemPrompt := collaborationPrompt(c.cfg)

	args := append([]string(nil), c.cfg.CommandArgs...)
	args = append(args, "-p", "--input-format", "stream-json", "--output-format", "stream-json")
	if info.SessionName != "" && flags["--name"] {
		args = append(args, "--name="+info.SessionName)
	}
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
	args = appendClaudePermissionArgs(args, flags, c.cfg.PermissionMode)
	c.mu.Lock()
	access := c.access
	c.mu.Unlock()
	if access == model.NativeAccessReadOnly && flags["--disallowedTools"] {
		// Plan mode prevents direct execution, while explicit deny rules remove
		// the native write tools and ExitPlanMode from the read-only context. Bash
		// remains available behind Claude's permission flow so the human can allow
		// a genuinely read-only inspection command when useful.
		args = append(args, "--disallowedTools", strings.Join([]string{"Edit", "Write", "NotebookEdit", "ExitPlanMode"}, ","))
	}
	if flags["--model"] && c.cfg.Model != "" {
		args = append(args, "--model", c.cfg.Model)
	}
	if flags["--effort"] && c.cfg.Effort != "" {
		args = append(args, "--effort", c.cfg.Effort)
	}

	c.mu.Lock()
	expectedSession := c.sessionID
	strictResume := c.resume && expectedSession != ""
	if c.resume && flags["--resume"] {
		args = append(args, "--resume="+c.sessionID)
	} else if !c.resume && flags["--session-id"] {
		args = append(args, "--session-id="+c.sessionID)
	} else if c.resume && !flags["--resume"] {
		c.mu.Unlock()
		err := fmt.Errorf("Claude Code cannot resume required session %q because this CLI does not expose --resume", expectedSession)
		c.setState(model.StateError, err.Error())
		return err
	}
	c.mu.Unlock()
	if batchLauncher {
		if err := checkBatchLauncherArgs("Claude Code", probe.Path, args); err != nil {
			c.setState(model.StateError, err.Error())
			return err
		}
	}

	cmd := exec.Command(c.cfg.Command, args...)
	execx.NoConsole(cmd)
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
	tree, err := execx.StartTree(cmd)
	if err != nil {
		_ = stdin.Close()
		c.setState(model.StateError, err.Error())
		return fmt.Errorf("start claude: %w", err)
	}

	procDone := make(chan struct{})
	c.mu.Lock()
	c.cmd = cmd
	c.tree = tree
	c.stdin = stdin
	c.procDone = procDone
	c.resume = true
	c.mu.Unlock()

	// cmd.Wait closes the pipes; both readers must finish draining before Wait
	// so a final stdout record (result/turn JSON) is never lost to the race.
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); c.readStdout(stdout) }()
	go func() { defer readers.Done(); c.readStderr(stderr) }()
	go func() { readers.Wait(); c.waitProcess(cmd); tree.Release(); close(procDone) }()

	initCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	initErr := c.initializeControl(initCtx)
	cancel()
	if initErr != nil {
		detail := "initialize Claude control protocol: " + initErr.Error()
		c.mu.Lock()
		if c.cmd == cmd {
			c.intentional = true
			c.cmd = nil
			c.tree = nil
			c.stdin = nil
		}
		c.mu.Unlock()
		_ = stdin.Close()
		_ = tree.Kill()
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
	session := runtimeEvent(c.cfg.Actor, model.RuntimeSession)
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
	// Both slots share one Room DataDir and either slot may select Claude. The
	// prompt file name carries the durable actor identity, and the write is
	// staged and renamed, so a concurrent Start for the peer slot can never
	// serve this slot's CLI the wrong bootstrap or a half-written file.
	actor := string(c.cfg.Actor)
	if !c.cfg.Actor.ValidParticipant() {
		actor = "unassigned"
	}
	path := filepath.Join(dir, "claude-pairroom-prompt-"+actor+".md")
	tmp, err := os.CreateTemp(dir, "claude-pairroom-prompt-"+actor+"-*.tmp")
	if err != nil {
		return "", fmt.Errorf("stage claude system prompt: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write([]byte(content + "\n")); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write claude system prompt: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close claude system prompt: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return "", fmt.Errorf("replace claude system prompt: %w", err)
	}
	return path, nil
}

func (c *ClaudeAdapter) waitProcess(cmd *exec.Cmd) {
	err := cmd.Wait()
	c.mu.Lock()
	active := c.cmd == cmd
	intentional := c.intentional
	streamFailure := c.streamFailure
	if active {
		c.streamFailure = ""
		c.cmd = nil
		c.tree = nil
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
	if streamFailure != "" {
		detail = streamFailure
	} else if err != nil {
		detail += ": " + err.Error()
	}
	for _, item := range pending {
		c.emitInputState(item, model.RuntimeInputFailed, model.ProcessingFailed, detail)
		completed := runtimeEvent(c.cfg.Actor, model.RuntimeTurnCompleted)
		completed.TurnID = item.turnID
		completed.CorrelationID = item.input.MessageID
		completed.Name = "process_exited"
		c.sink(completed)
	}
	if err != nil || streamFailure != "" || len(pending) > 0 {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeError)
		e.Name = "adapter.process_exited"
		e.Text = detail
		c.sink(e)
		c.setState(model.StateError, detail)
		return
	}
	c.setState(model.StateStopped, "")
}

func (c *ClaudeAdapter) Interrupt(ctx context.Context) error {
	c.mu.Lock()
	cmd := c.cmd
	tree := c.tree
	stdin := c.stdin
	procDone := c.procDone
	c.stdin = nil
	c.intentional = true
	c.mu.Unlock()
	c.clearApprovals()
	c.failControlWaiters(errors.New("Claude Code was interrupted"))
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		// Windows cannot deliver os.Interrupt to a child; go straight to the
		// process-tree kill there instead of waiting out the graceful window.
		if err := cmd.Process.Signal(os.Interrupt); err == nil {
			waitGracefulExit(ctx, procDone)
		}
		// Settling the pending Turn releases the Room owner, so it waits for
		// the whole vendor process tree to exit rather than for the signal to
		// be sent.
		if err := stopProcessTree(tree, procDone, "Claude Code"); err != nil {
			return err
		}
		c.forgetProcess(cmd)
	}
	c.cancelPending("interrupted", "interrupted by user")
	c.setState(model.StateStopped, "interrupted; next message resumes the Claude session")
	return nil
}

func (c *ClaudeAdapter) Stop(ctx context.Context) error {
	c.mu.Lock()
	cmd := c.cmd
	tree := c.tree
	stdin := c.stdin
	procDone := c.procDone
	c.stdin = nil
	c.intentional = true
	c.mu.Unlock()
	c.clearApprovals()
	c.failControlWaiters(errors.New("Claude Code was stopped"))
	if stdin != nil {
		_ = stdin.Close()
	}
	waitGracefulExit(ctx, procDone)
	if cmd != nil {
		// The process stays recorded until its whole tree has exited, so a
		// failed stop can be retried instead of freeing capacity while the real
		// CLI behind a launcher shim keeps running.
		if err := stopProcessTree(tree, procDone, "Claude Code"); err != nil {
			return err
		}
		c.forgetProcess(cmd)
	}
	c.cancelPending("stopped", "Claude Code was stopped")
	c.setState(model.StateStopped, "")
	return nil
}

// failStream stops a Claude process whose stdout can no longer be read. The
// exit is then reported through waitProcess like any unexpected exit, so
// pending input fails and the Turn owner is released on real exit.
func (c *ClaudeAdapter) failStream(reason string) {
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

// forgetProcess clears the process record once its tree is confirmed exited;
// waitProcess normally clears it first.
func (c *ClaudeAdapter) forgetProcess(cmd *exec.Cmd) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == cmd {
		c.cmd, c.tree, c.stdin, c.procDone = nil, nil, nil, nil
	}
}

// waitGracefulExit gives the vendor CLI a bounded window to flush its own
// session state after stdin closed, before the hard kill. A CLI that ignores
// the closed stdin is killed after the window; the caller's context still wins.
func waitGracefulExit(ctx context.Context, procDone <-chan struct{}) {
	if procDone == nil {
		return
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-procDone:
	case <-timer.C:
	case <-ctx.Done():
	}
}
