//go:build windows

package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	windowsTaskName         = "PairRoom Service"
	windowsScriptName       = "pairroom-daemon.vbs"
	legacyWindowsScriptName = "pairroom-daemon.ps1"
)

type taskSchedulerManager struct{}

var runPowerShell = func(script string) (string, error) {
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference = 'Stop'\n"+script)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func newPlatformManager() (Manager, error) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		return nil, fmt.Errorf("powershell.exe not found: Windows Task Scheduler management requires PowerShell")
	}
	return &taskSchedulerManager{}, nil
}

func (*taskSchedulerManager) Platform() string { return "Task Scheduler" }

func (m *taskSchedulerManager) Install(cfg Config) error {
	if _, err := exec.LookPath("wscript.exe"); err != nil {
		return fmt.Errorf("wscript.exe not found: Windows daemon launcher requires Windows Script Host")
	}
	root, err := DefaultDataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create daemon data directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o700); err != nil {
		return fmt.Errorf("create daemon log directory: %w", err)
	}
	if err := os.Remove(cfg.ControlFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear daemon control file: %w", err)
	}
	path, err := windowsTaskScriptPath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(buildWindowsTaskScript(cfg)), 0o600); err != nil {
		return fmt.Errorf("write scheduled-task script: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("protect scheduled-task script: %w", err)
	}
	status, err := m.Status()
	if err != nil {
		return err
	}
	if status != nil && status.Installed {
		if err := m.Stop(); err != nil {
			return err
		}
		if err := deleteWindowsTask(); err != nil {
			return err
		}
	}
	if err := createWindowsTask(path); err != nil {
		return err
	}
	if err := m.Start(); err != nil {
		return err
	}
	legacyPath, err := legacyWindowsTaskScriptPath()
	if err != nil {
		return err
	}
	if err := removeWindowsTaskScript(legacyPath); err != nil {
		slog.Warn("task scheduler: remove legacy PowerShell launcher failed", "error", err)
	}
	return nil
}

func (m *taskSchedulerManager) Uninstall() error {
	status, err := m.Status()
	if err != nil {
		return err
	}
	if status != nil && status.Installed {
		if err := m.Stop(); err != nil {
			return err
		}
	}
	if err := deleteWindowsTask(); err != nil {
		return err
	}
	legacyPath, err := legacyWindowsTaskScriptPath()
	if err != nil {
		return err
	}
	path, err := windowsTaskScriptPath()
	if err != nil {
		return err
	}
	for _, path := range []string{path, legacyPath} {
		if err := removeWindowsTaskScript(path); err != nil {
			return fmt.Errorf("remove scheduled-task script: %w", err)
		}
	}
	return nil
}

func (*taskSchedulerManager) Start() error {
	if err := RemoveControlFile(); err != nil {
		return err
	}
	output, err := runPowerShell(fmt.Sprintf(`
$task = Get-ScheduledTask -TaskName %s -ErrorAction SilentlyContinue
if ($null -eq $task) { throw 'scheduled task is not installed' }
if ($task.State -ne 'Running') { Start-ScheduledTask -TaskName %s }
`, powerShellLiteral(windowsTaskName), powerShellLiteral(windowsTaskName)))
	if err != nil {
		return fmt.Errorf("start scheduled task: %s (%w)", output, err)
	}
	return nil
}

func (m *taskSchedulerManager) Stop() error {
	status, err := m.Status()
	if err != nil {
		return err
	}
	if status == nil || !status.Installed || !status.Running {
		return stopWindowsLaunchers()
	}
	if err := RequestStop(); err != nil {
		return err
	}
	deadline := time.Now().Add(installedStopTimeout())
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		status, err = m.Status()
		if err != nil {
			return err
		}
		if status == nil || !status.Running {
			return stopWindowsLaunchers()
		}
	}
	return fmt.Errorf("PairRoom is still draining active Turns; the scheduled task was left running to preserve graceful shutdown")
}

