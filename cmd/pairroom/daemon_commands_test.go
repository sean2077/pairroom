package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/daemon"
)

func useFakeDaemonManager(t *testing.T, manager *fakeDaemonManager) {
	t.Helper()
	original, originalOpen := newDaemonManager, openManagementBrowser
	t.Cleanup(func() { newDaemonManager, openManagementBrowser = original, originalOpen })
	newDaemonManager = func() (daemon.Manager, error) { return manager, nil }
	openManagementBrowser = func(string) error {
		t.Error("lifecycle test unexpectedly opened a browser")
		return nil
	}
}

func TestDaemonDispatchRejectsUnknownSubcommand(t *testing.T) {
	requireErrorContains(t, run([]string{"daemon", "reboot"}), `unknown daemon command "reboot"`)
}

func TestDaemonCommandsWithoutArgumentsRejectExtras(t *testing.T) {
	manager := &fakeDaemonManager{status: daemon.Status{Installed: true, Running: true}}
	useFakeDaemonManager(t, manager)
	for _, command := range []string{"uninstall", "stop", "status", "open"} {
		requireErrorContains(t, run([]string{"daemon", command, "extra"}), "daemon "+command+" does not accept arguments: extra")
	}
	for _, command := range []string{"start", "restart"} {
		requireErrorContains(t, run([]string{"daemon", command, "--force"}), `unknown daemon `+command+` option "--force"`)
	}
	if manager.uninstalled+manager.stopped+manager.started+manager.restarted != 0 {
		t.Fatalf("rejected arguments reached the service manager: %+v", manager)
	}
}

func TestDaemonLifecycleRequiresAnInstalledService(t *testing.T) {
	setDaemonTestConfigDir(t, t.TempDir())
	manager := &fakeDaemonManager{}
	useFakeDaemonManager(t, manager)
	for _, command := range []string{"start", "stop", "restart", "open"} {
		requireErrorContains(t, run([]string{"daemon", command}), "PairRoom daemon is not installed; run pairroom daemon install")
	}
	if manager.stopped+manager.started+manager.restarted != 0 {
		t.Fatalf("lifecycle reached an uninstalled manager: %+v", manager)
	}
}

func TestDaemonStartAndStopAreIdempotent(t *testing.T) {
	setDaemonTestConfigDir(t, t.TempDir())
	manager := &fakeDaemonManager{status: daemon.Status{Installed: true, Running: true}}
	useFakeDaemonManager(t, manager)
	output, err := captureRun(t, "daemon", "start")
	if err != nil || output != "PairRoom daemon is already running.\n" || manager.started != 0 {
		t.Fatalf("start while running: %q %v %+v", output, err, manager)
	}
	output, err = captureRun(t, "daemon", "stop")
	if err != nil || output != "PairRoom daemon stopped.\n" || manager.stopped != 1 {
		t.Fatalf("stop while running: %q %v %+v", output, err, manager)
	}
	manager.status.Running = false
	output, err = captureRun(t, "daemon", "stop")
	if err != nil || output != "PairRoom daemon is already stopped.\n" || manager.stopped != 1 {
		t.Fatalf("stop while stopped: %q %v %+v", output, err, manager)
	}
}

func TestDaemonOpenRequiresARunningServiceAndMetadata(t *testing.T) {
	setDaemonTestConfigDir(t, t.TempDir())
	manager := &fakeDaemonManager{status: daemon.Status{Installed: true}}
	useFakeDaemonManager(t, manager)
	requireErrorContains(t, run([]string{"daemon", "open"}), "PairRoom daemon is not running; run pairroom daemon start")
	manager.status.Running = true
	requireErrorContains(t, run([]string{"daemon", "open"}), "load daemon metadata")
}

