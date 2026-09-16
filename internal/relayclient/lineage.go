package relayclient

import (
	"os"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

// procInfo is one platform process-table row.
type procInfo struct {
	ppid int
	name string
}

// processTable maps pid -> process info for the current platform. It is a
// variable so tests can stub the platform scan.
var processTable = platformProcessTable

// Recognize the nearest supported harness so inherited outer-session
// metadata cannot select another runtime's relay binding.
var harnessRuntimes = map[string]model.RuntimeKind{
	"claude": model.RuntimeClaude,
	"codex":  model.RuntimeCodex,
	"grok":   model.RuntimeGrok,
}

// sessionEnvVars maps a native runtime to the environment variable its official
// harness exposes to tool-call subprocesses carrying the current session id:
// Claude Code sets CLAUDE_CODE_SESSION_ID, and Codex sets CODEX_SESSION_ID
// (openai/codex codex-rs/core/src/exec_env.rs); Grok sets GROK_SESSION_ID.
// bind reads it to associate the official session immediately, without the
// former nonce echo round-trip. The approved Stop hook later reports the same
// id, which the Service re-checks.
var sessionEnvVars = map[model.RuntimeKind]string{
	model.RuntimeClaude: "CLAUDE_CODE_SESSION_ID",
	model.RuntimeCodex:  "CODEX_SESSION_ID",
	model.RuntimeGrok:   "GROK_SESSION_ID",
}

// sessionIDFromEnv returns the official session id the harness exposed to this
// subprocess, or "" when running outside a recognized native session for kind.
func sessionIDFromEnv(kind model.RuntimeKind) string {
	name, ok := sessionEnvVars[kind]
	if !ok {
		return ""
	}
	return os.Getenv(name)
}

// harnessAncestor walks the current process ancestry for a native harness.
// Lineage is a best-effort DEFAULT SELECTOR for foreground relay commands in
// multi-binding workspaces; it is never authentication material. Authorization
// still requires the owner-only slot credential, generation, and — on the hook
// path — the associated official session identity.
var harnessAncestor = findHarnessAncestor

func findHarnessAncestor() (int, string, bool) {
	table, err := processTable()
	if err != nil {
		return 0, "", false
	}
	pid := os.Getpid()
	for range 16 {
		entry, ok := table[pid]
		if !ok {
			return 0, "", false
		}
		name := strings.TrimSuffix(strings.ToLower(entry.name), ".exe")
		if _, isHarness := harnessRuntimes[name]; isHarness {
			return pid, name, true
		}
		if entry.ppid <= 0 || entry.ppid == pid {
			return 0, "", false
		}
		pid = entry.ppid
	}
	return 0, "", false
}
