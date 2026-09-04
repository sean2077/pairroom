package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestExtractSemanticVersion(t *testing.T) {
	cases := map[string]string{
		"2.1.231 (Claude Code)": "2.1.231",
		"codex-cli 0.42.0":      "0.42.0",
		"v1.2.3-beta.1":         "1.2.3-beta.1",
		"unknown":               "",
	}
	for input, want := range cases {
		if got := extractSemanticVersion(input); got != want {
			t.Fatalf("extractSemanticVersion(%q)=%q want %q", input, got, want)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, tc := range []struct {
		value               string
		major, minor, patch int
		want                bool
	}{
		{"2.1.211", 2, 1, 211, true},
		{"2.1.231", 2, 1, 211, true},
		{"2.1.210", 2, 1, 211, false},
		{"3.0.0", 2, 9, 9, true},
		{"unknown", 2, 1, 211, false},
	} {
		if got := versionAtLeast(tc.value, tc.major, tc.minor, tc.patch); got != tc.want {
			t.Fatalf("versionAtLeast(%q)=%v want %v", tc.value, got, tc.want)
		}
	}
}

func TestProbeClaudeNegotiatesOptionalFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	path := writeProbeFixture(t, `#!/bin/sh
case "$1" in
  --version) echo "2.1.210 (Claude Code)" ;;
  --help) echo "--input-format --output-format --resume --session-id --model --effort --permission-mode --verbose" ;;
  *) exit 2 ;;
esac
`)
	probe, err := ProbeRuntime(context.Background(), Config{Actor: model.ActorClaude, Command: path})
	if err != nil {
		t.Fatal(err)
	}
	if !probe.SupportedFlags["--resume"] || !probe.SupportedFlags["--effort"] || !probe.SupportedFlags["--permission-prompt-tool"] || probe.SupportedFlags["--include-partial-messages"] {
		t.Fatalf("unexpected negotiated flags: %#v", probe.SupportedFlags)
	}
	if containsString(probe.Capabilities, "partial-messages") {
		t.Fatalf("unsupported capability was advertised: %#v", probe.Capabilities)
	}
	if len(probe.Warnings) == 0 {
		t.Fatal("expected subagent compatibility warning")
	}
}

func TestProbeClaudeDoesNotTreatHelpAsExhaustive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	path := writeProbeFixture(t, `#!/bin/sh
case "$1" in
  --version) echo "1.0.0" ;;
  --help) echo "--output-format" ;;
  *) exit 2 ;;
esac
`)
	probe, err := ProbeRuntime(context.Background(), Config{Actor: model.ActorClaude, Command: path})
	if err != nil {
		t.Fatalf("incomplete --help must not reject a documented protocol: %v", err)
	}
	if !probe.SupportedFlags["--input-format"] || !probe.SupportedFlags["--output-format"] {
		t.Fatalf("required stream-json flags were not retained: %#v", probe.SupportedFlags)
	}
}

func TestProbeClaudeDefersApprovalCapabilityToNativeHandshake(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	path := writeProbeFixture(t, `#!/bin/sh
case "$1" in
  --version) echo "2.1.231 (Claude Code)" ;;
  --help) echo "--input-format --output-format --resume --session-id --model --effort --permission-mode --disallowedTools --include-partial-messages --forward-subagent-text --verbose" ;;
  *) exit 2 ;;
esac
`)
	probe, err := ProbeRuntime(context.Background(), Config{Actor: model.ActorClaude, Command: path})
	if err != nil {
		t.Fatal(err)
	}
	if !probe.SupportedFlags["--disallowedTools"] {
		t.Fatalf("current Claude probe omitted reviewer deny rules: %#v", probe.SupportedFlags)
	}
	if !containsString(probe.Capabilities, "control-handshake-pending") {
		t.Fatalf("native control handshake capability omitted: %#v", probe.Capabilities)
	}
	if containsString(probe.Capabilities, "interactive-approvals") {
		t.Fatalf("approval capability must be advertised only after the native initialize handshake: %#v", probe.Capabilities)
	}
}

func TestProbeCodexRequiresAppServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	path := writeProbeFixture(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli 0.42.0"; exit 0; fi
if [ "$1" = "app-server" ] && [ "$2" = "--help" ]; then echo "app-server help"; exit 0; fi
exit 2
`)
	probe, err := ProbeRuntime(context.Background(), Config{Actor: model.ActorCodex, Command: path})
	if err != nil {
		t.Fatal(err)
	}
	if probe.Protocol != "codex-app-server-jsonrpc" || !containsString(probe.Capabilities, "turn-steer") {
		t.Fatalf("unexpected probe: %#v", probe)
	}
}

func writeProbeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-fixture")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
