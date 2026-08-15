//go:build linux

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const systemdServiceName = ServiceName + ".service"

type systemdManager struct {
	system bool
}

var runSystemctl = func(args ...string) (string, error) {
	command := exec.Command("systemctl", args...)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func newPlatformManager() (Manager, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil, errorsForMissingSystemd()
	}
	system := os.Getuid() == 0
	if err := checkSystemdRunning(system); err != nil {
		return nil, err
	}
	return &systemdManager{system: system}, nil
}

func (m *systemdManager) Platform() string {
	if m.system {
		return "systemd (system)"
	}
	return "systemd (user)"
}

func (m *systemdManager) Install(cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(m.unitPath()), 0o700); err != nil {
		return fmt.Errorf("create systemd unit directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o700); err != nil {
		return fmt.Errorf("create daemon log directory: %w", err)
	}
	if err := os.Remove(cfg.ControlFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear daemon control file: %w", err)
	}
	if err := os.WriteFile(m.unitPath(), []byte(m.buildUnit(cfg)), 0o600); err != nil {
		return fmt.Errorf("write systemd unit: %w", err)
	}
	if err := os.Chmod(m.unitPath(), 0o600); err != nil {
		return fmt.Errorf("protect systemd unit: %w", err)
	}
	for _, arguments := range [][]string{
		m.systemctlArgs("daemon-reload"),
		m.systemctlArgs("enable", systemdServiceName),
		m.systemctlArgs("restart", systemdServiceName),
	} {
		if output, err := runSystemctl(arguments...); err != nil {
			return fmt.Errorf("systemctl %s: %s (%w)", strings.Join(arguments, " "), output, err)
		}
	}
	return nil
}

func (m *systemdManager) Uninstall() error {
	if output, err := runSystemctl(m.systemctlArgs("disable", "--now", systemdServiceName)...); err != nil {
		return fmt.Errorf("stop and disable systemd service: %s (%w)", output, err)
	}
	if err := os.Remove(m.unitPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove systemd unit: %w", err)
	}
	if output, err := runSystemctl(m.systemctlArgs("daemon-reload")...); err != nil {
		return fmt.Errorf("reload systemd after uninstall: %s (%w)", output, err)
	}
	return nil
}

func (m *systemdManager) Start() error {
	if err := RemoveControlFile(); err != nil {
		return err
	}
	output, err := runSystemctl(m.systemctlArgs("start", systemdServiceName)...)
	if err != nil {
		return fmt.Errorf("start systemd service: %s (%w)", output, err)
	}
	return nil
}

func (m *systemdManager) Stop() error {
	output, err := runSystemctl(m.systemctlArgs("stop", systemdServiceName)...)
	if err != nil {
		return fmt.Errorf("stop systemd service: %s (%w)", output, err)
	}
	return nil
}

func (m *systemdManager) Restart() error {
	if err := RemoveControlFile(); err != nil {
		return err
	}
	output, err := runSystemctl(m.systemctlArgs("restart", systemdServiceName)...)
	if err != nil {
		return fmt.Errorf("restart systemd service: %s (%w)", output, err)
	}
	return nil
}

func (m *systemdManager) Status() (*Status, error) {
	status := &Status{Platform: m.Platform()}
	if _, err := os.Stat(m.unitPath()); err != nil {
		if os.IsNotExist(err) {
			return status, nil
		}
		return nil, fmt.Errorf("stat systemd unit: %w", err)
	}
	status.Installed = true
	output, err := runSystemctl(m.systemctlArgs("show", systemdServiceName, "--no-page", "--property", "ActiveState,MainPID")...)
	if err != nil {
		return status, nil
	}
	properties := parseSystemdProperties(output)
	status.Running = strings.EqualFold(properties["ActiveState"], "active")
	if pid, err := strconv.Atoi(properties["MainPID"]); err == nil && pid > 0 {
		status.PID = pid
	}
	return status, nil
}

func (m *systemdManager) systemctlArgs(args ...string) []string {
	if m.system {
		return args
	}
	return append([]string{"--user"}, args...)
}

func (m *systemdManager) unitPath() string {
	if m.system {
		return filepath.Join("/etc/systemd/system", systemdServiceName)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", systemdServiceName)
}

func (m *systemdManager) buildUnit(cfg Config) string {
	arguments := append([]string{cfg.BinaryPath}, cfg.Args...)
	quoted := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		quoted = append(quoted, quoteSystemdExecArgument(argument))
	}
	var builder strings.Builder
	builder.WriteString("[Unit]\n")
	builder.WriteString("Description=PairRoom local Claude Code and Codex coordination service\n")
	builder.WriteString("After=network-online.target\n")
	builder.WriteString("Wants=network-online.target\n\n")
	builder.WriteString("[Service]\n")
	builder.WriteString("Type=simple\n")
	fmt.Fprintf(&builder, "ExecStart=%s\n", strings.Join(quoted, " "))
	fmt.Fprintf(&builder, "WorkingDirectory=\"%s\"\n", escapeSystemdValue(cfg.WorkDir))
	builder.WriteString("Restart=on-failure\n")
	builder.WriteString("RestartSec=10\n")
	fmt.Fprintf(&builder, "TimeoutStopSec=%ds\n", durationSeconds(cfg.StopTimeout))
	for _, pair := range SortedEnvironment(cfg) {
		fmt.Fprintf(&builder, "Environment=\"%s=%s\"\n", pair[0], escapeSystemdValue(pair[1]))
	}
	builder.WriteString("\n[Install]\n")
	if m.system {
		builder.WriteString("WantedBy=multi-user.target\n")
	} else {
		builder.WriteString("WantedBy=default.target\n")
	}
	return builder.String()
}

func quoteSystemdExecArgument(value string) string {
	return "\"" + escapeSystemdValue(value) + "\""
}

func escapeSystemdValue(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			builder.WriteString(`\\`)
		case '"':
			builder.WriteString(`\"`)
		case '%':
			builder.WriteString(`%%`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func parseSystemdProperties(output string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

func checkSystemdRunning(system bool) error {
	arguments := []string{"is-system-running"}
	if !system {
		arguments = append([]string{"--user"}, arguments...)
	}
	output, _ := runSystemctl(arguments...)
	state := strings.ToLower(strings.TrimSpace(output))
	switch state {
	case "running", "degraded", "starting", "initializing":
		return nil
	}
	if strings.Contains(strings.ToLower(readProcVersion()), "microsoft") || strings.Contains(strings.ToLower(readProcVersion()), "wsl") {
		return fmt.Errorf("systemd is not active in this WSL instance; enable systemd in /etc/wsl.conf before installing the PairRoom daemon")
	}
	if system {
		return fmt.Errorf("systemd system manager is unavailable (state %q)", state)
	}
	return fmt.Errorf("systemd user manager is unavailable (state %q); use a login session with user systemd or install as root", state)
}

func errorsForMissingSystemd() error {
	return fmt.Errorf("systemctl not found: PairRoom daemon management on Linux requires systemd")
}

func readProcVersion() string {
	data, _ := os.ReadFile("/proc/version")
	return string(data)
}

func CheckLinger() (bool, string) {
	user := os.Getenv("USER")
	if os.Getuid() == 0 {
		return true, user
	}
	output, err := exec.Command("loginctl", "show-user", user, "-p", "Linger").Output()
	return err == nil && strings.TrimSpace(string(output)) == "Linger=yes", user
}
