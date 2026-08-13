package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/openbrowser"
	"github.com/sean2077/pairroom/internal/room"
	"github.com/sean2077/pairroom/internal/server"
	"github.com/sean2077/pairroom/internal/store"
	"github.com/sean2077/pairroom/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pairroom:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printHelp()
		return nil
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "version", "--version", "-v":
		fmt.Println("pairroom", version.Current)
		return nil
	case "help", "--help", "-h":
		printHelp()
		return nil
	default:
		return fmt.Errorf("unknown command %q (use pairroom help)", args[0])
	}
}

func runServe(args []string) error {
	configPath := preparseValue(args, "--config")
	fileCfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	flags := flag.NewFlagSet("pairroom serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configFlag := flags.String("config", configPath, "JSON configuration file")
	repoFlag := flags.String("repo", ".", "repository/workspace directory")
	nameFlag := flags.String("name", fileCfg.RoomName, "room display name")
	listenFlag := flags.String("listen", fileCfg.Listen, "HTTP listen address")
	dataFlag := flags.String("data-dir", "", "room state directory (default: per-repository user config directory)")
	tokenFlag := flags.String("token", fileCfg.Token, "API bearer token; generated automatically for non-loopback binds")
	mockFlag := flags.Bool("mock", false, "run deterministic mock agents instead of vendor CLIs")
	noBrowserFlag := flags.Bool("no-browser", false, "do not open the room in a browser")
	autoStartFlag := flags.Bool("auto-start", fileCfg.AutoStart, "start both agents when the room opens")
	routingFlag := flags.String("routing", string(fileCfg.RoutingMode), "manual, mentions, or roundtable")
	maxHopsFlag := flags.Int("max-hops", fileCfg.MaxAgentHops, "maximum automatic agent hops")
	stallWarningFlag := flags.Int("stall-warning-seconds", fileCfg.StallWarningSeconds, "warn when a working agent emits no runtime event; -1 disables")
	claudeCommand := flags.String("claude-command", fileCfg.Claude.Command, "Claude Code executable")
	claudeModel := flags.String("claude-model", fileCfg.Claude.Model, "Claude model override")
	claudePermission := flags.String("claude-permission-mode", fileCfg.Claude.PermissionMode, "Claude permission mode")
	codexCommand := flags.String("codex-command", fileCfg.Codex.Command, "Codex executable")
	codexModel := flags.String("codex-model", fileCfg.Codex.Model, "Codex model override")
	codexEffort := flags.String("codex-effort", fileCfg.Codex.Effort, "Codex reasoning effort")
	codexApproval := flags.String("codex-approval-policy", fileCfg.Codex.ApprovalPolicy, "Codex approval policy")
	codexSandbox := flags.String("codex-sandbox", fileCfg.Codex.Sandbox, "Codex sandbox mode")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	_ = configFlag
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	repo, err := canonicalDirectory(*repoFlag)
	if err != nil {
		return err
	}
	dataDir := *dataFlag
	if dataDir == "" {
		dataDir, err = defaultDataDir(repo)
		if err != nil {
			return err
		}
	} else {
		dataDir, err = filepath.Abs(dataDir)
		if err != nil {
			return fmt.Errorf("resolve data directory: %w", err)
		}
	}
	routing := model.RoutingMode(*routingFlag)
	if !routing.Valid() {
		return fmt.Errorf("invalid routing mode %q", routing)
	}
	if *maxHopsFlag < 1 || *maxHopsFlag > 30 {
		return errors.New("max-hops must be between 1 and 30")
	}
	if *stallWarningFlag != -1 && (*stallWarningFlag < 30 || *stallWarningFlag > 86400) {
		return errors.New("stall-warning-seconds must be -1 or between 30 and 86400")
	}

	token := *tokenFlag
	if !isLoopbackListen(*listenFlag) && token == "" {
		token, err = randomToken()
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "warning: non-loopback HTTP has no TLS; use only on a trusted LAN or behind a secure tunnel")
	}

	eventStore, err := store.Open(dataDir)
	if err != nil {
		return err
	}
	attachmentStore, err := attachment.Open(dataDir, repo)
	if err != nil {
		_ = eventStore.Close()
		return err
	}
	claudeFactory := agent.ClaudeFactory
	codexFactory := agent.CodexFactory
	if *mockFlag {
		claudeFactory = agent.MockFactory
		codexFactory = agent.MockFactory
	}
	engine, err := room.New(room.Config{
		Name: *nameFlag,
		Repo: repo,
		Settings: model.RoomSettings{
			RoutingMode: routing, MaxHops: *maxHopsFlag, StallWarningSeconds: *stallWarningFlag,
		},
		Store:         eventStore,
		ClaudeFactory: claudeFactory,
		CodexFactory:  codexFactory,
		ClaudeConfig: agent.Config{
			ClientVersion: version.Current, Command: *claudeCommand, Model: *claudeModel, PermissionMode: *claudePermission,
		},
		CodexConfig: agent.Config{
			ClientVersion: version.Current, Command: *codexCommand, Model: *codexModel, Effort: *codexEffort,
			ApprovalPolicy: *codexApproval, Sandbox: *codexSandbox,
		},
		Attachments: attachmentStore,
		AutoStart:   *autoStartFlag,
	})
	if err != nil {
		_ = eventStore.Close()
		return err
	}
	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if err := engine.Start(rootCtx); err != nil {
		_ = engine.Close()
		return err
	}
	web, err := server.New(server.Config{Engine: engine, Repo: repo, Token: token, Attachments: attachmentStore})
	if err != nil {
		_ = engine.Close()
		return err
	}

	roomURL := browserURL(*listenFlag, token)
	fmt.Printf("PairRoom %s\n", version.Current)
	fmt.Printf("  room: %s\n", roomURL)
	fmt.Printf("  repo: %s\n", repo)
	fmt.Printf("  data: %s\n", dataDir)
	if *mockFlag {
		fmt.Println("  mode: mock")
	} else {
		fmt.Println("  mode: native Claude Code + Codex")
	}

	serverErrors := make(chan error, 1)
	go func() { serverErrors <- web.Serve(*listenFlag) }()
	if !*noBrowserFlag {
		go func() {
			time.Sleep(180 * time.Millisecond)
			if err := openbrowser.Open(roomURL); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}()
	}

	select {
	case <-rootCtx.Done():
	case err := <-serverErrors:
		if err != nil {
			_ = engine.Close()
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_ = web.Shutdown(shutdownCtx)
	return engine.Close()
}

type doctorCommandReport struct {
	Available bool   `json:"available"`
	Command   string `json:"command"`
	Path      string `json:"path,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
}

type doctorRuntimeReport struct {
	Probe *agent.ProbeResult `json:"probe,omitempty"`
	Error string             `json:"error,omitempty"`
}

type doctorReport struct {
	PairRoom string                         `json:"pairroom"`
	OS       string                         `json:"os"`
	Repo     string                         `json:"repository"`
	Git      doctorCommandReport            `json:"git"`
	Runtimes map[string]doctorRuntimeReport `json:"runtimes"`
	OK       bool                           `json:"ok"`
}

func runDoctor(args []string) error {
	configPath := preparseValue(args, "--config")
	fileCfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("pairroom doctor", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configFlag := flags.String("config", configPath, "JSON configuration file")
	repoFlag := flags.String("repo", ".", "repository/workspace directory")
	claudeCommand := flags.String("claude-command", fileCfg.Claude.Command, "Claude Code executable")
	codexCommand := flags.String("codex-command", fileCfg.Codex.Command, "Codex executable")
	jsonFlag := flags.Bool("json", false, "emit a machine-readable report")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	_ = configFlag
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	repo, err := canonicalDirectory(*repoFlag)
	if err != nil {
		return err
	}

	report := doctorReport{
		PairRoom: version.Current,
		OS:       runtime.GOOS + "/" + runtime.GOARCH,
		Repo:     repo,
		Git:      probeCommand("git", "--version"),
		Runtimes: make(map[string]doctorRuntimeReport, 2),
	}
	for _, cfg := range []agent.Config{
		{Actor: model.ActorClaude, Command: *claudeCommand, Repo: repo, PermissionMode: fileCfg.Claude.PermissionMode},
		{Actor: model.ActorCodex, Command: *codexCommand, Repo: repo, ApprovalPolicy: fileCfg.Codex.ApprovalPolicy, Sandbox: fileCfg.Codex.Sandbox},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		probe, probeErr := agent.ProbeRuntime(ctx, cfg)
		cancel()
		entry := doctorRuntimeReport{}
		if probeErr != nil {
			entry.Error = probeErr.Error()
		} else {
			entry.Probe = &probe
		}
		report.Runtimes[string(cfg.Actor)] = entry
	}
	report.OK = report.Git.Available
	for _, actor := range []string{string(model.ActorClaude), string(model.ActorCodex)} {
		report.OK = report.OK && report.Runtimes[actor].Error == "" && report.Runtimes[actor].Probe != nil
	}

	if *jsonFlag {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
	} else {
		printDoctorReport(report)
	}
	if !report.OK {
		return errors.New("runtime checks failed; pairroom serve --mock remains available")
	}
	return nil
}

func probeCommand(command string, args ...string) doctorCommandReport {
	report := doctorCommandReport{Command: command}
	path, err := exec.LookPath(command)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	report.Path = path
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	report.Output = firstLine(strings.TrimSpace(string(output)))
	if ctx.Err() != nil {
		report.Error = ctx.Err().Error()
		return report
	}
	if err != nil {
		report.Error = err.Error()
		return report
	}
	report.Available = true
	return report
}

func printDoctorReport(report doctorReport) {
	fmt.Printf("PairRoom doctor %s\n", report.PairRoom)
	fmt.Printf("%-16s %s\n", "OS", report.OS)
	fmt.Printf("%-16s %s\n", "Repository", report.Repo)
	if report.Git.Available {
		fmt.Printf("%-16s ✓ %s (%s)\n", "Git", report.Git.Output, report.Git.Path)
	} else {
		fmt.Printf("%-16s ✗ %s\n", "Git", report.Git.Error)
	}
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		entry := report.Runtimes[string(actor)]
		if entry.Error != "" || entry.Probe == nil {
			fmt.Printf("%-16s ✗ %s\n", actor.DisplayName(), entry.Error)
			continue
		}
		probe := entry.Probe
		versionText := probe.Version
		if versionText == "" {
			versionText = probe.VersionLine
		}
		fmt.Printf("%-16s ✓ %s (%s)\n", actor.DisplayName(), versionText, probe.Path)
		fmt.Printf("%-16s   protocol: %s\n", "", probe.Protocol)
		if len(probe.Capabilities) > 0 {
			fmt.Printf("%-16s   capabilities: %s\n", "", strings.Join(probe.Capabilities, ", "))
		}
		for _, warning := range probe.Warnings {
			fmt.Printf("%-16s   warning: %s\n", "", warning)
		}
	}
}

func canonicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve repository: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("stat repository: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repository is not a directory: %s", absolute)
	}
	return filepath.Clean(absolute), nil
}

func defaultDataDir(repo string) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	hash := sha256.Sum256([]byte(repo))
	name := sanitize(filepath.Base(repo))
	return filepath.Join(base, "pairroom", "rooms", name+"-"+hex.EncodeToString(hash[:6])), nil
}

func sanitize(value string) string {
	value = strings.ToLower(value)
	var out strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteByte('-')
		}
	}
	result := strings.Trim(out.String(), "-")
	if result == "" {
		return "room"
	}
	return result
}

func isLoopbackListen(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func randomToken() (string, error) {
	var bytes [24]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate access token: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func browserURL(address, token string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	} else if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
		address = net.JoinHostPort(host, port)
	}
	result := url.URL{Scheme: "http", Host: address, Path: "/"}
	if token != "" {
		query := result.Query()
		query.Set("token", token)
		result.RawQuery = query.Encode()
	}
	return result.String()
}

func preparseValue(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"=")
		}
	}
	return ""
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return value[:index]
	}
	if value == "" {
		return "ok"
	}
	return value
}

func printHelp() {
	fmt.Print(`PairRoom — native Claude Code + Codex collaboration room

Usage:
  pairroom serve [options]   Start the local daemon and IM-style web room
  pairroom doctor [options]  Verify Git and vendor CLI installations
  pairroom version           Print version

Quick start:
  pairroom serve --repo /path/to/project
  pairroom serve --repo . --mock

Run "pairroom serve -help" for runtime and routing options.
`)
}
