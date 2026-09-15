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

// harnessRuntimes also recognizes unsupported Native runtimes so their tools
// cannot accidentally select an outer Claude/Codex session's relay binding.
var harnessRuntimes = map[string]model.RuntimeKind{
	"claude": model.RuntimeClaude,
	"codex":  model.RuntimeCodex,
	"grok":   model.RuntimeGrok,
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
