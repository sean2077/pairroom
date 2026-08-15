//go:build windows

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildWindowsTaskScriptQuotesArgumentsAndRedirectsOutput(t *testing.T) {
	cfg := Config{
		BinaryPath:  `C:\Program Files\PairRoom\pairroom.exe`,
		WorkDir:     `C:\Users\me\Pair Room`,
		LogFile:     `C:\Users\me\Pair Room\service.log`,
		ControlFile: `C:\Users\me\Pair Room\daemon.stop`,
		LogMaxSize:  10 * 1024 * 1024,
		LogBackups:  3,
		Args:        []string{"service", "--config", `C:\Users\me\pair'room.json`, "--no-browser"},
		EnvPATH:     `C:\Tools;C:\Program Files\Git\cmd`,
		EnvExtra:    map[string]string{"HTTPS_PROXY": "http://127.0.0.1:7890"},
	}
	script := buildWindowsTaskScript(cfg)
	for _, expected := range []string{
		`$env:PATH = 'C:\Tools;C:\Program Files\Git\cmd'`,
		`$env:PAIRROOM_LOG_FILE = 'C:\Users\me\Pair Room\service.log'`,
		`$env:HTTPS_PROXY = 'http://127.0.0.1:7890'`,
		`Set-Location -LiteralPath 'C:\Users\me\Pair Room'`,
		`& 'C:\Program Files\PairRoom\pairroom.exe' 'service' '--config' 'C:\Users\me\pair''room.json' '--no-browser'`,
		`for ($attempt = 1; $attempt -le 3; $attempt++)`,
		`if (Test-Path -LiteralPath 'C:\Users\me\Pair Room\daemon.stop')`,
		`if ($exitCode -eq 0) { exit 0 }`,
		`Start-Sleep -Seconds 10`,
		`exit $exitCode`,
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("script missing %q:\n%s", expected, script)
		}
	}
}

func TestTaskSchedulerStopRequestsGracefulServiceShutdown(t *testing.T) {
	root := t.TempDir()
	setTestUserConfigDir(t, root)
	control := filepath.Join(root, "daemon.stop")
	if err := SaveMeta(&Meta{ControlFile: control}); err != nil {
		t.Fatal(err)
	}
	original := runPowerShell
	t.Cleanup(func() { runPowerShell = original })
	calls := 0
	runPowerShell = func(script string) (string, error) {
		calls++
		if calls == 1 {
			return "Running", nil
		}
		return "Ready", nil
	}
	manager := &taskSchedulerManager{}
	if err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(control); err != nil {
		t.Fatalf("graceful stop request was not written: %v", err)
	}
}

func TestTaskSchedulerStartClearsStaleStopRequest(t *testing.T) {
	root := t.TempDir()
	setTestUserConfigDir(t, root)
	control, err := DefaultControlFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(control), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(control, []byte("stop"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := runPowerShell
	t.Cleanup(func() { runPowerShell = original })
	runPowerShell = func(string) (string, error) { return "", nil }
	if err := (&taskSchedulerManager{}).Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(control); !os.IsNotExist(err) {
		t.Fatalf("stale control file still exists: %v", err)
	}
}

func TestCreateWindowsTaskDisablesExecutionLimit(t *testing.T) {
	original := runPowerShell
	t.Cleanup(func() { runPowerShell = original })
	var script string
	runPowerShell = func(value string) (string, error) {
		script = value
		return "", nil
	}
	if err := createWindowsTask(`C:\Users\me\pairroom-daemon.ps1`); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`-ExecutionTimeLimit ([TimeSpan]::Zero)`,
		`-MultipleInstances IgnoreNew`,
		`-StartWhenAvailable`,
		`-Settings $settings`,
		`-WindowStyle Hidden`,
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("scheduled task definition missing %q:\n%s", expected, script)
		}
	}
}

func TestTaskSchedulerStatusRejectsConflictingTask(t *testing.T) {
	root := t.TempDir()
	setTestUserConfigDir(t, root)
	original := runPowerShell
	t.Cleanup(func() { runPowerShell = original })
	runPowerShell = func(string) (string, error) { return "CONFLICT", nil }
	status, err := (&taskSchedulerManager{}).Status()
	if err == nil || status != nil {
		t.Fatalf("conflicting task returned status=%#v err=%v", status, err)
	}
}
