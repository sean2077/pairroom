package relayclient

import (
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
