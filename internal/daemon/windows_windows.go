//go:build windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	windowsTaskName   = "PairRoom Service"
	windowsScriptName = "pairroom-daemon.ps1"
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
	return m.Start()
}

func (m *taskSchedulerManager) Uninstall() error {
	status, err := m.Status()
	if err != nil {
		return err
	}
	if status != nil && status.Running {
		if err := m.Stop(); err != nil {
			return err
		}
	}
	if err := deleteWindowsTask(); err != nil {
		return err
	}
	path, err := windowsTaskScriptPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove scheduled-task script: %w", err)
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
		return nil
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
			return nil
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
	output, err := runPowerShell(fmt.Sprintf(`
$task = Get-ScheduledTask -TaskName %s -ErrorAction SilentlyContinue
if ($null -eq $task) { Write-Output 'NOT_FOUND'; exit 0 }
$expectedArgs = %s
$owned = $false
foreach ($action in $task.Actions) {
	if (([IO.Path]::GetFileName($action.Execute) -ieq 'powershell.exe') -and ($action.Arguments -eq $expectedArgs)) { $owned = $true }
}
if (-not $owned) { Write-Output 'CONFLICT'; exit 0 }
Write-Output $task.State
`, powerShellLiteral(windowsTaskName), powerShellLiteral(windowsTaskActionArguments(scriptPath))))
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

func buildWindowsTaskScript(cfg Config) string {
	var builder strings.Builder
	builder.WriteString("$ErrorActionPreference = 'Stop'\r\n")
	for _, pair := range SortedEnvironment(cfg) {
		fmt.Fprintf(&builder, "$env:%s = %s\r\n", pair[0], powerShellLiteral(pair[1]))
	}
	fmt.Fprintf(&builder, "Set-Location -LiteralPath %s\r\n", powerShellLiteral(cfg.WorkDir))
	builder.WriteString("for ($attempt = 1; $attempt -le 3; $attempt++) {\r\n")
	fmt.Fprintf(&builder, "  & %s", powerShellLiteral(cfg.BinaryPath))
	for _, argument := range cfg.Args {
		fmt.Fprintf(&builder, " %s", powerShellLiteral(argument))
	}
	builder.WriteString("\r\n")
	builder.WriteString("  $exitCode = $LASTEXITCODE\r\n")
	fmt.Fprintf(&builder, "  if (Test-Path -LiteralPath %s) { Remove-Item -LiteralPath %s -Force; exit $exitCode }\r\n", powerShellLiteral(cfg.ControlFile), powerShellLiteral(cfg.ControlFile))
	builder.WriteString("  if ($exitCode -eq 0) { exit 0 }\r\n")
	builder.WriteString("  if ($attempt -lt 3) { Start-Sleep -Seconds 10 }\r\n")
	builder.WriteString("}\r\n")
	builder.WriteString("exit $exitCode\r\n")
	return builder.String()
}

func createWindowsTask(scriptPath string) error {
	arguments := windowsTaskActionArguments(scriptPath)
	output, err := runPowerShell(fmt.Sprintf(`
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument %s
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
	return fmt.Sprintf(`-WindowStyle Hidden -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "%s"`, scriptPath)
}

func powerShellLiteral(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func CheckLinger() (bool, string) { return true, "" }