func TestDaemonRestartWithRecoveryStopsThenStartsARunningService(t *testing.T) {
	root := t.TempDir()
	setDaemonTestConfigDir(t, filepath.Join(root, "config"))
	dataRoot := filepath.Join(root, "data")
	staleLock := filepath.Join(dataRoot, "service.lock")
	writeTestFile(t, staleLock, `{"pid":99999999,"started_at":"2026-09-02T03:55:24Z","nonce":"stale"}`+"\n")
	if err := daemon.SaveMeta(&daemon.Meta{LogFile: filepath.Join(root, "service.log"), LogBackups: 1, DataRoot: dataRoot}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeDaemonManager{status: daemon.Status{Installed: true, Running: true}}
	useFakeDaemonManager(t, manager)
	management := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer restart-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}))
	defer management.Close()
	writeTestFile(t, filepath.Join(root, "service.log"), "management: "+management.URL+"/#token=restart-secret\n")
	var opened []string
	openManagementBrowser = func(value string) error { opened = append(opened, value); return nil }
	output, err := captureRun(t, "daemon", "restart", "--recover-stale-lock")
	if err != nil {
		t.Fatal(err)
	}
	if manager.stopped != 1 || manager.started != 1 || manager.restarted != 0 {
		t.Fatalf("recovering restart calls: %+v", manager)
	}
	if output != "PairRoom daemon restarted.\nPairRoom Management Shell opened in the default browser.\n" || len(opened) != 1 {
		t.Fatalf("restart output = %q, opened %q", output, opened)
	}
	assertNotExist(t, staleLock)
}

func TestDaemonUninstallRemovesOwnedFiles(t *testing.T) {
	setDaemonTestConfigDir(t, t.TempDir())
	metaPath, err := daemon.MetaPath()
	if err != nil {
		t.Fatal(err)
	}
	controlPath, err := daemon.DefaultControlFile()
	if err != nil {
		t.Fatal(err)
	}
	for _, installed := range []bool{false, true} {
		if err := daemon.SaveMeta(&daemon.Meta{LogFile: "service.log"}); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, controlPath, "stop\n")
		manager := &fakeDaemonManager{status: daemon.Status{Installed: installed}}
		useFakeDaemonManager(t, manager)
		output, err := captureRun(t, "daemon", "uninstall")
		if err != nil {
			t.Fatal(err)
		}
		want, calls := "PairRoom daemon is not installed.\n", 0
		if installed {
			want, calls = "PairRoom daemon uninstalled.\n", 1
		}
		if output != want || manager.uninstalled != calls {
			t.Fatalf("installed=%v: output %q, uninstall calls %d", installed, output, manager.uninstalled)
		}
		assertNotExist(t, metaPath)
		assertNotExist(t, controlPath)
	}
}

func TestDaemonStatusReportsInstallationProcessAndLockState(t *testing.T) {
	root := t.TempDir()
	setDaemonTestConfigDir(t, filepath.Join(root, "config"))
	dataRoot := filepath.Join(root, "data")
	manager := &fakeDaemonManager{}
	useFakeDaemonManager(t, manager)

	output, err := captureRun(t, "daemon", "status")
	if err != nil || output != "PairRoom daemon status\n  status:   not installed\n  platform: test\n" {
		t.Fatalf("status without install: %q %v", output, err)
	}

	installedAt := time.Date(2026, 9, 1, 2, 3, 4, 0, time.UTC)
	if err := daemon.SaveMeta(&daemon.Meta{LogFile: "/logs/service.log", LogMaxSize: 2048, LogBackups: 4, DataRoot: dataRoot, InstalledAt: installedAt.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	output, err = captureRun(t, "daemon", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  status:   not installed\n",
		"  metadata: present but the platform service is missing; run `pairroom daemon install --force` to repair it\n",
		"  data root: " + dataRoot + "\n",
		"  lock:      absent\n",
		"  log:      /logs/service.log\n",
		"  rotation: 2048 bytes, 4 backups\n",
		"  installed: " + installedAt.Local().Format("2006-01-02 15:04:05") + "\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("orphaned-metadata status missing %q:\n%s", want, output)
		}
	}

	manager.status = daemon.Status{Installed: true, Running: true, PID: 4242, Platform: "test-platform"}
	lockPath := filepath.Join(dataRoot, "service.lock")
	pid := jsonNumber(os.Getpid())
	for _, tc := range []struct{ lock, want string }{
		{`{"pid":` + pid + `,"started_at":"` + time.Now().UTC().Format(time.RFC3339) + `","nonce":"live"}`, "  lock:      pid " + pid + " running (started "},
		{`{"pid":99999999,"started_at":"2026-09-02T03:55:24Z","nonce":"stale"}`, "  lock:      pid 99999999 not running; daemon start recovers this crash-stale lock\n"},
		{`{"pid":0}`, "  lock:      unreadable ("},
	} {
		writeTestFile(t, lockPath, tc.lock+"\n")
		output, err = captureRun(t, "daemon", "status")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"  status:   running\n", "  platform: test-platform\n", "  pid:      4242\n", "  open:     pairroom daemon open\n", tc.want} {
			if !strings.Contains(output, want) {
				t.Fatalf("running status missing %q:\n%s", want, output)
			}
		}
	}

	manager.status = daemon.Status{Installed: true, Platform: "test-platform"}
	metaPath, err := daemon.MetaPath()
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, metaPath, "{not json")
	output, err = captureRun(t, "daemon", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "  status:   stopped\n") || strings.Contains(output, "pid:") || !strings.Contains(output, "  metadata: unreadable (decode daemon metadata") {
		t.Fatalf("stopped status with unreadable metadata:\n%s", output)
	}

	writeTestFile(t, metaPath, `{"log_file":"x","data_root":"relative"}`)
	output, err = captureRun(t, "daemon", "status")
	if err != nil || !strings.Contains(output, "  data root: unresolved (service data root must be absolute)\n") {
		t.Fatalf("relative metadata root status: %v\n%s", err, output)
	}
}

