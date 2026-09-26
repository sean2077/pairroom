package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/daemon"
	"github.com/sean2077/pairroom/internal/relay"
)

// occupiedLoopbackAddress returns a numeric loopback address whose port stays
// bound for the test, so a command's own listener fails deterministically.
func occupiedLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String()
}

type verifyJSON struct {
	DataDir    string         `json:"data_dir"`
	OK         bool           `json:"ok"`
	EventCount int            `json:"event_count"`
	RoomID     string         `json:"room_id"`
	EventKinds map[string]int `json:"event_kinds"`
	Errors     []string       `json:"errors"`
}

func verifyRoomData(t *testing.T, dataDir string) verifyJSON {
	t.Helper()
	output, err := captureRun(t, "verify", "--data-dir", dataDir, "--json")
	if err != nil {
		t.Fatalf("verify %s: %v\n%s", dataDir, err, output)
	}
	var report verifyJSON
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("verify --json: %v\n%s", err, output)
	}
	return report
}

func TestServeValidatesCollaborationBeforeOpeningRoomState(t *testing.T) {
	repo := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "room")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--collaboration", "custom"}, "custom collaboration instructions must not be empty"},
		{[]string{"--collaboration", "freeform"}, `invalid collaboration mode "freeform"`},
		{[]string{"--collaboration-instructions", "Do it my way"}, "default mode has fixed instructions"},
		{[]string{"--stall-warning-seconds", "5"}, "stall-warning-seconds must be -1 or between 30 and 86400"},
	} {
		args := append([]string{"serve", "--repo", repo, "--data-dir", dataDir, "--mock", "--no-browser"}, tc.args...)
		requireErrorContains(t, run(args), tc.want)
	}
	requireErrorContains(t, run([]string{"serve", "--repo", filepath.Join(repo, "missing"), "--data-dir", dataDir}), "stat repository")
	assertNotExist(t, dataDir)
}

func TestServeCreatesAVerifiableRoomThatBacksUpAndRestores(t *testing.T) {
	isolateUserDirs(t)
	repo := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "room")
	address := occupiedLoopbackAddress(t)
	output, err := captureRun(t, "serve", "--repo", repo, "--data-dir", dataDir, "--listen", address,
		"--mock", "--no-browser", "--token", "serve-secret", "--collaboration", "custom", "--collaboration-instructions", "Pair on every change.")
	if err == nil {
		t.Fatal("serve succeeded on an occupied listener")
	}
	for _, want := range []string{
		"  room: http://" + address + "/#token=serve-secret\n",
		"  repo: " + repo + "\n",
		"  data: " + dataDir + "\n",
		"  mode: mock\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("serve output missing %q:\n%s", want, output)
		}
	}
	report := verifyRoomData(t, dataDir)
	if !report.OK || report.RoomID == "" || report.EventKinds["room.created"] != 1 {
		t.Fatalf("serve left unverifiable Room data: %+v", report)
	}

	// Collaboration is fixed at creation: reopening with different explicit
	// rules must fail instead of rewriting the Room.
	err = run([]string{"serve", "--repo", repo, "--data-dir", dataDir, "--listen", address, "--mock", "--no-browser", "--collaboration", "default"})
	requireErrorContains(t, err, "collaboration is immutable")
	if after := verifyRoomData(t, dataDir); after.EventCount != report.EventCount {
		t.Fatalf("rejected reopen appended events: %d -> %d", report.EventCount, after.EventCount)
	}

	text, err := captureRun(t, "verify", "--data-dir", dataDir)
	if err != nil || !strings.Contains(text, "  result:      OK\n") {
		t.Fatalf("verify text: %v\n%s", err, text)
	}

	backup := filepath.Join(t.TempDir(), "room.tar.gz")
	text, err = captureRun(t, "backup", "--data-dir", dataDir, "--output", backup)
	if err != nil || !strings.HasPrefix(text, "PairRoom backup created\n  output: "+backup+"\n  files:  ") {
		t.Fatalf("backup: %v\n%s", err, text)
	}
	manifestText, err := captureRun(t, "backup", "--data-dir", dataDir, "--output", backup+".2", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Format string            `json:"format"`
		Files  []json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal([]byte(manifestText), &manifest); err != nil || manifest.Format == "" || len(manifest.Files) == 0 {
		t.Fatalf("backup --json: %v\n%s", err, manifestText)
	}

	restored := filepath.Join(t.TempDir(), "restored")
	text, err = captureRun(t, "restore", "--input", backup, "--data-dir", restored)
	want := "PairRoom backup restored\n  data:   " + restored + "\n  events: " + jsonNumber(report.EventCount) + "\n"
	if err != nil || text != want {
		t.Fatalf("restore: %v\n%q, want %q", err, text, want)
	}
	if again := verifyRoomData(t, restored); !again.OK || again.RoomID != report.RoomID || again.EventCount != report.EventCount {
		t.Fatalf("restored Room differs: %+v vs %+v", again, report)
	}
	requireErrorContains(t, run([]string{"restore", "--input", backup, "--data-dir", restored}), "force")
	restoredJSON, err := captureRun(t, "restore", "--input", backup, "--data-dir", restored, "--force", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var restoredReport verifyJSON
	if err := json.Unmarshal([]byte(restoredJSON), &restoredReport); err != nil || !restoredReport.OK || restoredReport.DataDir != restored {
		t.Fatalf("restore --force --json: %v\n%s", err, restoredJSON)
	}
}

func TestServeRejectsInvalidAgentSelectionFlags(t *testing.T) {
	repo := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "room")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--claude-runtime", "gemini"}, "Agent 1 default"},
		{[]string{"--codex-sandbox", "everything"}, "Agent 2 default"},
		{[]string{"--cc-switch-db", "relative.db"}, "cc_switch.database must be an absolute path"},
		{[]string{"--claude-command", " "}, "runtimes.claude.command is required"},
	} {
		args := append([]string{"serve", "--repo", repo, "--data-dir", dataDir, "--mock", "--no-browser"}, tc.args...)
		requireErrorContains(t, run(args), tc.want)
	}
}

