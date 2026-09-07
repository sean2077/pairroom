package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func loadTestConfig(t *testing.T, data string) (File, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pairroom.json")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestRuntimeSwitchPreservesExplicitNativePolicies(t *testing.T) {
	for _, tt := range []struct {
		name, document                string
		slot                          model.ActorID
		permission, approval, sandbox string
	}{
		{"second-claude-inherit", `{"codex":{"runtime":"claude","permission_mode":""}}`, model.ActorCodex, "", "", ""},
		{"second-grok-strict", `{"codex":{"runtime":"grok","permission_mode":"ask","sandbox":"strict"}}`, model.ActorCodex, "ask", "", "strict"},
		{"first-codex-inherit", `{"claude":{"runtime":"codex","approval_policy":"","sandbox":""}}`, model.ActorClaude, "", "", ""},
		{"first-codex-restricted", `{"claude":{"runtime":"codex","approval_policy":"on-request","sandbox":"workspace-write"}}`, model.ActorClaude, "", "on-request", "workspace-write"},
		{"first-codex-read-only", `{"claude":{"runtime":"codex","sandbox":"read-only"}}`, model.ActorClaude, "", "yolo", "read-only"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadTestConfig(t, tt.document)
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.DefaultSelections()[tt.slot]
			if got.PermissionMode != tt.permission || got.ApprovalPolicy != tt.approval || got.Sandbox != tt.sandbox {
				t.Fatalf("policy = (%q, %q, %q); want (%q, %q, %q)", got.PermissionMode, got.ApprovalPolicy, got.Sandbox, tt.permission, tt.approval, tt.sandbox)
			}
		})
	}
}

func TestConfigRequiresOneObject(t *testing.T) {
	for _, data := range []string{`{} {}`, `{} trailing`, `null`, `[]`} {
		t.Run(data, func(t *testing.T) {
			if _, err := loadTestConfig(t, data); err == nil {
				t.Fatalf("accepted %q", data)
			}
		})
	}
}

func TestRuntimeTemplateRejectsInlinePerRoomOverrides(t *testing.T) {
	for _, args := range [][]string{
		{`--config=approval_policy="never"`},
		{`-csandbox_mode="danger-full-access"`},
		{`-c=model_provider="other"`},
		{`--config=model="other"`},
		{`-mother`},
		{`-c`, `model_reasoning_effort="high"`},
	} {
		t.Run(args[0], func(t *testing.T) {
			cfg := Defaults()
			cfg.Runtimes.Codex.Args = args
			if err := cfg.Validate(); err == nil {
				t.Fatalf("accepted per-Room overrides: %q", args)
			}
		})
	}
}

func TestCaseInsensitiveRuntimeFieldsAndAmbiguousPolicies(t *testing.T) {
	cfg, err := loadTestConfig(t, `{"CLAUDE":{"RUNTIME":"codex","APPROVAL_POLICY":"","SANDBOX":""}}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Claude.PermissionMode != "" || cfg.Claude.ApprovalPolicy != "" || cfg.Claude.Sandbox != "" {
		t.Fatalf("explicit native policy widened: %#v", cfg.Claude)
	}
	for _, input := range []string{
		`{"claude":{},"CLAUDE":{"runtime":"codex"}}`,
		`{"claude":{"runtime":"claude","runtime":"codex"}}`,
		`{"claude":{"runtime":"claude","Permission_Mode":null}}`,
		`{"codex":{"sandbox":null}}`,
		`{"codex":null}`,
		`{"runtimes":{"claude":{"command":"claude\u0000other"}}}`,
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := loadTestConfig(t, input); err == nil {
				t.Fatalf("ambiguous config accepted: %s", input)
			}
		})
	}
}
