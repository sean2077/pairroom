package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/daemon"
	"github.com/sean2077/pairroom/internal/service"
)

type fakeDaemonManager struct {
	status    daemon.Status
	installed int
	started   int
	restarted int
	start     func() error
	restart   func() error
}

func (m *fakeDaemonManager) Install(daemon.Config) error { m.installed++; return nil }
func (*fakeDaemonManager) Uninstall() error              { return nil }
func (m *fakeDaemonManager) Start() error {
	m.started++
	if m.start != nil {
		return m.start()
	}
	return nil
}
func (*fakeDaemonManager) Stop() error { return nil }
func (m *fakeDaemonManager) Restart() error {
	m.restarted++
	if m.restart != nil {
		return m.restart()
	}
	return nil
}
func (m *fakeDaemonManager) Status() (*daemon.Status, error) {
	status := m.status
	return &status, nil
}
func (*fakeDaemonManager) Platform() string { return "test" }

func setHostUserConfigDir(t *testing.T, root string) {
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

func TestEmbeddedHostOwnsOneDataRootAndShutsDown(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	first, err := Start(ctx, Options{
		DataRoot:                 root,
		Mock:                     true,
		DisableExternalDiscovery: true,
		RuntimeLimit:             1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Mode() != ModeEmbedded || !strings.Contains(first.URL(), "?desktop=1#token=") {
		t.Fatalf("unexpected embedded host: mode=%q url=%q", first.Mode(), first.URL())
	}
	if got := filepath.Clean(first.DataRoot()); got != filepath.Clean(root) {
		t.Fatalf("embedded data root = %q, want %q", got, root)
	}
	if first.BrowserURL() == "" || strings.Contains(first.BrowserURL(), "desktop=1") || !strings.Contains(first.BrowserURL(), "#token=") {
		t.Fatalf("embedded browser URL = %q, want token fragment without the desktop marker", first.BrowserURL())
	}

	if _, err := Start(ctx, Options{
		DataRoot:                 root,
		Mock:                     true,
		DisableExternalDiscovery: true,
	}); !errors.Is(err, service.ErrServiceLockOwnerRunning) {
		t.Fatalf("second host error = %v, want ErrServiceLockOwnerRunning", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := first.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if err := first.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("idempotent shutdown: %v", err)
	}
}

func TestStartEmbeddedRecoversCrashStaleLock(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := json.Marshal(map[string]any{"pid": 99999999, "started_at": "2026-09-02T03:55:24Z", "nonce": "stale"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "service.lock"), append(lock, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	host, err := Start(ctx, Options{
		DataRoot:                 root,
		Mock:                     true,
		DisableExternalDiscovery: true,
		RuntimeLimit:             1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Shutdown(context.Background())
	if host.Mode() != ModeEmbedded {
		t.Fatalf("mode = %q", host.Mode())
	}
	info, found, err := service.InspectServiceLock(root)
	if err != nil || !found || info.PID != os.Getpid() {
		t.Fatalf("embedded lock after stale recovery: found=%v pid=%d err=%v", found, info.PID, err)
	}
}

func TestStartStartsInstalledDaemonInsteadOfStartingEmbeddedCompetitor(t *testing.T) {
	configRoot := t.TempDir()
	setHostUserConfigDir(t, configRoot)
	t.Setenv("PAIRROOM_DESKTOP_URL", "")
	t.Setenv(dataRootVariable, "")

	managementSecret := "desktop-daemon-secret"
	managementURL := ""
	logFile := filepath.Join(configRoot, "service.log")
	dataRoot := filepath.Join(configRoot, "data")
	server := newTestManagementServer(t, managementSecret, &managementURL, dataRoot)
	defer server.Close()
	if err := daemon.SaveMeta(&daemon.Meta{LogFile: logFile, LogBackups: 1, DataRoot: dataRoot, BinaryPath: "pairroom.exe"}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeDaemonManager{status: daemon.Status{Installed: true, Running: false}}
	manager.start = func() error {
		return os.WriteFile(logFile, []byte("management: "+managementURL+"\n"), 0o600)
	}
	original := newDaemonManager
	t.Cleanup(func() { newDaemonManager = original })
	newDaemonManager = func() (daemon.Manager, error) { return manager, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	host, err := Start(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if host.Mode() != ModeExternal || manager.started != 1 {
		t.Fatalf("host mode=%q daemon starts=%d", host.Mode(), manager.started)
	}
	if got := filepath.Clean(host.DataRoot()); got != filepath.Clean(dataRoot) {
		t.Fatalf("external host data root = %q, want daemon root %q", got, dataRoot)
	}
	if host.BrowserURL() == "" || strings.Contains(host.BrowserURL(), "desktop=1") {
		t.Fatalf("external host browser URL = %q, want authenticated URL without the desktop marker", host.BrowserURL())
	}
}

func TestStartRecoversCrashStaleLockAndStartsInstalledDaemon(t *testing.T) {
	configRoot := t.TempDir()
	setHostUserConfigDir(t, configRoot)
	t.Setenv("PAIRROOM_DESKTOP_URL", "")
	t.Setenv(dataRootVariable, "")

	managementSecret := "desktop-stale-secret"
	managementURL := ""
	logFile := filepath.Join(configRoot, "service.log")
	dataRoot := filepath.Join(configRoot, "data")
	server := newTestManagementServer(t, managementSecret, &managementURL, dataRoot)
	defer server.Close()
	if err := daemon.SaveMeta(&daemon.Meta{LogFile: logFile, LogBackups: 1, DataRoot: dataRoot, BinaryPath: "pairroom.exe"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	const stalePID = 99999999
	lock, err := json.Marshal(map[string]any{"pid": stalePID, "started_at": "2026-09-02T03:55:24Z", "nonce": "stale"})
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dataRoot, "service.lock")
	if err := os.WriteFile(lockPath, append(lock, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &fakeDaemonManager{status: daemon.Status{Installed: true, Running: false}}
	manager.start = func() error {
		return os.WriteFile(logFile, []byte("management: "+managementURL+"\n"), 0o600)
	}
	original := newDaemonManager
	t.Cleanup(func() { newDaemonManager = original })
	newDaemonManager = func() (daemon.Manager, error) { return manager, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	host, err := Start(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if host.Mode() != ModeExternal || manager.started != 1 || manager.restarted != 0 {
		t.Fatalf("host mode=%q daemon starts=%d restarts=%d", host.Mode(), manager.started, manager.restarted)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crash-stale lock still present: %v", err)
	}
}

func TestStartRestartsRunningDaemonAfterCrashStaleLockRecovery(t *testing.T) {
	configRoot := t.TempDir()
	setHostUserConfigDir(t, configRoot)
	t.Setenv("PAIRROOM_DESKTOP_URL", "")
	t.Setenv(dataRootVariable, "")

	managementSecret := "desktop-stale-restart-secret"
	managementURL := ""
	logFile := filepath.Join(configRoot, "service.log")
	dataRoot := filepath.Join(configRoot, "data")
	server := newTestManagementServer(t, managementSecret, &managementURL, dataRoot)
	defer server.Close()
	if err := daemon.SaveMeta(&daemon.Meta{LogFile: logFile, LogBackups: 1, DataRoot: dataRoot, BinaryPath: "pairroom.exe"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := json.Marshal(map[string]any{"pid": 99999999, "started_at": "2026-09-02T03:55:24Z", "nonce": "stale"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "service.lock"), append(lock, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &fakeDaemonManager{status: daemon.Status{Installed: true, Running: true}}
	manager.restart = func() error {
		return os.WriteFile(logFile, []byte("management: "+managementURL+"\n"), 0o600)
	}
	original := newDaemonManager
	t.Cleanup(func() { newDaemonManager = original })
	newDaemonManager = func() (daemon.Manager, error) { return manager, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	host, err := Start(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if host.Mode() != ModeExternal || manager.started != 0 || manager.restarted != 1 {
		t.Fatalf("host mode=%q daemon starts=%d restarts=%d", host.Mode(), manager.started, manager.restarted)
	}
}

func TestStartLeavesLiveLockOwnerAndDoesNotStartEmbeddedCompetitor(t *testing.T) {
	configRoot := t.TempDir()
	setHostUserConfigDir(t, configRoot)
	t.Setenv("PAIRROOM_DESKTOP_URL", "")
	t.Setenv(dataRootVariable, "")
	logFile := filepath.Join(configRoot, "service.log")
	dataRoot := filepath.Join(configRoot, "data")
	if err := daemon.SaveMeta(&daemon.Meta{LogFile: logFile, LogBackups: 1, DataRoot: dataRoot, BinaryPath: "pairroom.exe"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, []byte("not a management url\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := json.Marshal(map[string]any{"pid": os.Getpid(), "started_at": "2026-09-02T03:55:24Z", "nonce": "live"})
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dataRoot, "service.lock")
	if err := os.WriteFile(lockPath, append(lock, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &fakeDaemonManager{status: daemon.Status{Installed: true, Running: false}}
	original := newDaemonManager
	t.Cleanup(func() { newDaemonManager = original })
	newDaemonManager = func() (daemon.Manager, error) { return manager, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, err = Start(ctx, Options{})
	if err == nil || !strings.Contains(err.Error(), "authenticated Management Shell did not become available") {
		t.Fatalf("live-lock startup error = %v", err)
	}
	if manager.started != 0 || manager.restarted != 0 {
		t.Fatalf("daemon starts=%d restarts=%d, want no start while a live owner holds the root", manager.started, manager.restarted)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("live lock was removed: %v", err)
	}
}

func TestStartRefusesEmbeddedWhenDaemonManagerCannotBeInspected(t *testing.T) {
	configRoot := t.TempDir()
	setHostUserConfigDir(t, configRoot)
	t.Setenv("PAIRROOM_DESKTOP_URL", "")
	t.Setenv(dataRootVariable, "")
	original := newDaemonManager
	t.Cleanup(func() { newDaemonManager = original })
	newDaemonManager = func() (daemon.Manager, error) { return nil, errors.New("scheduler unavailable") }

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Start(ctx, Options{})
	if err == nil || !strings.Contains(err.Error(), "scheduler unavailable") {
		t.Fatalf("uninspectable daemon startup error = %v", err)
	}
}

func TestStartRefusesInstalledDaemonWithoutMetadata(t *testing.T) {
	configRoot := t.TempDir()
	setHostUserConfigDir(t, configRoot)
	t.Setenv("PAIRROOM_DESKTOP_URL", "")
	t.Setenv(dataRootVariable, "")
	manager := &fakeDaemonManager{status: daemon.Status{Installed: true, Running: false}}
	original := newDaemonManager
	t.Cleanup(func() { newDaemonManager = original })
	newDaemonManager = func() (daemon.Manager, error) { return manager, nil }

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Start(ctx, Options{})
	if err == nil || !strings.Contains(err.Error(), "metadata is missing") {
		t.Fatalf("missing metadata startup error = %v", err)
	}
	if manager.started != 0 {
		t.Fatalf("daemon starts=%d, want no start while metadata is missing", manager.started)
	}
}

func TestDefaultStartDoesNotInstallDaemon(t *testing.T) {
	setHostUserConfigDir(t, t.TempDir())
	for _, name := range []string{"PAIRROOM_DESKTOP_URL", configPathVariable, dataRootVariable} {
		t.Setenv(name, "")
	}
	manager := &fakeDaemonManager{}
	original := newDaemonManager
	t.Cleanup(func() { newDaemonManager = original })
	newDaemonManager = func() (daemon.Manager, error) { return manager, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	host, err := Start(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Shutdown(context.Background())
	if host.Mode() != ModeEmbedded || manager.installed != 0 || manager.started != 0 || manager.restarted != 0 {
		t.Fatalf("mode=%q, daemon mutations=%+v", host.Mode(), manager)
	}
	if _, err := daemon.LoadMeta(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("desktop launch created daemon metadata: %v", err)
	}
}

func TestStartExplicitEmbeddedSelectionDoesNotInspectInstalledDaemon(t *testing.T) {
	configRoot := t.TempDir()
	setHostUserConfigDir(t, configRoot)
	t.Setenv("PAIRROOM_DESKTOP_URL", "")
	dataRoot := filepath.Join(configRoot, "embedded")
	if err := daemon.SaveMeta(&daemon.Meta{LogFile: filepath.Join(configRoot, "service.log"), DataRoot: filepath.Join(configRoot, "daemon")}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeDaemonManager{status: daemon.Status{Installed: true, Running: false}}
	original := newDaemonManager
	t.Cleanup(func() { newDaemonManager = original })
	newDaemonManager = func() (daemon.Manager, error) { return manager, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	host, err := Start(ctx, Options{DataRoot: dataRoot, Mock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Shutdown(context.Background())
	if host.Mode() != ModeEmbedded || manager.started != 0 {
		t.Fatalf("host mode=%q daemon starts=%d", host.Mode(), manager.started)
	}
}

func newTestManagementServer(t *testing.T, secret string, urlOut *string, dataRoot string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/service" || r.Header.Get("Authorization") != "Bearer "+secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"healthy":true,"data_root":%q}`, dataRoot)
	}))
	*urlOut = server.URL + "/#token=" + secret
	return server
}

func TestEnvironmentPathsAreAccepted(t *testing.T) {
	root := t.TempDir()
	t.Setenv(dataRootVariable, root)
	t.Setenv(configPathVariable, "")
	t.Setenv("PAIRROOM_DESKTOP_URL", "")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	value, err := Start(ctx, Options{
		Mock:                     true,
		DisableExternalDiscovery: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer value.Shutdown(context.Background())
	if value.Mode() != ModeEmbedded {
		t.Fatalf("mode = %q", value.Mode())
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("desktop data root was not used: %v", err)
	}
}

func TestShutdownRetriesAfterTimeoutAndKeepsServiceLockUntilComplete(t *testing.T) {
	root := t.TempDir()
	lock, err := service.AcquireServiceLock(root, false)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	value := &Host{
		mode:      ModeEmbedded,
		lock:      lock,
		serveDone: serveDone,
	}

	expiredCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := value.Shutdown(expiredCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("first shutdown error = %v, want context cancellation", err)
	}
	if _, err := service.AcquireServiceLock(root, false); !errors.Is(err, service.ErrServiceAlreadyRunning) {
		t.Fatalf("lock acquisition after incomplete shutdown = %v, want ErrServiceAlreadyRunning", err)
	}

	serveDone <- nil
	if err := value.Shutdown(context.Background()); err != nil {
		t.Fatalf("retry shutdown: %v", err)
	}
	replacement, err := service.AcquireServiceLock(root, false)
	if err != nil {
		t.Fatalf("acquire lock after completed shutdown: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}
