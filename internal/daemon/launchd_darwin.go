//go:build darwin

package daemon

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const launchdLabel = "com.sean2077.pairroom"

type launchdManager struct{}

var runLaunchctl = func(args ...string) (string, error) {
	command := exec.Command("launchctl", args...)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func newPlatformManager() (Manager, error) {
	return &launchdManager{}, nil
}

func (*launchdManager) Platform() string { return "launchd" }

func (m *launchdManager) Install(cfg Config) error {
	path := launchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o700); err != nil {
		return fmt.Errorf("create daemon log directory: %w", err)
	}
	if err := os.Remove(cfg.ControlFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear daemon control file: %w", err)
	}
	if err := bootoutLoadedLaunchdTarget(); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(buildLaunchdPlist(cfg)), 0o600); err != nil {
		return fmt.Errorf("write launchd plist: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("protect launchd plist: %w", err)
	}
	domain := preferredLaunchdDomain()
	if output, err := runLaunchctl("bootstrap", domain, path); err != nil {
		return fmt.Errorf("launchctl bootstrap: %s (%w)", output, err)
	}
	if output, err := runLaunchctl("kickstart", "-kp", launchdTarget(domain)); err != nil {
		return fmt.Errorf("launchctl kickstart: %s (%w)", output, err)
	}
	return nil
}

func (*launchdManager) Uninstall() error {
	if err := bootoutLoadedLaunchdTarget(); err != nil {
		return err
	}
	if err := os.Remove(launchdPlistPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove launchd plist: %w", err)
	}
	return nil
}

func (*launchdManager) Start() error {
	if err := RemoveControlFile(); err != nil {
		return err
	}
	if _, target, _, ok := loadedLaunchdTarget(); ok {
		output, err := runLaunchctl("kickstart", "-kp", target)
		if err != nil {
			return fmt.Errorf("start launchd service: %s (%w)", output, err)
		}
		return nil
	}
	domain := preferredLaunchdDomain()
	if output, err := runLaunchctl("bootstrap", domain, launchdPlistPath()); err != nil {
		return fmt.Errorf("bootstrap launchd service: %s (%w)", output, err)
	}
	return nil
}

func (*launchdManager) Stop() error {
	return bootoutLoadedLaunchdTarget()
}

func (m *launchdManager) Restart() error {
	if err := RemoveControlFile(); err != nil {
		return err
	}
	domain := preferredLaunchdDomain()
	if loadedDomain, _, _, ok := loadedLaunchdTarget(); ok {
		domain = loadedDomain
	}
	if err := bootoutLoadedLaunchdTarget(); err != nil {
		return err
	}
	var output string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		output, err = runLaunchctl("bootstrap", domain, launchdPlistPath())
		if err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("restart launchd service: %s (%w)", output, err)
	}
	output, err = runLaunchctl("kickstart", "-kp", launchdTarget(domain))
	if err != nil {
		return fmt.Errorf("restart launchd service: %s (%w)", output, err)
	}
	return nil
}

func (*launchdManager) Status() (*Status, error) {
	status := &Status{Platform: "launchd"}
	if _, err := os.Stat(launchdPlistPath()); err != nil {
		if os.IsNotExist(err) {
			return status, nil
		}
		return nil, fmt.Errorf("stat launchd plist: %w", err)
	}
	status.Installed = true
	_, _, output, ok := loadedLaunchdTarget()
	if !ok {
		return status, nil
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "pid = ") {
			if pid, err := strconv.Atoi(strings.TrimPrefix(line, "pid = ")); err == nil && pid > 0 {
				status.PID = pid
				status.Running = true
			}
		}
		if strings.Contains(line, "state = running") {
			status.Running = true
		}
	}
	return status, nil
}

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func launchdUserDomain() string { return fmt.Sprintf("user/%d", os.Getuid()) }
func launchdGUIDomain() string  { return fmt.Sprintf("gui/%d", os.Getuid()) }

func preferredLaunchdDomain() string {
	domain := launchdGUIDomain()
	if _, err := runLaunchctl("print", domain); err == nil {
		return domain
	}
	return launchdUserDomain()
}

func launchdDomains() []string {
	preferred := preferredLaunchdDomain()
	other := launchdGUIDomain()
	if preferred == other {
		other = launchdUserDomain()
	}
	return []string{preferred, other}
}

func launchdTarget(domain string) string { return domain + "/" + launchdLabel }

func launchdTargets() []string {
	result := make([]string, 0, 2)
	for _, domain := range launchdDomains() {
		result = append(result, launchdTarget(domain))
	}
	return result
}

func loadedLaunchdTarget() (string, string, string, bool) {
	for _, domain := range launchdDomains() {
		target := launchdTarget(domain)
		if output, err := runLaunchctl("print", target); err == nil {
			return domain, target, output, true
		}
	}
	return "", "", "", false
}

func bootoutLoadedLaunchdTarget() error {
	_, target, _, ok := loadedLaunchdTarget()
	if !ok {
		return nil
	}
	output, err := runLaunchctl("bootout", target)
	if err != nil {
		return fmt.Errorf("stop launchd service: %s (%w)", output, err)
	}
	return nil
}

func buildLaunchdPlist(cfg Config) string {
	var arguments strings.Builder
	for _, argument := range append([]string{cfg.BinaryPath}, cfg.Args...) {
		fmt.Fprintf(&arguments, "\t\t<string>%s</string>\n", xmlEscape(argument))
	}
	var environment strings.Builder
	for _, pair := range SortedEnvironment(cfg) {
		fmt.Fprintf(&environment, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", xmlEscape(pair[0]), xmlEscape(pair[1]))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>WorkingDirectory</key>
	<string>%s</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict><key>SuccessfulExit</key><false/></dict>
	<key>ExitTimeOut</key>
	<integer>%d</integer>
	<key>EnvironmentVariables</key>
	<dict>
%s	</dict>
	<key>StandardOutPath</key>
	<string>/dev/null</string>
	<key>StandardErrorPath</key>
	<string>/dev/null</string>
</dict>
</plist>
`, launchdLabel, arguments.String(), xmlEscape(cfg.WorkDir), durationSeconds(cfg.StopTimeout), environment.String())
}

func xmlEscape(value string) string {
	var builder strings.Builder
	_ = xml.EscapeText(&builder, []byte(value))
	return builder.String()
}

func CheckLinger() (bool, string) { return true, "" }
