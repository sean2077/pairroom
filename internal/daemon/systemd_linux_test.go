//go:build linux

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildSystemdUnitQuotesArgumentsAndLogs(t *testing.T) {
	cfg := Config{
		BinaryPath: "/opt/Pair Room/pairroom", WorkDir: "/srv/Pair Room",
		LogFile: "/tmp/pair room.log", Args: []string{"service", "--token", "value%with space"},
		LogMaxSize: 10 * 1024 * 1024, LogBackups: 3, StopTimeout: 31 * time.Minute,
		EnvPATH: "/usr/local/bin:/usr/bin", EnvExtra: map[string]string{"HTTPS_PROXY": "http://a\"b\nnext\tvalue"},
	}
	unit := (&systemdManager{}).buildUnit(cfg)
	for _, expected := range []string{
		`ExecStart="/opt/Pair Room/pairroom" "service" "--token" "value%%with space"`,
		`WorkingDirectory="/srv/Pair Room"`,
		`TimeoutStopSec=1860s`,
		`Environment="PAIRROOM_LOG_FILE=/tmp/pair room.log"`,
		`Environment="PATH=/usr/local/bin:/usr/bin"`,
		`Environment="HTTPS_PROXY=http://a\"b\nnext\tvalue"`,
	} {
		if !strings.Contains(unit, expected) {
			t.Fatalf("unit missing %q:\n%s", expected, unit)
		}
	}
	if strings.Contains(unit, "StandardOutput=append:") || strings.Contains(unit, "StandardError=append:") {
		t.Fatalf("systemd must not hold the rotating log file open:\n%s", unit)
	}
}

func TestSystemdUninstallPreservesUnitWhenStopFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manager := &systemdManager{}
	path := manager.unitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[Service]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := runSystemctl
	t.Cleanup(func() { runSystemctl = original })
	runSystemctl = func(args ...string) (string, error) {
		return "permission denied", errors.New("failed")
	}
	if err := manager.Uninstall(); err == nil {
		t.Fatal("Uninstall succeeded after systemd stop failure")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unit was removed despite stop failure: %v", err)
	}
}
