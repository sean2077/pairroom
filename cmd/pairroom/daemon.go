package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/daemon"
	"github.com/sean2077/pairroom/internal/openbrowser"
	"github.com/sean2077/pairroom/internal/service"
)

var newDaemonManager = daemon.NewManager
var openManagementBrowser = openbrowser.Open

func runDaemon(args []string) error {
	if len(args) == 0 {
		printDaemonHelp()
		return nil
	}
	switch args[0] {
	case "install":
		return daemonInstall(args[1:])
	case "uninstall":
		return daemonUninstall(args[1:])
	case "start":
		return daemonStart(args[1:])
	case "stop":
		return daemonStop(args[1:])
	case "restart":
		return daemonRestart(args[1:])
	case "status":
		return daemonStatus(args[1:])
	case "logs":
		return daemonLogs(args[1:])
	case "open":
		return daemonOpen(args[1:])
	case "help", "--help", "-h":
		printDaemonHelp()
		return nil
	default:
		return fmt.Errorf("unknown daemon command %q (use pairroom daemon help)", args[0])
	}
}

func daemonInstall(args []string) error {
	cfg, force, help, err := parseDaemonInstallArgs(args)
	if err != nil {
		return err
	}
	if help {
		printDaemonHelp()
		return nil
	}
	if err := daemon.Resolve(&cfg); err != nil {
		return err
	}
	if err := normalizeDaemonServiceArgs(&cfg); err != nil {
		return err
	}
	manager, err := newDaemonManager()
	if err != nil {
		return err
	}
	status, err := manager.Status()
	if err != nil {
		return err
	}
	if status != nil && status.Installed && !force {
		return errors.New("PairRoom daemon is already installed; use --force to replace its service definition")
	}
	if err := manager.Install(cfg); err != nil {
		return err
	}
	meta := &daemon.Meta{
		LogFile: cfg.LogFile, LogMaxSize: cfg.LogMaxSize, LogBackups: cfg.LogBackups, ControlFile: cfg.ControlFile, WorkDir: cfg.WorkDir,
		DataRoot: flagValue(cfg.Args, "--data-root"), StopTimeoutSeconds: int64(cfg.StopTimeout / time.Second), BinaryPath: cfg.BinaryPath,
		Platform: manager.Platform(), InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := daemon.SaveMeta(meta); err != nil {
		fmt.Fprintln(os.Stderr, "warning: daemon installed but metadata could not be saved:", err)
	}
	fmt.Println("PairRoom daemon installed and started.")
	fmt.Printf("  platform: %s\n", manager.Platform())
	fmt.Printf("  binary:   %s\n", cfg.BinaryPath)
	fmt.Printf("  log:      %s\n", cfg.LogFile)
	fmt.Printf("  rotation: %d bytes, %d backups\n", cfg.LogMaxSize, cfg.LogBackups)
	fmt.Println("  open:     pairroom daemon open")
	if strings.Contains(manager.Platform(), "user") {
		if enabled, user := daemon.CheckLinger(); !enabled {
			fmt.Fprintf(os.Stderr, "warning: systemd linger is disabled; run 'sudo loginctl enable-linger %s' to keep PairRoom running after logout\n", user)
		}
	}
	return nil
}

func parseDaemonInstallArgs(args []string) (daemon.Config, bool, bool, error) {
	var cfg daemon.Config
	var serviceArgs []string
	force := false
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			serviceArgs = append(serviceArgs, args[index+1:]...)
			break
		}
		switch {
		case argument == "--help" || argument == "-h":
			return daemon.Config{}, false, true, nil
		case argument == "--force":
			force = true
		case argument == "--binary":
			value, next, err := daemonFlagValue(args, index, argument)
			if err != nil {
				return daemon.Config{}, false, false, err
			}
			cfg.BinaryPath, index = value, next
		case strings.HasPrefix(argument, "--binary="):
			cfg.BinaryPath = strings.TrimPrefix(argument, "--binary=")
		case argument == "--work-dir":
			value, next, err := daemonFlagValue(args, index, argument)
			if err != nil {
				return daemon.Config{}, false, false, err
			}
			cfg.WorkDir, index = value, next
		case strings.HasPrefix(argument, "--work-dir="):
			cfg.WorkDir = strings.TrimPrefix(argument, "--work-dir=")
		case argument == "--log-file":
			value, next, err := daemonFlagValue(args, index, argument)
			if err != nil {
				return daemon.Config{}, false, false, err
			}
			cfg.LogFile, index = value, next
		case strings.HasPrefix(argument, "--log-file="):
			cfg.LogFile = strings.TrimPrefix(argument, "--log-file=")
		case argument == "--log-max-size":
			value, next, err := daemonFlagValue(args, index, argument)
			if err != nil {
				return daemon.Config{}, false, false, err
			}
			cfg.LogMaxSize, err = daemon.ParseLogSize(value)
			if err != nil {
				return daemon.Config{}, false, false, err
			}
			index = next
		case strings.HasPrefix(argument, "--log-max-size="):
			value := strings.TrimPrefix(argument, "--log-max-size=")
			var err error
			cfg.LogMaxSize, err = daemon.ParseLogSize(value)
			if err != nil {
				return daemon.Config{}, false, false, err
			}
		case argument == "--log-max-backups":
			value, next, err := daemonFlagValue(args, index, argument)
			if err != nil {
				return daemon.Config{}, false, false, err
			}
			cfg.LogBackups, err = daemon.ParseLogBackups(value)
			if err != nil {
				return daemon.Config{}, false, false, err
			}
			index = next
		case strings.HasPrefix(argument, "--log-max-backups="):
			value := strings.TrimPrefix(argument, "--log-max-backups=")
			var err error
			cfg.LogBackups, err = daemon.ParseLogBackups(value)
			if err != nil {
				return daemon.Config{}, false, false, err
			}
		default:
			serviceArgs = append(serviceArgs, argument)
		}
	}
	cfg.Args = append([]string{"service"}, serviceArgs...)
	return cfg, force, false, nil
}

