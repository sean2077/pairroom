package relayclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/version"
)

// Stubbed by tests; production resolves the bare command the hooks run.
var (
	preflightLookPath   = exec.LookPath
	preflightExecutable = os.Executable
)

var errPreflightNotReady = errors.New("native setup is not ready; follow next_steps in the report")

const (
	checkPass = "pass"
	checkWarn = "warn"
	checkFail = "fail"
)

type preflightCLI struct {
	Status     string `json:"status"`
	Version    string `json:"version"`
	Executable string `json:"executable,omitempty"`
	OnPath     string `json:"on_path,omitempty"`
	SameBinary bool   `json:"same_binary"`
	Hint       string `json:"hint,omitempty"`
}

type preflightWorkspace struct {
	Status string `json:"status"`
	Root   string `json:"root,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

type preflightCaller struct {
	Status     string `json:"status"`
	Runtime    string `json:"runtime,omitempty"`
	SessionEnv string `json:"session_env,omitempty"`
	InSession  bool   `json:"in_session"`
	Bound      bool   `json:"bound"`
	Hint       string `json:"hint,omitempty"`
}

type preflightService struct {
	Status            string `json:"status"`
	EndpointPath      string `json:"endpoint_path,omitempty"`
	Version           string `json:"version,omitempty"`
	VersionMatch      bool   `json:"version_match"`
	ProjectRegistered bool   `json:"project_registered"`
	ActiveNativeRooms int    `json:"active_native_rooms"`
	Hint              string `json:"hint,omitempty"`
}

type preflightHook struct {
	Status   string `json:"status"`
	File     string `json:"file,omitempty"`
	Approval string `json:"approval"`
	Detail   string `json:"detail,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

type preflightReport struct {
	Ready     bool                     `json:"ready"`
	CLI       preflightCLI             `json:"cli"`
	Workspace preflightWorkspace       `json:"workspace"`
	Caller    preflightCaller          `json:"caller"`
	Service   preflightService         `json:"service"`
	Hooks     map[string]preflightHook `json:"hooks"`
	NextSteps []string                 `json:"next_steps"`
	Notice    string                   `json:"notice"`
}

// runPreflight reports whether this machine and workspace are ready for a
// Native bind. It is strictly read-only: no relay state, Project, Room or hook
// is created, no model is contacted, and the Service endpoint token is used
// only for one scoped read and never printed. Hook approval stays unknown;
// relay doctor's last_hook_at after a finished turn is the evidence.
func runPreflight(ctx context.Context, o options, out io.Writer) error {
	report := preflightReport{Hooks: map[string]preflightHook{}, Notice: "Read-only setup check. Contacts no model and changes nothing. Hook approval is decided in each harness and stays unknown here; after binding, relay doctor shows last_hook_at."}
	report.CLI = preflightCommandLine()
	report.Workspace = preflightGitWorkspace(ctx, o.repo)
	report.Caller = preflightNativeCaller(report.Workspace.Root)
	report.Service = preflightServiceState(ctx, o.endpoint, report.Workspace.Root)

	var selected []model.RuntimeKind
	switch {
	case o.kind != "":
		selected = []model.RuntimeKind{model.RuntimeKind(o.kind)}
	case report.Caller.Runtime != "":
		selected = []model.RuntimeKind{model.RuntimeKind(report.Caller.Runtime)}
	default:
		selected = []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok}
	}
	// A chosen runtime needs its own hook; with none chosen, any one installed
	// harness is enough to proceed.
	hooksReady := false
	if report.Workspace.Root != "" {
		for _, kind := range selected {
			hook := preflightHookState(report.Workspace.Root, kind)
			report.Hooks[string(kind)] = hook
			if hook.Status == "installed" {
				hooksReady = true
			}
		}
	}

	report.Ready = report.CLI.Status != checkFail && report.Workspace.Status == checkPass &&
		report.Caller.Status != checkFail && report.Service.Status != checkFail && hooksReady
	for _, hint := range []string{report.CLI.Hint, report.Workspace.Hint, report.Service.Hint} {
		if hint != "" {
			report.NextSteps = append(report.NextSteps, hint)
		}
	}
	if !hooksReady {
		for _, kind := range selected {
			if hint := report.Hooks[string(kind)].Hint; hint != "" {
				report.NextSteps = append(report.NextSteps, hint)
			}
		}
	}
	if report.Caller.Hint != "" {
		report.NextSteps = append(report.NextSteps, report.Caller.Hint)
	}
	if report.Ready && !report.Caller.Bound {
		if report.Service.ActiveNativeRooms == 0 {
			report.NextSteps = append(report.NextSteps, `Create and bind from inside the first Agent session: pairroom relay bind --create --name "<topic>" (skill: /pairroom-relay <topic>)`)
		} else {
			report.NextSteps = append(report.NextSteps, "Join from inside the Agent session with pairroom relay bind (pass --room when several native Rooms are active), or create another Room with bind --create")
		}
	}
	if report.NextSteps == nil {
		report.NextSteps = []string{}
	}
	if err := writeJSON(out, report); err != nil {
		return err
	}
	if !report.Ready {
		return errPreflightNotReady
	}
	return nil
}

