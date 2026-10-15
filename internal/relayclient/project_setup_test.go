package relayclient

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/projectsetup"
)

// The GUI's project-only path must satisfy the actual bind/preflight hook
// predicate, not merely agree with its own status checker. No vendor is run.
func TestProjectSetupHooksSatisfyNativePreflight(t *testing.T) {
	t.Setenv("GROK_CLAUDE_HOOKS_ENABLED", "false")
	for _, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok, model.RuntimeGemini} {
		t.Run(string(kind), func(t *testing.T) {
			root := t.TempDir()
			status, err := projectsetup.Inspect(root)
			if err != nil {
				t.Fatal(err)
			}
			var revision string
			for _, runtime := range status.Runtimes {
				if runtime.Runtime == string(kind) {
					revision = runtime.Hook.Revision
				}
			}
			_, err = projectsetup.Install(root, projectsetup.Request{Runtime: string(kind), Component: "hook", Revision: revision})
			if err != nil {
				t.Fatal(err)
			}
			if err := installed(root, kind); err != nil {
				t.Fatalf("project hook does not satisfy native preflight: %v", err)
			}
		})
	}
}

// The inverse direction of the same invariant: a hand-written configuration the
// preflight refuses must never be reported as configured, and the confirmed
// repair the panel then offers has to be what bind accepts. A Gemini BeforeTool
// group with a wildcard matcher is the reachable case, because bind captures the
// caller only from its documented run_shell_command observation.
func TestProjectSetupStatusNeverClaimsAPreflightRefusalIsConfigured(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	const settings = `{"hooks":{"AfterAgent":[{"hooks":[{"type":"command","command":"pairroom relay hook --runtime gemini","timeout":45000}]}],"BeforeTool":[{"matcher":"*","hooks":[{"type":"command","command":"pairroom relay hook --runtime gemini","timeout":45000}]}]}}`
	if err := os.WriteFile(filepath.Join(root, ".gemini", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installed(root, model.RuntimeGemini); err == nil {
		t.Fatal("fixture no longer reproduces a configuration that bind refuses")
	}
	status, err := projectsetup.Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	var hook projectsetup.Item
	for _, runtime := range status.Runtimes {
		if runtime.Runtime == string(model.RuntimeGemini) {
			hook = runtime.Hook
		}
	}
	if hook.State == "current" || hook.State == "shared" {
		t.Fatalf("status %q claims readiness for a configuration the preflight refuses: %+v", hook.State, hook)
	}
	if !hook.CanInstall || !hook.NeedsConfirmation {
		t.Fatalf("no confirmed repair is offered: %+v", hook)
	}
	if _, err := projectsetup.Install(root, projectsetup.Request{Runtime: string(model.RuntimeGemini), Component: "hook", Revision: hook.Revision, ConfirmReplace: true}); err != nil {
		t.Fatal(err)
	}
	if err := installed(root, model.RuntimeGemini); err != nil {
		t.Fatalf("confirmed repair does not satisfy native preflight: %v", err)
	}
}