func daemonFlagValue(args []string, index int, name string) (string, int, error) {
	next := index + 1
	if next >= len(args) || strings.HasPrefix(args[next], "--") {
		return "", index, fmt.Errorf("missing value for %s", name)
	}
	return args[next], next, nil
}

func normalizeDaemonServiceArgs(cfg *daemon.Config) error {
	if len(cfg.Args) == 0 || cfg.Args[0] != "service" {
		return errors.New("daemon must run the pairroom service command")
	}
	args := append([]string(nil), cfg.Args[1:]...)
	for _, argument := range args {
		if argument == "service" {
			return errors.New("daemon install accepts service options, not a second service command")
		}
		if argument == "--daemon-control-file" || strings.HasPrefix(argument, "--daemon-control-file=") {
			return errors.New("daemon-control-file is managed internally")
		}
	}
	var err error
	args, err = absolutizeServiceFlag(args, "--config", cfg.WorkDir)
	if err != nil {
		return err
	}
	args, err = absolutizeServiceFlag(args, "--data-root", cfg.WorkDir)
	if err != nil {
		return err
	}
	if configPath := flagValue(args, "--config"); configPath != "" {
		if _, err := config.Load(configPath); err != nil {
			return err
		}
	}
	if value := flagValue(args, "--shutdown-timeout"); value != "" {
		shutdownTimeout, err := time.ParseDuration(value)
		if err != nil || shutdownTimeout <= 0 {
			return fmt.Errorf("invalid service shutdown-timeout %q", value)
		}
		if shutdownTimeout > time.Duration(1<<63-1)-time.Minute {
			return errors.New("service shutdown-timeout is too large")
		}
		cfg.StopTimeout = shutdownTimeout + time.Minute
	}
	args = append(args, "--no-browser", "--daemon-control-file", cfg.ControlFile)
	cfg.Args = append([]string{"service"}, args...)
	return nil
}

func absolutizeServiceFlag(args []string, name, base string) ([]string, error) {
	result := append([]string(nil), args...)
	for index := 0; index < len(result); index++ {
		argument := result[index]
		if argument == name {
			if index+1 >= len(result) {
				return nil, fmt.Errorf("missing value for %s", name)
			}
			absolute, err := absoluteDaemonArgument(base, result[index+1])
			if err != nil {
				return nil, err
			}
			result[index+1] = absolute
			index++
			continue
		}
		if strings.HasPrefix(argument, name+"=") {
			absolute, err := absoluteDaemonArgument(base, strings.TrimPrefix(argument, name+"="))
			if err != nil {
				return nil, err
			}
			result[index] = name + "=" + absolute
		}
	}
	return result, nil
}