func TestApplySlotCLIKeepsOnlyTheRuntimesPolicyFields(t *testing.T) {
	for _, tc := range []struct {
		runtime                       string
		wantRuntime                   string
		permission, approval, sandbox string
	}{
		{"claude-code", "claude", "plan", "", ""},
		{"CODEX", "codex", "", "on-request", "read-only"},
		{"grok_build", "grok", "plan", "", "read-only"},
	} {
		cfg := config.Agent{Runtime: "claude", PermissionMode: "old", ApprovalPolicy: "old", Sandbox: "old", Instructions: "old"}
		applySlotCLI(&cfg, tc.runtime, "model-x", "high", "plan", "on-request", "read-only", "  be brief \n")
		if cfg.Runtime != tc.wantRuntime || cfg.Model != "model-x" || cfg.Effort != "high" || cfg.Instructions != "be brief" {
			t.Fatalf("%s: selection = %+v", tc.runtime, cfg)
		}
		if cfg.PermissionMode != tc.permission || cfg.ApprovalPolicy != tc.approval || cfg.Sandbox != tc.sandbox {
			t.Fatalf("%s: policy = %q/%q/%q, want %q/%q/%q", tc.runtime, cfg.PermissionMode, cfg.ApprovalPolicy, cfg.Sandbox, tc.permission, tc.approval, tc.sandbox)
		}
	}
	cfg := config.Agent{Runtime: "codex"}
	applySlotCLI(&cfg, "", "", "", "", "", "", "")
	if cfg.Runtime != "codex" {
		t.Fatalf("empty runtime flag replaced the configured runtime: %+v", cfg)
	}
}

func TestServiceListenFailureReleasesTheDataRoot(t *testing.T) {
	isolateUserDirs(t)
	root := filepath.Join(t.TempDir(), "service-root")
	address := occupiedLoopbackAddress(t)
	err := run([]string{"service", "--data-root", root, "--listen", address, "--mock", "--no-browser"})
	requireErrorContains(t, err, "listen for Management Shell")
	assertNotExist(t, filepath.Join(root, "service.lock"))
	assertNotExist(t, filepath.Join(root, relay.EndpointFile))
}

func TestServiceRejectsInvalidAgentSelectionAfterClaimingThenReleasingTheRoot(t *testing.T) {
	isolateUserDirs(t)
	root := filepath.Join(t.TempDir(), "service-root")
	err := run([]string{"service", "--data-root", root, "--listen", "127.0.0.1:0", "--mock", "--no-browser", "--codex-runtime", "gemini"})
	requireErrorContains(t, err, "Agent 2 default")
	assertNotExist(t, filepath.Join(root, "service.lock"))
}

// The daemon control file is the Service's graceful-stop contract; this test
// drives a real mock Service through it and opens the logged Management URL
// with the same authenticated check `pairroom daemon open` uses.
func TestServiceServesAuthenticatedManagementUntilTheControlFileAppears(t *testing.T) {
	isolateUserDirs(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "service-root")
	control := filepath.Join(dir, "daemon.stop")
	writeTestFile(t, control, "stale request from a previous run\n")

	capture := startStdoutCapture(t)
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		done <- run([]string{"service", "--data-root", root, "--listen", "127.0.0.1:0", "--mock", "--no-browser",
			"--token", "service-secret", "--runtime-limit", "2", "--idle-timeout", "1m", "--shutdown-timeout", "30s", "--daemon-control-file", control})
	}()
	// Stop the Service before the capture and temporary roots are torn down,
	// even when an assertion below fails first.
	t.Cleanup(func() {
		_ = os.WriteFile(control, []byte("stop\n"), 0o600)
		select {
		case <-stopped:
		case <-time.After(30 * time.Second):
			t.Error("Service did not stop during cleanup")
		}
	})
	output := capture.waitFor(t, "  mode:       mock\n", 30*time.Second)
	for _, want := range []string{"PairRoom Service ", "  data root:  " + root + "\n", "  runtimes:   2 active maximum, idle timeout 1m0s\n"} {
		if !strings.Contains(output, want) {
			t.Fatalf("service output missing %q:\n%s", want, output)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "service.lock")); err != nil {
		t.Fatalf("running Service holds no lock: %v", err)
	}

	logFile := filepath.Join(dir, "service.log")
	writeTestFile(t, logFile, output)
	setDaemonTestConfigDir(t, filepath.Join(dir, "config"))
	if err := daemon.SaveMeta(&daemon.Meta{LogFile: logFile, LogBackups: 1, DataRoot: root}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeDaemonManager{status: daemon.Status{Installed: true, Running: true}}
	useFakeDaemonManager(t, manager)
	var opened string
	openManagementBrowser = func(value string) error { opened = value; return nil }
	if err := daemonOpen(nil); err != nil {
		t.Fatalf("daemon open against the running Service: %v", err)
	}
	if !strings.HasPrefix(opened, "http://127.0.0.1:") || !strings.HasSuffix(opened, "/#token=service-secret") {
		t.Fatalf("opened %q", opened)
	}

	writeTestFile(t, control, "stop\n")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Service stopped with %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Service ignored its daemon control file")
	}
	capture.Stop()
	assertNotExist(t, filepath.Join(root, "service.lock"))
	assertNotExist(t, filepath.Join(root, relay.EndpointFile))
}