func jsonNumber(value int) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestParseDaemonInstallArgs(t *testing.T) {
	cfg, force, help, err := parseDaemonInstallArgs([]string{
		"--force", "--binary=/bin/pairroom", "--work-dir=/work", "--log-file=/logs/p.log",
		"--log-max-size=1K", "--log-max-backups=7", "--mock", "--", "--binary", "forwarded",
	})
	if err != nil || !force || help {
		t.Fatalf("parse: force=%v help=%v err=%v", force, help, err)
	}
	if cfg.BinaryPath != "/bin/pairroom" || cfg.WorkDir != "/work" || cfg.LogFile != "/logs/p.log" || cfg.LogMaxSize != 1024 || cfg.LogBackups != 7 {
		t.Fatalf("parsed config = %+v", cfg)
	}
	if strings.Join(cfg.Args, " ") != "service --mock --binary forwarded" {
		t.Fatalf("service args = %q", cfg.Args)
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--binary"}, "missing value for --binary"},
		{[]string{"--work-dir", "--force"}, "missing value for --work-dir"},
		{[]string{"--log-file"}, "missing value for --log-file"},
		{[]string{"--log-max-size"}, "missing value for --log-max-size"},
		{[]string{"--log-max-backups"}, "missing value for --log-max-backups"},
		{[]string{"--log-max-size", "0"}, "invalid log size"},
		{[]string{"--log-max-size=big"}, "invalid log size"},
		{[]string{"--log-max-backups", "0"}, "invalid log backup count"},
		{[]string{"--log-max-backups=1001"}, "invalid log backup count"},
	} {
		_, _, _, err := parseDaemonInstallArgs(tc.args)
		requireErrorContains(t, err, tc.want)
	}
}

func TestNormalizeDaemonServiceArgsRejectsUnsafeServiceOptions(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"serve"}, "daemon must run the pairroom service command"},
		{[]string{"service", "service"}, "not a second service command"},
		{[]string{"service", "--daemon-control-file", "/x"}, "daemon-control-file is managed internally"},
		{[]string{"service", "--config"}, "missing value for --config"},
		{[]string{"service", "--data-root="}, "daemon service path must not be empty"},
		{[]string{"service", "--config", "missing.json"}, "read config"},
		{[]string{"service", "--shutdown-timeout", "soon"}, `invalid service shutdown-timeout "soon"`},
		{[]string{"service", "--shutdown-timeout=-1m"}, `invalid service shutdown-timeout "-1m"`},
		{[]string{"service", "--shutdown-timeout=2562047h47m"}, "service shutdown-timeout is too large"},
	} {
		cfg := daemon.Config{WorkDir: root, ControlFile: filepath.Join(root, "daemon.stop"), Args: tc.args}
		requireErrorContains(t, normalizeDaemonServiceArgs(&cfg), tc.want)
	}
}

