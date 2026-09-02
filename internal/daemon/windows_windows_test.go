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
		`Option Explicit`,
		`Set shell = CreateObject("WScript.Shell")`,
		`Set processEnv = shell.Environment("Process")`,
		`processEnv("PATH") = "C:\Tools;C:\Program Files\Git\cmd"`,
		`processEnv("PAIRROOM_LOG_FILE") = "C:\Users\me\Pair Room\service.log"`,
		`processEnv("PAIRROOM_DETACH_CONSOLE") = "1"`,
		`processEnv("HTTPS_PROXY") = "http://127.0.0.1:7890"`,
		`shell.CurrentDirectory = "C:\Users\me\Pair Room"`,
		`exitCode = shell.Run("""C:\Program Files\PairRoom\pairroom.exe"" service --config C:\Users\me\pair'room.json --no-browser", 0, True)`,
		`If fileSystem.FileExists("C:\Users\me\Pair Room\daemon.stop") Then`,
		`WScript.Quit exitCode`,
		`If exitCode = 0 Then WScript.Quit 0`,
		`WScript.Sleep 10000`,
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("script missing %q:\n%s", expected, script)
		}
	}
	if strings.Contains(strings.ToLower(script), "powershell") {
		t.Fatalf("task launcher still invokes PowerShell:\n%s", script)
	}
}

func TestWindowsCommandLineEscapesArguments(t *testing.T) {
	got := windowsCommandLine(`C:\Program Files\PairRoom\pairroom.exe`, []string{"service", `--name=quoted"value`, ""})
	want := `"C:\Program Files\PairRoom\pairroom.exe" service --name=quoted\"value ""`
	if got != want {
		t.Fatalf("windowsCommandLine() = %q, want %q", got, want)
	}
}

func TestWindowsTaskActionUsesWindowlessScriptHost(t *testing.T) {
	got := windowsTaskAction(`C:\Users\me\pairroom-daemon.vbs`)
	for _, expected := range []string{
		`wscript.exe`,
		`//B`,
		`//NoLogo`,
		`"C:\Users\me\pairroom-daemon.vbs"`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("windowsTaskAction() missing %q: %q", expected, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "powershell") {
		t.Fatalf("windowsTaskAction() still launches PowerShell: %q", got)
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

func TestTaskSchedulerInstallReplacesLegacyLauncher(t *testing.T) {
	root := t.TempDir()
	setTestUserConfigDir(t, root)
	dataDir, err := DefaultDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPath, err := legacyWindowsTaskScriptPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("legacy\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	original := runPowerShell
	t.Cleanup(func() { runPowerShell = original })
	runPowerShell = func(string) (string, error) { return "NOT_FOUND", nil }
	cfg := Config{
		BinaryPath:  `C:\Tools\pairroom.exe`,
		WorkDir:     root,
		LogFile:     filepath.Join(root, "service.log"),
		ControlFile: filepath.Join(root, "daemon.stop"),
		Args:        []string{"service", "--no-browser"},
	}
	if err := (&taskSchedulerManager{}).Install(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy launcher still exists: %v", err)
	}
	currentPath, err := windowsTaskScriptPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(currentPath); err != nil {
		t.Fatalf("windowless launcher was not written: %v", err)
	}
}

func TestStopWindowsLaunchersCleansCurrentAndLegacyHosts(t *testing.T) {
	root := t.TempDir()
	setTestUserConfigDir(t, root)
	original := runPowerShell
	t.Cleanup(func() { runPowerShell = original })
	var script string
	runPowerShell = func(value string) (string, error) {
		script = value
		return "", nil
	}
	if err := stopWindowsLaunchers(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`pairroom-daemon.vbs`,
		`pairroom-daemon.ps1`,
		`Get-CimInstance Win32_Process`,
		`$_.ProcessId -ne $PID`,
		`Stop-Process -Id $processId -Force`,
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("launcher cleanup missing %q:\n%s", expected, script)
		}
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
	if err := createWindowsTask(`C:\Users\me\pairroom-daemon.vbs`); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`-ExecutionTimeLimit ([TimeSpan]::Zero)`,
		`-MultipleInstances IgnoreNew`,
		`-StartWhenAvailable`,
		`-Settings $settings`,
		`-Execute 'wscript.exe'`,
		`//B //NoLogo "C:\Users\me\pairroom-daemon.vbs"`,
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

func TestTaskSchedulerStatusRecognizesLegacyPowerShellTask(t *testing.T) {
	root := t.TempDir()
	setTestUserConfigDir(t, root)
	original := runPowerShell
	t.Cleanup(func() { runPowerShell = original })
	var script string
	runPowerShell = func(value string) (string, error) {
		script = value
		return "Ready", nil
	}
	status, err := (&taskSchedulerManager{}).Status()
	if err != nil {
		t.Fatal(err)
	}
	if status == nil || !status.Installed || status.Running {
		t.Fatalf("legacy task status = %#v, want installed and stopped", status)
	}
	for _, expected := range []string{
		`wscript.exe`,
		`//B //NoLogo`,
		`powershell.exe`,
		`pairroom-daemon.ps1`,
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("status query missing %q:\n%s", expected, script)
		}
	}
}

func TestVBScriptLiteralEscapesDoubleQuotes(t *testing.T) {
	got := vbScriptLiteral(`C:\Users\Jane "quoted"`)
	want := `"C:\Users\Jane ""quoted"""`
	if got != want {
		t.Fatalf("vbScriptLiteral() = %q, want %q", got, want)
	}
}
