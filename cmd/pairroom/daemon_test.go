package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/daemon"
)

type fakeDaemonManager struct {
	status      daemon.Status
	installed   *daemon.Config
	started     int
	stopped     int
	restarted   int
	uninstalled int
}

func (m *fakeDaemonManager) Install(cfg daemon.Config) error {
	copy := cfg
	m.installed = &copy
	return nil
}

func (m *fakeDaemonManager) Uninstall() error { m.uninstalled++; return nil }
func (m *fakeDaemonManager) Start() error     { m.started++; return nil }
func (m *fakeDaemonManager) Stop() error      { m.stopped++; return nil }
func (m *fakeDaemonManager) Restart() error   { m.restarted++; return nil }
func (m *fakeDaemonManager) Status() (*daemon.Status, error) {
	status := m.status
	return &status, nil
}
func (*fakeDaemonManager) Platform() string { return "test" }

func setDaemonTestConfigDir(t *testing.T, root string) {
	t.Helper()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", root)
	case "darwin":
		t.Setenv("HOME", root)
	default:
		t.Setenv("XDG_CONFIG_HOME", root)
	}
}

func TestNormalizeDaemonServiceArgsMakesPathsStable(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "pairroom.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := daemon.Config{
		WorkDir: root, ControlFile: filepath.Join(root, "daemon.stop"),
		Args: []string{"service", "--config", "pairroom.json", "--data-root=state", "--shutdown-timeout", "30m", "--mock", "--token", "secret"},
	}
	if err := normalizeDaemonServiceArgs(&cfg); err != nil {
		t.Fatal(err)
	}
	if got := flagValue(cfg.Args, "--config"); got != configPath {
		t.Fatalf("config path = %q, want %q", got, configPath)
	}
	if got := flagValue(cfg.Args, "--data-root"); got != filepath.Join(root, "state") {
		t.Fatalf("data root = %q", got)
	}
	if cfg.StopTimeout != 31*time.Minute {
		t.Fatalf("stop timeout = %s, want 31m", cfg.StopTimeout)
	}
	joined := strings.Join(cfg.Args, "\x00")
	for _, expected := range []string{"--no-browser", "--daemon-control-file", cfg.ControlFile, "secret"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("normalized args missing %q: %#v", expected, cfg.Args)
		}
	}
}

func TestFlagValueMatchesServiceLastValueWinsSemantics(t *testing.T) {
	args := []string{"service", "--data-root", "first", "--data-root=second"}
	if got := flagValue(args, "--data-root"); got != "second" {
		t.Fatalf("data root = %q, want last value", got)
	}
}

func TestDaemonInstallForwardsServiceOptionsWithoutPersistingToken(t *testing.T) {
	root := t.TempDir()
	setDaemonTestConfigDir(t, filepath.Join(root, "config"))
	binary := filepath.Join(root, "pairroom")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if err := os.WriteFile(binary, []byte("test"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &fakeDaemonManager{}
	original := newDaemonManager
	t.Cleanup(func() { newDaemonManager = original })
	newDaemonManager = func() (daemon.Manager, error) { return manager, nil }
	if err := daemonInstall([]string{
		"--binary", binary, "--work-dir", root, "--log-file", filepath.Join(root, "service.log"),
		"--log-max-size", "2MB", "--log-max-backups", "2",
		"--mock", "--token", "super-secret",
	}); err != nil {
		t.Fatal(err)
	}
	if manager.installed == nil || flagValue(manager.installed.Args, "--token") != "super-secret" {
		t.Fatalf("service options were not forwarded: %#v", manager.installed)
	}
	if manager.installed.LogMaxSize != 2*1024*1024 || manager.installed.LogBackups != 2 {
		t.Fatalf("log rotation was not configured: %#v", manager.installed)
	}
	meta, err := daemon.LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("super-secret")) {
		t.Fatalf("token leaked into metadata: %s", data)
	}
	if meta.LogMaxSize != 2*1024*1024 || meta.LogBackups != 2 {
		t.Fatalf("log rotation metadata was not saved: %#v", meta)
	}
}

func TestWatchDaemonControlFileCancelsServiceContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.stop")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchDaemonControlFile(ctx, path, cancel)
	if err := os.WriteFile(path, []byte("stop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("control file did not stop the service context")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("service removed the stop request before the Windows supervisor could observe it: %v", err)
	}
}

func TestPrintLastLogLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	if err := os.WriteFile(path, []byte("one\r\ntwo\r\nthree\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := printLastLogLines(&output, path, 2); err != nil {
		t.Fatal(err)
	}
	if output.String() != "two\nthree\n" {
		t.Fatalf("unexpected tail: %q", output.String())
	}
}