func TestDaemonInstallRefusesToReplaceWithoutForce(t *testing.T) {
	root := t.TempDir()
	setDaemonTestConfigDir(t, filepath.Join(root, "config"))
	binary := fakeExecutable(t, root, "pairroom")
	manager := &fakeDaemonManager{status: daemon.Status{Installed: true}}
	useFakeDaemonManager(t, manager)
	requireErrorContains(t, run([]string{"daemon", "install", "--binary", binary, "--work-dir", root}), "already installed; use --force")
	if manager.installed != nil {
		t.Fatal("existing installation was replaced without --force")
	}
	if _, err := daemon.LoadMeta(); !os.IsNotExist(err) {
		t.Fatalf("refused install wrote metadata: %v", err)
	}

	output, err := captureRun(t, "daemon", "install", "--force", "--binary", binary, "--work-dir", root)
	if err != nil {
		t.Fatal(err)
	}
	if manager.installed == nil || !strings.HasPrefix(output, "PairRoom daemon installed and started.\n") || !strings.Contains(output, "  binary:   "+binary+"\n") {
		t.Fatalf("forced install: %q %+v", output, manager.installed)
	}
}

func TestDaemonInstallRejectsInvalidOptionsBeforeTouchingTheManager(t *testing.T) {
	root := t.TempDir()
	setDaemonTestConfigDir(t, filepath.Join(root, "config"))
	binary := fakeExecutable(t, root, "pairroom")
	manager := &fakeDaemonManager{}
	useFakeDaemonManager(t, manager)
	for _, args := range [][]string{
		{"--recover-stale-lock"},
		{"--config", "missing.json"},
		{"--", "service"},
		{"--log-max-size", "0"},
	} {
		if err := run(append([]string{"daemon", "install", "--binary", binary, "--work-dir", root}, args...)); err == nil {
			t.Fatalf("install accepted %q", args)
		}
	}
	requireErrorContains(t, run([]string{"daemon", "install", "--binary", filepath.Join(root, "missing"), "--work-dir", root}), "pairroom executable")
	if manager.installed != nil {
		t.Fatalf("invalid install reached the manager: %#v", manager.installed.Args)
	}
	if _, err := daemon.LoadMeta(); !os.IsNotExist(err) {
		t.Fatalf("invalid install wrote metadata: %v", err)
	}
}

func TestDaemonLogsPrintsTailFromMetadataOrOverride(t *testing.T) {
	root := t.TempDir()
	setDaemonTestConfigDir(t, filepath.Join(root, "config"))
	metaLog := filepath.Join(root, "meta.log")
	writeTestFile(t, metaLog, "a\nb\nc\n")
	if err := daemon.SaveMeta(&daemon.Meta{LogFile: metaLog}); err != nil {
		t.Fatal(err)
	}
	output, err := captureRun(t, "daemon", "logs", "-n", "2")
	if err != nil || output != "b\nc\n" {
		t.Fatalf("logs from metadata: %q %v", output, err)
	}
	override := filepath.Join(root, "override.log")
	writeTestFile(t, override, "only\n")
	output, err = captureRun(t, "daemon", "logs", "--log-file", override)
	if err != nil || output != "only\n" {
		t.Fatalf("logs override: %q %v", output, err)
	}
	empty := filepath.Join(root, "empty.log")
	writeTestFile(t, empty, "\r\n")
	output, err = captureRun(t, "daemon", "logs", "--log-file", empty)
	if err != nil || output != "" {
		t.Fatalf("empty log: %q %v", output, err)
	}
}

func TestDaemonLogsDefaultsToTheServiceLogWithoutMetadata(t *testing.T) {
	setDaemonTestConfigDir(t, t.TempDir())
	defaultLog, err := daemon.DefaultLogFile()
	if err != nil {
		t.Fatal(err)
	}
	requireErrorContains(t, run([]string{"daemon", "logs"}), "read daemon log "+defaultLog)
	writeTestFile(t, defaultLog, "default\n")
	output, err := captureRun(t, "daemon", "logs")
	if err != nil || output != "default\n" {
		t.Fatalf("default log: %q %v", output, err)
	}
}

