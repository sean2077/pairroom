package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/daemon"
	"github.com/sean2077/pairroom/internal/version"
)

func TestConfigureProcessLoggingExemptsRelayStdout(t *testing.T) {
	t.Setenv(daemon.LogFileEnvironment, filepath.Join(t.TempDir(), "relay-hijack.log"))
	stdout, stderr := os.Stdout, os.Stderr
	cleanup, err := configureProcessLogging([]string{"relay", "wait", "--timeout", "1"})
	if err != nil {
		t.Fatalf("configureProcessLogging(relay): %v", err)
	}
	if os.Stdout != stdout || os.Stderr != stderr {
		t.Fatal("relay subcommand must keep the original stdout/stderr for harness handoff")
	}
	if err := cleanup(); err != nil {
		t.Fatalf("relay cleanup: %v", err)
	}
}

func TestSubcommandHelpReturnsSuccess(t *testing.T) {
	for _, args := range [][]string{{"daemon", "--help"}, {"daemon", "install", "--help"}, {"daemon", "logs", "--help"}, {"service", "--help"}, {"serve", "--help"}, {"doctor", "--help"}, {"providers", "--help"}, {"verify", "--help"}, {"backup", "--help"}, {"restore", "--help"}, {"diagnostics", "--help"}, {"protocol", "--help"}, {"help"}, {"--help"}} {
		if err := run(args); err != nil {
			t.Fatalf("run(%q) returned %v", args, err)
		}
	}
}

func TestUnknownCommandReturnsError(t *testing.T) {
	if err := run([]string{"unknown"}); err == nil {
		t.Fatal("unknown command must fail")
	}
}

func TestVersionJSON(t *testing.T) {
	if err := run([]string{"version", "--json"}); err != nil {
		t.Fatalf("version --json: %v", err)
	}
}

func TestVersionSummaryIncludesGitMetadata(t *testing.T) {
	originalCommit, originalLastTag, originalCommits := version.Commit, version.LastTag, version.CommitsSinceTag
	t.Cleanup(func() {
		version.Commit, version.LastTag, version.CommitsSinceTag = originalCommit, originalLastTag, originalCommits
	})
	version.Commit = "44b6a7a1234567890abcdef1234567890abcdef12"
	version.LastTag = "v1.1.0"
	version.CommitsSinceTag = "8"
	want := "pairroom v1.1.0+8.44b6a7a"
	if got := versionSummary(); got != want {
		t.Fatalf("versionSummary()=%q want %q", got, want)
	}
	version.CommitsSinceTag = "1"
	want = "pairroom v1.1.0+1.44b6a7a"
	if got := versionSummary(); got != want {
		t.Fatalf("versionSummary()=%q want %q", got, want)
	}
}

func TestVersionSummaryFallsBackToBareVersion(t *testing.T) {
	originalCommit, originalLastTag, originalCommits := version.Commit, version.LastTag, version.CommitsSinceTag
	t.Cleanup(func() {
		version.Commit, version.LastTag, version.CommitsSinceTag = originalCommit, originalLastTag, originalCommits
	})
	version.Commit = "dev"
	version.LastTag = "unknown"
	version.CommitsSinceTag = "unknown"
	want := "pairroom v" + version.Current
	if got := versionSummary(); got != want {
		t.Fatalf("versionSummary()=%q want %q", got, want)
	}
}

func TestBrowserURLUsesFragmentBootstrapToken(t *testing.T) {
	value := browserURL("0.0.0.0:7332", "top-secret")
	if value != "http://127.0.0.1:7332/#token=top-secret" {
		t.Fatalf("browserURL=%q", value)
	}
	if got := browserURL("127.0.0.1:7332", ""); got != "http://127.0.0.1:7332/" {
		t.Fatalf("tokenless browserURL=%q", got)
	}
}

func TestServiceRejectsInvalidCapacityBeforeOpeningRegistry(t *testing.T) {
	for _, args := range [][]string{
		{"service", "--runtime-limit=0", "--no-browser"},
		{"service", "--idle-timeout=0s", "--no-browser"},
	} {
		if err := run(args); err == nil {
			t.Fatalf("run(%q) succeeded", args)
		}
	}
}