func installedStopTimeout() time.Duration {
	if meta, err := LoadMeta(); err == nil && meta.StopTimeoutSeconds > 0 {
		return time.Duration(meta.StopTimeoutSeconds) * time.Second
	}
	return 11 * time.Minute
}

func (m *taskSchedulerManager) Restart() error {
	if err := m.Stop(); err != nil {
		return err
	}
	return m.Start()
}

func (*taskSchedulerManager) Status() (*Status, error) {
	status := &Status{Platform: "Task Scheduler"}
	scriptPath, err := windowsTaskScriptPath()
	if err != nil {
		return nil, err
	}
	legacyScriptPath, err := legacyWindowsTaskScriptPath()
	if err != nil {
		return nil, err
	}
	output, err := runPowerShell(fmt.Sprintf(`
$task = Get-ScheduledTask -TaskName %s -ErrorAction SilentlyContinue
if ($null -eq $task) { Write-Output 'NOT_FOUND'; exit 0 }
$expectedArgs = @(%s, %s)
$owned = $false
foreach ($action in $task.Actions) {
	$execute = [IO.Path]::GetFileName([string]$action.Execute)
	if ((($execute -ieq 'wscript.exe') -and ($action.Arguments -eq $expectedArgs[0])) -or
		(($execute -ieq 'powershell.exe') -and ($action.Arguments -eq $expectedArgs[1]))) { $owned = $true }
}
if (-not $owned) { Write-Output 'CONFLICT'; exit 0 }
Write-Output $task.State
`, powerShellLiteral(windowsTaskName), powerShellLiteral(windowsTaskActionArguments(scriptPath)), powerShellLiteral(legacyWindowsTaskActionArguments(legacyScriptPath))))
	if err != nil {
		return nil, fmt.Errorf("query scheduled task: %s (%w)", output, err)
	}
	if strings.EqualFold(strings.TrimSpace(output), "NOT_FOUND") {
		return status, nil
	}
	if strings.EqualFold(strings.TrimSpace(output), "CONFLICT") {
		return nil, fmt.Errorf("scheduled task %q exists but is not owned by PairRoom", windowsTaskName)
	}
	status.Installed = true
	status.Running = strings.EqualFold(strings.TrimSpace(output), "Running")
	return status, nil
}

func windowsTaskScriptPath() (string, error) {
	root, err := DefaultDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, windowsScriptName), nil
}

func legacyWindowsTaskScriptPath() (string, error) {
	root, err := DefaultDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, legacyWindowsScriptName), nil
}

