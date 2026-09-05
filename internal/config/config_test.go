package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Listen != "127.0.0.1:7332" || cfg.StallWarningSeconds != 300 || !cfg.AutoStart {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if cfg.Runtimes.Claude.Command != "claude" || cfg.Runtimes.Codex.Command != "codex" || cfg.Runtimes.Grok.Command != "grok" {
		t.Fatalf("unexpected runtime commands: %#v", cfg)
	}
	if cfg.Claude.Runtime != "claude" || cfg.Codex.Runtime != "codex" {
		t.Fatalf("unexpected default runtimes: %#v", cfg)
	}
	if cfg.Claude.PermissionMode != "yolo" || cfg.Codex.ApprovalPolicy != "yolo" || cfg.Codex.Sandbox != "" {
		t.Fatalf("runtime-policy defaults must use yolo: %#v %#v", cfg.Claude, cfg.Codex)
	}
}

func TestLoadMergesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairroom.json")
	if err := os.WriteFile(path, []byte(`{
  "listen": "127.0.0.1:8000",
  "room_name": "Test Room",
  "auto_start": false,
  "claude": {"model": "opus"},
  "codex": {"model": "gpt", "effort": "medium"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RoomName != "Test Room" || cfg.Claude.Model != "opus" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if cfg.Claude.PermissionMode != "yolo" || cfg.Codex.ApprovalPolicy != "yolo" {
		t.Fatalf("omitted policy fields should keep yolo defaults: %#v", cfg)
	}
}

func TestLoadRejectsUnknownAndInvalid(t *testing.T) {
	tests := []string{
		`{"listen":"127.0.0.1:1","auto_start":true,"claude":{},"codex":{},"surprise":true}`,
		`{"listen":"127.0.0.1:1","stall_warning_seconds":1,"auto_start":true,"claude":{},"codex":{}}`,
	}
	for i, data := range tests {
		path := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func TestLoadRejectsRemovedRoutingSettings(t *testing.T) {
	for _, field := range []string{`"routing_mode":"turns"`, `"max_agent_hops":4`} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pairroom.json")
			data := `{"listen":"127.0.0.1:1",` + field + `,"stall_warning_seconds":300,"auto_start":true,"claude":{"command":"claude"},"codex":{"command":"codex"}}`
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("removed setting %s was accepted", field)
			}
		})
	}
}

func TestLoadAcceptsGrokRuntimeAndIdenticalSlots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairroom.json")
	if err := os.WriteFile(path, []byte(`{
  "listen": "127.0.0.1:8000",
  "auto_start": true,
  "claude": {"runtime": "grok", "model": "", "effort": "", "instructions": "Be terse"},
  "codex": {"runtime": "grok"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Claude.RuntimeKind(model.ActorClaude) != model.RuntimeGrok || cfg.Codex.RuntimeKind(model.ActorCodex) != model.RuntimeGrok {
		t.Fatalf("expected two Grok slots: %#v", cfg)
	}
	if cfg.Claude.Model != "" || cfg.Claude.Effort != "" || cfg.Codex.Effort != "" {
		t.Fatalf("empty overrides must stay empty: %#v %#v", cfg.Claude, cfg.Codex)
	}
	if cfg.Claude.Instructions != "Be terse" {
		t.Fatalf("instructions dropped: %#v", cfg.Claude)
	}
}

func TestLoadRejectsUnknownRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairroom.json")
	if err := os.WriteFile(path, []byte(`{
  "listen": "127.0.0.1:1",
  "auto_start": true,
  "claude": {"runtime": "cursor"},
  "codex": {}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown runtime was accepted")
	}
}

func TestLoadRejectsRemovedProviderAndPerSlotCommandConfiguration(t *testing.T) {
	for _, data := range []string{
		`{"providers":[],"listen":"127.0.0.1:1"}`,
		`{"cc_connect":{},"listen":"127.0.0.1:1"}`,
		`{"claude":{"provider":"old-name"},"listen":"127.0.0.1:1"}`,
		`{"claude":{"command":"claude"},"listen":"127.0.0.1:1"}`,
	} {
		path := filepath.Join(t.TempDir(), "legacy.json")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("removed configuration was accepted: %s", data)
		} else {
			var migration *MigrationError
			if !errors.As(err, &migration) {
				t.Fatalf("error = %T %v, want MigrationError", err, err)
			}
		}
	}
}

func TestRuntimeTemplatesRejectPerRoomModelAndPolicyOverrides(t *testing.T) {
	tests := []struct {
		kind model.RuntimeKind
		args []string
	}{
		{model.RuntimeClaude, []string{"--dangerously-skip-permissions"}},
		{model.RuntimeCodex, []string{"-c", `sandbox_mode="danger-full-access"`}},
		{model.RuntimeGrok, []string{"--yolo"}},
	}
	for _, test := range tests {
		cfg := Defaults()
		switch test.kind {
		case model.RuntimeClaude:
			cfg.Runtimes.Claude.Args = test.args
		case model.RuntimeCodex:
			cfg.Runtimes.Codex.Args = test.args
		case model.RuntimeGrok:
			cfg.Runtimes.Grok.Args = test.args
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "AgentSelection") {
			t.Fatalf("%s args %v error = %v", test.kind, test.args, err)
		}
	}
}

func TestStallWarningConfiguration(t *testing.T) {
	cfg := Defaults()
	if cfg.StallWarningSeconds != 300 {
		t.Fatalf("unexpected default stall warning: %d", cfg.StallWarningSeconds)
	}
	cfg.StallWarningSeconds = -1
	if err := cfg.Validate(); err != nil {
		t.Fatalf("-1 should disable warnings: %v", err)
	}
	for _, invalid := range []int{-2, 1, 29, 86401} {
		cfg.StallWarningSeconds = invalid
		if err := cfg.Validate(); err == nil {
			t.Fatalf("stall_warning_seconds=%d should be rejected", invalid)
		}
	}
}
