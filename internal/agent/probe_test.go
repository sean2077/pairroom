package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		{"2.1.211-rc.1", 2, 1, 211, false},
		{"2.1.211+build-1", 2, 1, 211, true},
		{"2.1.211-rc.1+build.2", 2, 1, 211, false},
		{"2.1.212-rc.1", 2, 1, 211, true},
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
	probe, err := ProbeRuntime(context.Background(), Config{Actor: model.ActorSlot1, Command: path})
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
	probe, err := ProbeRuntime(context.Background(), Config{Actor: model.ActorSlot1, Command: path})
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
	probe, err := ProbeRuntime(context.Background(), Config{Actor: model.ActorSlot1, Command: path})
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
	probe, err := ProbeRuntime(context.Background(), Config{Actor: model.ActorSlot2, Command: path})
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

func TestHelpAdvertisesOnlyCompleteFlags(t *testing.T) {
	for _, tc := range []struct {
		help, flag string
		want       bool
	}{
		{"--acp-debug --acp_version --acp2 --acp.extra", "--acp", false},
		{"--resume-session --resume_legacy --resume2", "--resume", false},
		{"prefix--acp --acpé", "--acp", false},
		{"--acp-debug, --acp=<value>", "--acp", true},
		{"[--resume]", "--resume", true},
		{"--append-system-prompt[-file]-debug", "--append-system-prompt-file", false},
		{"(--append-system-prompt[-file]),", "--append-system-prompt-file", true},
	} {
		if got := helpAdvertisesFlag(tc.help, tc.flag); got != tc.want {
			t.Errorf("helpAdvertisesFlag(%q, %q) = %v, want %v", tc.help, tc.flag, got, tc.want)
		}
	}
}

func TestProbeGeminiRejectsACPFlagPrefixes(t *testing.T) {
	t.Setenv("PAIRROOM_GEMINI_HELPER", "1")
	t.Setenv("PAIRROOM_GEMINI_MODE", "acp-prefix")
	_, err := ProbeRuntime(context.Background(), Config{Actor: model.ActorSlot2, Runtime: model.RuntimeGemini, Command: os.Args[0]})
	if err == nil || !strings.Contains(err.Error(), "does not advertise ACP") {
		t.Fatalf("similar option passed ACP preflight: %v", err)
	}
}

func TestProbeClaudeResumeEvidencePreservesExistingSession(t *testing.T) {
	t.Setenv("PAIRROOM_CLAUDE_SCRIPT", "success")
	t.Setenv("PAIRROOM_CLAUDE_SCRIPT_HELP", "--input-format --output-format --resume-session --session-id --verbose")
	pidFile := filepath.Join(t.TempDir(), "runtime.pid")
	t.Setenv("PAIRROOM_HELPER_PID_FILE", pidFile)
	cfg := Config{Actor: model.ActorSlot1, Runtime: model.RuntimeClaude, Command: os.Args[0], Repo: t.TempDir(), DataDir: t.TempDir(), SessionID: "retained-session", RequireExactSession: true}
	probe, err := ProbeRuntime(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Verification != "cli_metadata_only" || probe.SupportedFlags["--resume"] || containsString(probe.Capabilities, "session-resume") {
		t.Fatalf("invalid resume evidence: %+v", probe)
	}
	warnings := strings.Join(probe.Warnings, "\n")
	if !strings.Contains(warnings, "existing sessions are preserved") || strings.Contains(warnings, "fresh session") {
		t.Fatalf("unsafe restore guidance: %s", warnings)
	}
	adapter := NewClaude(cfg, func(model.RuntimeEvent) {})
	t.Cleanup(func() { _ = adapter.Stop(context.Background()) })
	if err := adapter.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot resume required session") {
		t.Fatalf("similar option passed exact-resume guard: %v", err)
	}
	if adapter.SessionID() != "retained-session" {
		t.Fatal("failed restore changed the session identity")
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("failed restore started a vendor process: %v", err)
	}
}