func TestWebCommandsRejectNonLoopbackBeforeOpeningState(t *testing.T) {
	commands := map[string][]string{
		"service": {"--data-root=relative"},
		"serve":   {"--repo=missing-repository"},
	}
	for command, invalidStateArgs := range commands {
		for _, address := range []string{"0.0.0.0:7332", "[::]:7332", "192.168.1.20:7332", "pairroom.local:7332", "localhost:7332"} {
			args := append([]string{command, "--listen=" + address, "--no-browser"}, invalidStateArgs...)
			err := run(args)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "loopback") {
				t.Fatalf("%s accepted non-loopback listen %q: %v", command, address, err)
			}
		}
	}
}

func TestLoopbackListenAcceptsOnlyNumericLoopback(t *testing.T) {
	for _, address := range []string{"127.0.0.1:7332", "127.0.0.2:7332", "[::1]:7332"} {
		if !isLoopbackListen(address) {
			t.Errorf("expected loopback listen %q", address)
		}
	}
	for _, address := range []string{"", ":7332", "0.0.0.0:7332", "[::]:7332", "localhost:7332", "192.168.1.20:7332", "pairroom.local:7332"} {
		if isLoopbackListen(address) {
			t.Errorf("unexpected loopback listen %q", address)
		}
	}
}

func TestSingleDashHelpIsHelp(t *testing.T) {
	for _, args := range [][]string{{"-help"}, {"daemon", "-help"}} {
		if err := run(args); err != nil {
			t.Fatalf("run(%q) returned %v", args, err)
		}
	}
}

func runMainProcess(t *testing.T, env []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, args...)
	command.Env = append(append(os.Environ(), mainEnvironment+"=1"), env...)
	var out, errOut bytes.Buffer
	command.Stdout, command.Stderr = &out, &errOut
	err = command.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), errOut.String(), exitErr.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), errOut.String(), 0
}

func TestMainExitCodesAndErrorPrefix(t *testing.T) {
	stdout, stderr, code := runMainProcess(t, nil, "version")
	if code != 0 || stdout != versionSummary()+"\n" || stderr != "" {
		t.Fatalf("version: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	stdout, stderr, code = runMainProcess(t, nil, "no-such-command")
	if code != 1 || stdout != "" || stderr != "pairroom: unknown command \"no-such-command\" (use pairroom help)\n" {
		t.Fatalf("unknown command: code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestMainRejectsInvalidLoggingEnvironmentBeforeRunning(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "service.log")
	stdout, stderr, code := runMainProcess(t, []string{daemon.LogFileEnvironment + "=" + logPath, daemon.LogBackupEnvironment + "=0"}, "version")
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "pairroom: configure daemon logging: invalid log backup count") {
		t.Fatalf("invalid logging env: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("invalid logging configuration created the log: %v", err)
	}
}

// A harness that exports PAIRROOM_LOG_FILE must still receive relay output on the
// original stdout/stderr, while every other command writes to the log instead.
func TestMainKeepsRelayStdoutOutOfTheDaemonLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "service.log")
	env := []string{daemon.LogFileEnvironment + "=" + logPath, daemon.ConsoleDetachEnvironment + "="}
	stdout, stderr, code := runMainProcess(t, env, "version")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("logged version: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if data, err := os.ReadFile(logPath); err != nil || string(data) != versionSummary()+"\n" {
		t.Fatalf("daemon log = %q, %v", data, err)
	}
	_, stderr, code = runMainProcess(t, env, "relay")
	if code != 1 || !strings.Contains(stderr, "pairroom: use pairroom relay") {
		t.Fatalf("relay diagnostics were redirected: code %d stderr %q", code, stderr)
	}
	if data, err := os.ReadFile(logPath); err != nil || strings.Contains(string(data), "relay") {
		t.Fatalf("relay output reached the daemon log: %q, %v", data, err)
	}
}
