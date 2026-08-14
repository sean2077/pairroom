package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Listen != "127.0.0.1:7332" || cfg.RoutingMode != model.RoutingMentions || cfg.MaxAgentHops != 6 {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if cfg.Claude.Command != "claude" || cfg.Codex.Command != "codex" {
		t.Fatalf("unexpected runtime commands: %#v", cfg)
	}
	if cfg.Codex.ApprovalPolicy != "untrusted" {
		t.Fatalf("unexpected Codex approval policy: %q", cfg.Codex.ApprovalPolicy)
	}
}

func TestLoadMergesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairroom.json")
	if err := os.WriteFile(path, []byte(`{
  "listen": "127.0.0.1:8000",
  "room_name": "Test Room",
  "routing_mode": "roundtable",
  "max_agent_hops": 4,
  "auto_start": false,
  "claude": {"command": "claude", "model": "opus"},
  "codex": {"command": "codex", "model": "gpt", "effort": "medium"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RoomName != "Test Room" || cfg.RoutingMode != model.RoutingRoundtable || cfg.Claude.Model != "opus" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if cfg.Claude.PermissionMode != "auto" || cfg.Codex.ApprovalPolicy != "untrusted" {
		t.Fatalf("defaults were not preserved: %#v", cfg)
	}
}

func TestLoadRejectsUnknownAndInvalid(t *testing.T) {
	tests := []string{
		`{"listen":"127.0.0.1:1","routing_mode":"mentions","max_agent_hops":4,"auto_start":true,"claude":{"command":"claude"},"codex":{"command":"codex"},"surprise":true}`,
		`{"listen":"127.0.0.1:1","routing_mode":"forever","max_agent_hops":4,"auto_start":true,"claude":{"command":"claude"},"codex":{"command":"codex"}}`,
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