func TestDaemonLogsValidatesArguments(t *testing.T) {
	log := filepath.Join(t.TempDir(), "service.log")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-n", "0"}, "daemon logs -n must be between 1 and 1000000"},
		{[]string{"-n", "1000001"}, "daemon logs -n must be between 1 and 1000000"},
		{[]string{"extra"}, "unexpected daemon logs arguments: extra"},
		{[]string{"--lines", "3"}, "lines"},
	} {
		requireErrorContains(t, run(append([]string{"daemon", "logs", "--log-file", log}, tc.args...)), tc.want)
	}
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (b *lockedBuffer) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for b.String() != want {
		if time.Now().After(deadline) {
			t.Fatalf("followed output = %q, want %q", b.String(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(text); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFollowLogStreamsAppendsAndRestartsAfterTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	writeTestFile(t, path, "already printed\n")
	// Fix the already-printed state before appending; followLog would otherwise
	// race the append with its own initial stat.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output lockedBuffer
	done := make(chan error, 1)
	go func() { done <- followLogFrom(ctx, &output, path, info) }()

	appendFile(t, path, "one\n")
	output.waitFor(t, "one\n")
	// A log rewritten below the followed offset is read again from its start.
	writeTestFile(t, path, "new\n")
	output.waitFor(t, "one\nnew\n")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("followLog returned %v after cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("followLog did not stop after cancellation")
	}
}

func TestFollowLogRequiresAnExistingLog(t *testing.T) {
	err := followLog(context.Background(), &lockedBuffer{}, filepath.Join(t.TempDir(), "missing.log"))
	requireErrorContains(t, err, "stat daemon log")
}

func TestManagementURLCandidatesPreferNewestAndReadPastTheTailWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	filler := strings.Repeat("x", 128) + "\n"
	var content strings.Builder
	content.WriteString("management: http://127.0.0.1:1111/#token=early\n")
	for content.Len() <= daemonLogTailWindow+len(filler) {
		content.WriteString(filler)
	}
	writeTestFile(t, path, content.String())
	candidates, err := managementURLCandidates(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0] != "http://127.0.0.1:1111/#token=early" {
		t.Fatalf("start line before the tail window was not found: %q", candidates)
	}

	writeTestFile(t, path, "management: http://127.0.0.1:1/#token=a\n  management:   http://127.0.0.1:2/#token=b  \nmanagement:\nmanagement: http://127.0.0.1:1/#token=a\n")
	writeTestFile(t, path+".1", "management: http://127.0.0.1:3/#token=c\n")
	candidates, err = managementURLCandidates(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://127.0.0.1:1/#token=a", "http://127.0.0.1:2/#token=b", "http://127.0.0.1:3/#token=c"}
	if strings.Join(candidates, " ") != strings.Join(want, " ") {
		t.Fatalf("candidates = %q, want newest first and deduplicated %q", candidates, want)
	}
}

func TestWaitForManagementURLExplainsWhyNoAddressWasUsable(t *testing.T) {
	dir := t.TempDir()
	_, err := waitForManagementURL(filepath.Join(dir, "missing.log"), 1, 150*time.Millisecond)
	requireErrorContains(t, err, "Management Shell address is not available in the daemon log")
	logged := filepath.Join(dir, "service.log")
	writeTestFile(t, logged, "management: http://127.0.0.1:1/#token=unreachable\n")
	_, err = waitForManagementURL(logged, 0, 150*time.Millisecond)
	requireErrorContains(t, err, "no logged Management Shell address authenticated the running daemon")
}

func TestParseManagementAccessNormalizesAcceptedURLs(t *testing.T) {
	access, err := parseManagementAccess(" http://[::1]:7332#token=%20abc%20 ")
	if err != nil {
		t.Fatal(err)
	}
	if access.browserURL != "http://[::1]:7332/#token=abc" || access.apiURL != "http://[::1]:7332/api/v1/service" || access.token != "abc" {
		t.Fatalf("access = %+v", access)
	}
	for _, value := range []string{
		"http://user@127.0.0.1:7332/#token=x",
		"http://127.0.0.1:7332/other#token=x",
		"http://127.0.0.1/#token=x",
		"http://127.0.0.1:0/#token=x",
		"http://127.0.0.1:7332/#token=x&extra=y",
		"http://127.0.0.1:7332/#token=%20",
	} {
		if _, err := parseManagementAccess(value); err == nil {
			t.Errorf("unsafe Management URL was accepted: %q", value)
		}
	}
}