func absoluteDaemonArgument(base, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("daemon service path must not be empty")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func flagValue(args []string, name string) string {
	value := ""
	for index, argument := range args {
		if argument == name && index+1 < len(args) {
			value = args[index+1]
		}
		if strings.HasPrefix(argument, name+"=") {
			value = strings.TrimPrefix(argument, name+"=")
		}
	}
	return value
}

func daemonUninstall(args []string) error {
	if err := noDaemonArgs("uninstall", args); err != nil {
		return err
	}
	manager, err := newDaemonManager()
	if err != nil {
		return err
	}
	status, err := manager.Status()
	if err != nil {
		return err
	}
	if status != nil && !status.Installed {
		_ = daemon.RemoveControlFile()
		_ = daemon.RemoveMeta()
		fmt.Println("PairRoom daemon is not installed.")
		return nil
	}
	if err := manager.Uninstall(); err != nil {
		return err
	}
	if err := errors.Join(daemon.RemoveControlFile(), daemon.RemoveMeta()); err != nil {
		return err
	}
	fmt.Println("PairRoom daemon uninstalled.")
	return nil
}

func daemonStart(args []string) error {
	recoverStale, err := parseRecoverFlag("start", args)
	if err != nil {
		return err
	}
	manager, status, err := installedDaemonManager()
	if err != nil {
		return err
	}
	if status.Running {
		fmt.Println("PairRoom daemon is already running.")
		return nil
	}
	if recoverStale {
		if err := recoverDaemonServiceLock(); err != nil {
			return err
		}
	}
	if err := manager.Start(); err != nil {
		return err
	}
	fmt.Println("PairRoom daemon started.")
	autoOpenDaemonManagementShell()
	return nil
}

func daemonStop(args []string) error {
	if err := noDaemonArgs("stop", args); err != nil {
		return err
	}
	manager, status, err := installedDaemonManager()
	if err != nil {
		return err
	}
	if !status.Running {
		fmt.Println("PairRoom daemon is already stopped.")
		return nil
	}
	if err := manager.Stop(); err != nil {
		return err
	}
	fmt.Println("PairRoom daemon stopped.")
	return nil
}

func daemonRestart(args []string) error {
	recoverStale, err := parseRecoverFlag("restart", args)
	if err != nil {
		return err
	}
	manager, status, err := installedDaemonManager()
	if err != nil {
		return err
	}
	if recoverStale {
		if status.Running {
			if err := manager.Stop(); err != nil {
				return err
			}
		}
		if err := recoverDaemonServiceLock(); err != nil {
			return err
		}
		if err := manager.Start(); err != nil {
			return err
		}
	} else if err := manager.Restart(); err != nil {
		return err
	}
	fmt.Println("PairRoom daemon restarted.")
	autoOpenDaemonManagementShell()
	return nil
}

func parseRecoverFlag(command string, args []string) (bool, error) {
	recoverStale := false
	for _, argument := range args {
		if argument != "--recover-stale-lock" {
			return false, fmt.Errorf("unknown daemon %s option %q", command, argument)
		}
		recoverStale = true
	}
	return recoverStale, nil
}

func recoverDaemonServiceLock() error {
	meta, err := daemon.LoadMeta()
	if err != nil {
		return fmt.Errorf("load daemon metadata before stale-lock recovery: %w", err)
	}
	return service.RecoverServiceLock(meta.DataRoot)
}

func installedDaemonManager() (daemon.Manager, *daemon.Status, error) {
	manager, err := newDaemonManager()
	if err != nil {
		return nil, nil, err
	}
	status, err := manager.Status()
	if err != nil {
		return nil, nil, err
	}
	if status == nil || !status.Installed {
		return nil, nil, errors.New("PairRoom daemon is not installed; run pairroom daemon install")
	}
	return manager, status, nil
}

func daemonStatus(args []string) error {
	if err := noDaemonArgs("status", args); err != nil {
		return err
	}
	manager, err := newDaemonManager()
	if err != nil {
		return err
	}
	status, err := manager.Status()
	if err != nil {
		return err
	}
	fmt.Println("PairRoom daemon status")
	if status == nil || !status.Installed {
		fmt.Println("  status:   not installed")
		fmt.Printf("  platform: %s\n", manager.Platform())
		return nil
	}
	state := "stopped"
	if status.Running {
		state = "running"
	}
	fmt.Printf("  status:   %s\n", state)
	fmt.Printf("  platform: %s\n", status.Platform)
	if status.PID > 0 {
		fmt.Printf("  pid:      %d\n", status.PID)
	}
	if status.Running {
		fmt.Println("  open:     pairroom daemon open")
	}
	if meta, err := daemon.LoadMeta(); err == nil {
		fmt.Printf("  log:      %s\n", meta.LogFile)
		if meta.LogMaxSize > 0 && meta.LogBackups > 0 {
			fmt.Printf("  rotation: %d bytes, %d backups\n", meta.LogMaxSize, meta.LogBackups)
		}
		if installed, err := time.Parse(time.RFC3339, meta.InstalledAt); err == nil {
			fmt.Printf("  installed:%s\n", installed.Local().Format(" 2006-01-02 15:04:05"))
		}
	}
	return nil
}

func daemonLogs(args []string) error {
	flags := flag.NewFlagSet("pairroom daemon logs", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	follow := flags.Bool("follow", false, "follow appended log output")
	flags.BoolVar(follow, "f", false, "follow appended log output")
	lines := flags.Int("n", 100, "number of trailing lines")
	logFile := flags.String("log-file", "", "override daemon log path")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected daemon logs arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *lines < 1 || *lines > 1_000_000 {
		return errors.New("daemon logs -n must be between 1 and 1000000")
	}
	path := *logFile
	if path == "" {
		if meta, err := daemon.LoadMeta(); err == nil {
			path = meta.LogFile
		} else {
			path, err = daemon.DefaultLogFile()
			if err != nil {
				return err
			}
		}
	}
	if err := printLastLogLines(os.Stdout, path, *lines); err != nil {
		return err
	}
	if !*follow {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return followLog(ctx, os.Stdout, path)
}

type managementAccess struct {
	browserURL string
	apiURL     string
	token      string
}

func daemonOpen(args []string) error {
	if err := noDaemonArgs("open", args); err != nil {
		return err
	}
	_, status, err := installedDaemonManager()
	if err != nil {
		return err
	}
	if !status.Running {
		return errors.New("PairRoom daemon is not running; run pairroom daemon start")
	}
	return openDaemonManagementShell()
}

// openDaemonManagementShell waits for the Management Shell started by the
// daemon and opens only the currently authenticated loopback URL. Lifecycle
// commands call this directly after starting the service so they do not race
// the service manager's status update.
func openDaemonManagementShell() error {
	meta, err := daemon.LoadMeta()
	if err != nil {
		return fmt.Errorf("load daemon metadata: %w", err)
	}
	backups := meta.LogBackups
	if backups < 1 {
		backups = daemon.DefaultLogMaxBackups
	}
	managementURL, err := waitForManagementURL(meta.LogFile, backups, 5*time.Second)
	if err != nil {
		return err
	}
	if err := openManagementBrowser(managementURL); err != nil {
		return fmt.Errorf("open Management Shell: %w", err)
	}
	fmt.Println("PairRoom Management Shell opened in the default browser.")
	return nil
}

// autoOpenDaemonManagementShell keeps a successful lifecycle operation
// successful even when the optional browser handoff is unavailable. The
// explicit `daemon open` command remains strict and can be used to retry.
func autoOpenDaemonManagementShell() {
	if err := openDaemonManagementShell(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not automatically open Management Shell: %v\n", err)
		fmt.Println("  open:     pairroom daemon open")
	}
}

func waitForManagementURL(logFile string, backups int, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	foundCandidate := false
	for {
		candidates, err := managementURLCandidates(logFile, backups)
		if err != nil {
			return "", err
		}
		for _, candidate := range candidates {
			access, err := parseManagementAccess(candidate)
			if err != nil {
				continue
			}
			foundCandidate = true
			if probeManagementAccess(ctx, access) {
				return access.browserURL, nil
			}
		}
		select {
		case <-ctx.Done():
			if foundCandidate {
				return "", errors.New("no logged Management Shell address authenticated the running daemon; run pairroom daemon restart")
			}
			return "", errors.New("Management Shell address is not available in the daemon log; run pairroom daemon restart")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func managementURLCandidates(logFile string, backups int) ([]string, error) {
	seen := make(map[string]struct{})
	var candidates []string
	for index := 0; index <= backups; index++ {
		path := logFile
		if index > 0 {
			path = fmt.Sprintf("%s.%d", logFile, index)
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read daemon log %s: %w", path, err)
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		for lineIndex := len(lines) - 1; lineIndex >= 0; lineIndex-- {
			line := strings.TrimSpace(lines[lineIndex])
			if !strings.HasPrefix(line, "management:") {
				continue
			}
			candidate := strings.TrimSpace(strings.TrimPrefix(line, "management:"))
			if candidate == "" {
				continue
			}
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

func parseManagementAccess(raw string) (managementAccess, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || (parsed.Path != "" && parsed.Path != "/") {
		return managementAccess{}, errors.New("invalid Management Shell address")
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return managementAccess{}, errors.New("invalid Management Shell listener")
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	portNumber, portErr := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return managementAccess{}, errors.New("Management Shell listener is not numeric loopback")
	}
	fragment, err := url.ParseQuery(parsed.Fragment)
	tokens := fragment["token"]
	if err != nil || len(fragment) != 1 || len(tokens) != 1 || strings.TrimSpace(tokens[0]) == "" {
		return managementAccess{}, errors.New("Management Shell address has no bootstrap token")
	}
	token := strings.TrimSpace(tokens[0])
	browser := url.URL{Scheme: "http", Host: parsed.Host, Path: "/"}
	values := url.Values{}
	values.Set("token", token)
	browser.Fragment = values.Encode()
	api := url.URL{Scheme: "http", Host: parsed.Host, Path: "/api/v1/service"}
	return managementAccess{browserURL: browser.String(), apiURL: api.String(), token: token}, nil
}

func probeManagementAccess(ctx context.Context, access managementAccess) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, access.apiURL, nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+access.token)
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	return response.StatusCode == http.StatusOK
}

func printLastLogLines(writer io.Writer, path string, count int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read daemon log %s: %w", path, err)
	}
	text := strings.TrimRight(string(data), "\r\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	_, err = fmt.Fprintln(writer, strings.Join(lines, "\n"))
	return err
}

func followLog(ctx context.Context, writer io.Writer, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat daemon log %s: %w", path, err)
	}
	offset := info.Size()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			nextInfo, err := os.Stat(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return err
			}
			if !os.SameFile(info, nextInfo) {
				offset = 0
				info = nextInfo
			} else if nextInfo.Size() < offset {
				offset = 0
			}
			if nextInfo.Size() == offset {
				continue
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			if _, err := file.Seek(offset, io.SeekStart); err != nil {
				_ = file.Close()
				return err
			}
			written, copyErr := io.Copy(writer, file)
			closeErr := file.Close()
			offset += written
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
}

func noDaemonArgs(command string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("daemon %s does not accept arguments: %s", command, strings.Join(args, " "))
	}
	return nil
}

func printDaemonHelp() {
	fmt.Print(`Usage: pairroom daemon <command> [options]

Commands:
  install     Install and start pairroom service with the OS service manager
  uninstall   Stop and remove the installed service
  start       Start the installed service and open the Management Shell
  stop        Gracefully drain and stop the installed service
  restart     Gracefully restart the installed service and open the Management Shell
  status      Show installation and process state
  logs        Show or follow daemon output
  open        Validate and open the current Management Shell

Install options:
  --binary PATH       PairRoom executable to install (default: current executable)
  --work-dir DIR      Stable working directory (default: current directory)
  --log-file PATH     Combined stdout/stderr log path
  --log-max-size SIZE Rotate after SIZE (default: 10MB; K/M/G suffixes accepted)
  --log-max-backups N Retain N rotated logs (default: 3)
  --force             Replace an existing service definition
  --                  Treat all remaining options as pairroom service options

Unrecognized install options are forwarded to pairroom service. The daemon
always adds --no-browser and an internal graceful-shutdown control file.

Start/restart options:
  --recover-stale-lock  Explicitly remove a verified crash-stale service.lock

Logs options:
  -n N                Show the last N lines (default: 100)
  -f, --follow        Follow appended output
  --log-file PATH     Read a specific log file

Supported platforms: Linux systemd, macOS launchd, Windows Task Scheduler.
`)
}