func removeWindowsTaskScript(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func stopWindowsLaunchers() error {
	currentPath, err := windowsTaskScriptPath()
	if err != nil {
		return err
	}
	legacyPath, err := legacyWindowsTaskScriptPath()
	if err != nil {
		return err
	}
	output, err := runPowerShell(fmt.Sprintf(`
$currentScript = %s
$legacyScript = %s
$launcherPids = @(
	Get-CimInstance Win32_Process -ErrorAction SilentlyContinue | Where-Object {
		$commandLine = [string]$_.CommandLine
		$_.ProcessId -ne $PID -and
		($_.Name -ieq 'wscript.exe' -or $_.Name -ieq 'powershell.exe') -and
		($commandLine.IndexOf($currentScript, [System.StringComparison]::OrdinalIgnoreCase) -ge 0 -or
		 $commandLine.IndexOf($legacyScript, [System.StringComparison]::OrdinalIgnoreCase) -ge 0)
	} | Select-Object -ExpandProperty ProcessId
)
foreach ($processId in ($launcherPids | Sort-Object -Unique)) {
	Stop-Process -Id $processId -Force -ErrorAction SilentlyContinue
	Wait-Process -Id $processId -Timeout 5 -ErrorAction SilentlyContinue
}
`, powerShellLiteral(currentPath), powerShellLiteral(legacyPath)))
	if err != nil {
		return fmt.Errorf("stop orphaned Windows daemon launchers: %s (%w)", output, err)
	}
	return nil
}

func windowsTaskAction(scriptPath string) string {
	return fmt.Sprintf(`wscript.exe %s`, windowsTaskActionArguments(scriptPath))
}

func buildWindowsTaskScript(cfg Config) string {
	var builder strings.Builder
	builder.WriteString("Option Explicit\r\n")
	builder.WriteString("Dim shell, processEnv, fileSystem, exitCode, attempt\r\n")
	builder.WriteString("Set shell = CreateObject(\"WScript.Shell\")\r\n")
	builder.WriteString("Set processEnv = shell.Environment(\"Process\")\r\n")
	builder.WriteString("Set fileSystem = CreateObject(\"Scripting.FileSystemObject\")\r\n")
	for _, pair := range SortedEnvironment(cfg) {
		fmt.Fprintf(&builder, "processEnv(%s) = %s\r\n", vbScriptLiteral(pair[0]), vbScriptLiteral(pair[1]))
	}
	fmt.Fprintf(&builder, "shell.CurrentDirectory = %s\r\n", vbScriptLiteral(cfg.WorkDir))
	builder.WriteString("For attempt = 1 To 3\r\n")
	fmt.Fprintf(&builder, "  exitCode = shell.Run(%s, 0, True)\r\n", vbScriptLiteral(windowsCommandLine(cfg.BinaryPath, cfg.Args)))
	fmt.Fprintf(&builder, "  If fileSystem.FileExists(%s) Then\r\n", vbScriptLiteral(cfg.ControlFile))
	fmt.Fprintf(&builder, "    fileSystem.DeleteFile %s, True\r\n", vbScriptLiteral(cfg.ControlFile))
	builder.WriteString("    WScript.Quit exitCode\r\n")
	builder.WriteString("  End If\r\n")
	builder.WriteString("  If exitCode = 0 Then WScript.Quit 0\r\n")
	builder.WriteString("  If attempt < 3 Then WScript.Sleep 10000\r\n")
	builder.WriteString("Next\r\n")
	builder.WriteString("WScript.Quit exitCode\r\n")
	return builder.String()
}

func windowsCommandLine(binaryPath string, args []string) string {
	var builder strings.Builder
	builder.WriteString(syscall.EscapeArg(binaryPath))
	for _, argument := range args {
		builder.WriteByte(' ')
		builder.WriteString(syscall.EscapeArg(argument))
	}
	return builder.String()
}

func vbScriptLiteral(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func createWindowsTask(scriptPath string) error {
	arguments := windowsTaskActionArguments(scriptPath)
	output, err := runPowerShell(fmt.Sprintf(`
$action = New-ScheduledTaskAction -Execute 'wscript.exe' -Argument %s
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -StartWhenAvailable
Register-ScheduledTask -TaskName %s -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null
`, powerShellLiteral(arguments), powerShellLiteral(windowsTaskName)))
	if err != nil {
		return fmt.Errorf("register scheduled task: %s (%w)", output, err)
	}
	return nil
}

func deleteWindowsTask() error {
	output, err := runPowerShell(fmt.Sprintf(`
$task = Get-ScheduledTask -TaskName %s -ErrorAction SilentlyContinue
if ($null -ne $task) { Unregister-ScheduledTask -TaskName %s -Confirm:$false }
`, powerShellLiteral(windowsTaskName), powerShellLiteral(windowsTaskName)))
	if err != nil {
		return fmt.Errorf("unregister scheduled task: %s (%w)", output, err)
	}
	return nil
}

func windowsTaskActionArguments(scriptPath string) string {
	return fmt.Sprintf(`//B //NoLogo "%s"`, scriptPath)
}

func legacyWindowsTaskActionArguments(scriptPath string) string {
	return fmt.Sprintf(`-WindowStyle Hidden -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "%s"`, scriptPath)
}

func powerShellLiteral(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func CheckLinger() (bool, string) { return true, "" }