func preflightCommandLine() preflightCLI {
	cli := preflightCLI{Status: checkPass, Version: version.Describe()}
	executable, err := preflightExecutable()
	if err == nil {
		cli.Executable = executable
	}
	resolved, err := preflightLookPath("pairroom")
	if err != nil {
		cli.Status = checkFail
		cli.Hint = "The bare pairroom command is not on this shell's PATH, but the relay hooks run exactly that. Add the CLI's directory to PATH, restart this Agent session, and rerun preflight."
		return cli
	}
	if absolute, err := filepath.Abs(resolved); err == nil {
		resolved = absolute
	}
	cli.OnPath = resolved
	if cli.Executable != "" {
		a, errA := os.Stat(cli.Executable)
		b, errB := os.Stat(resolved)
		cli.SameBinary = errA == nil && errB == nil && os.SameFile(a, b)
	}
	if !cli.SameBinary {
		cli.Status = checkWarn
		cli.Hint = "The pairroom on PATH is a different file from the one running now. Hooks run the PATH copy; make sure it comes from the same release as the Service."
	}
	return cli
}

func preflightGitWorkspace(ctx context.Context, repo string) preflightWorkspace {
	root, err := workspace(ctx, repo)
	if err != nil {
		return preflightWorkspace{Status: checkFail, Hint: "Run preflight inside the project's Git repository, or pass --repo <project>."}
	}
	return preflightWorkspace{Status: checkPass, Root: root}
}

func preflightNativeCaller(root string) preflightCaller {
	caller, err := currentNativeCaller()
	if err != nil {
		return preflightCaller{Status: checkFail, Hint: err.Error()}
	}
	result := preflightCaller{Status: checkPass, Runtime: string(caller.runtime), InSession: caller.session != ""}
	if caller.runtime != "" {
		result.SessionEnv = sessionEnvVars[caller.runtime]
	}
	if !result.InSession {
		result.Status = checkWarn
		result.Hint = "No native session identity here. That is fine for preflight, but bind must run as the Agent's own tool call inside its Claude Code, Codex or Grok Build session (not a plain terminal or Grok's ! shell)."
		return result
	}
	if root == "" {
		return result
	}
	paths, err := statePaths(root)
	if err != nil {
		return result
	}
	for _, path := range paths {
		var s State
		if readPrivate(path, &s) == nil && s.Schema == 2 && s.Generation != 0 && s.SessionID == caller.session && s.Runtime == caller.runtime {
			result.Bound = true
			result.Hint = "This session is already bound; use pairroom relay doctor instead of binding again."
			break
		}
	}
	return result
}

func preflightServiceState(ctx context.Context, endpointPath, root string) preflightService {
	result := preflightService{Status: checkFail}
	if endpointPath == "" {
		var err error
		if endpointPath, err = defaultEndpoint(); err != nil {
			result.Hint = "Cannot locate the user configuration directory for the Service endpoint; pass --service-file <data-root>/relay-endpoint.json."
			return result
		}
	}
	result.EndpointPath = endpointPath
	endpoint, err := relay.ReadEndpoint(endpointPath)
	if errors.Is(err, os.ErrNotExist) {
		result.Hint = "No running Service found. Open PairRoom Desktop, check pairroom daemon status, or start pairroom service in your own terminal. A custom data root needs --service-file <data-root>/relay-endpoint.json."
		return result
	}
	if err != nil {
		result.Hint = fmt.Sprintf("The Service endpoint file is unusable (%v). Restart the Service so it rewrites it.", err)
		return result
	}
	var snapshot struct {
		Version  string           `json:"version"`
		Projects []serviceProject `json:"projects"`
		Rooms    []serviceRoom    `json:"rooms"`
	}
	if err := management(ctx, endpoint, http.MethodGet, "/api/v1/service", nil, &snapshot); err != nil {
		result.Hint = "The endpoint file exists but the Service did not answer; it may have stopped uncleanly. Restart it, then rerun preflight."
		return result
	}
	result.Status = checkPass
	result.Version = snapshot.Version
	// Compare releases, not build metadata: a display version such as
	// v5.5.1+8.a5cb253 still belongs to release 5.5.1.
	release, _, _ := strings.Cut(strings.TrimPrefix(snapshot.Version, "v"), "+")
	result.VersionMatch = release == version.Current
	if !result.VersionMatch {
		result.Status = checkWarn
		result.Hint = "The Service runs a different PairRoom version from this CLI. Use the CLI from the Service's release, for example the one bundled with Desktop."
	}
	if root == "" {
		return result
	}
	for _, project := range snapshot.Projects {
		if !sameWorkspace(project.Root, root) {
			continue
		}
		result.ProjectRegistered = true
		for _, room := range snapshot.Rooms {
			if room.ProjectID == project.ID && room.HostMode == model.HostNative && room.Lifecycle == roomLifecycleActive {
				result.ActiveNativeRooms++
			}
		}
	}
	return result
}

func preflightHookState(root string, kind model.RuntimeKind) preflightHook {
	hook := preflightHook{Approval: "unknown"}
	if path, err := hookPath(root, kind); err == nil {
		hook.File = path
	}
	install := fmt.Sprintf("Install the %s hook from the project: pairroom relay install --runtime %s, then approve it in the harness (%s).", kind, kind, approvalPlace(kind))
	present, disabled, err := ownRelayStopHook(root, kind)
	if err == nil && !present && !disabled && kind == model.RuntimeGrok && grokReusesClaudeHooks() {
		present, disabled, err = ownRelayStopHook(root, model.RuntimeClaude)
		if present || disabled {
			if path, pathErr := hookPath(root, model.RuntimeClaude); pathErr == nil {
				hook.File = path
			}
			hook.Detail = "Grok Build reuses the Claude Code project hook"
		}
	}
	switch {
	case err != nil:
		hook.Status = "error"
		hook.Hint = fmt.Sprintf("Cannot read the %s hook configuration (%v); fix the file, then rerun preflight.", kind, err)
	case disabled:
		hook.Status = "disabled"
		hook.Hint = fmt.Sprintf("Hooks are disabled in %s; re-enable them, then approve the PairRoom Stop hook.", hook.File)
	case present:
		hook.Status = "installed"
	default:
		hook.Status = "missing"
		hook.Hint = install
	}
	return hook
}

func approvalPlace(kind model.RuntimeKind) string {
	switch kind {
	case model.RuntimeCodex:
		return "Codex: /hooks"
	case model.RuntimeGrok:
		return "Grok: /hooks, press r to reload, then folder trust"
	default:
		return "Claude Code: project hook consent"
	}
}
