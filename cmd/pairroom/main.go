package main

import (
	"context"
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
	"github.com/sean2077/pairroom/internal/archive"
	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/ccswitch"
	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/daemon"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/openbrowser"
	"github.com/sean2077/pairroom/internal/room"
	"github.com/sean2077/pairroom/internal/server"
	"github.com/sean2077/pairroom/internal/service"
	"github.com/sean2077/pairroom/internal/store"
	"github.com/sean2077/pairroom/internal/version"
	"github.com/sean2077/pairroom/internal/workspace"
)

func main() {
	cleanupLogging, err := daemon.ConfigureProcessLoggingFromEnvironment()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pairroom: configure daemon logging:", err)
		os.Exit(1)
	}
	runErr := run(os.Args[1:])
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "pairroom:", runErr)
	}
	loggingErr := cleanupLogging()
	if loggingErr != nil {
		fmt.Fprintln(os.Stderr, "pairroom: close daemon logging:", loggingErr)
	}
	if runErr != nil || loggingErr != nil {
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printHelp()
		return nil
	}
	switch args[0] {
	case "daemon":
		return runDaemon(args[1:])
	case "service":
		return runService(args[1:])
	case "serve":
		return runServe(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "providers":
		return runProviders(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "backup":
		return runBackup(args[1:])
	case "restore":
		return runRestore(args[1:])
	case "diagnostics":
		return runDiagnostics(args[1:])
	case "protocol":
		return runProtocol(args[1:])
	case "version", "--version", "-v":
		return runVersion(args[1:])
	case "help", "--help", "-h":
		printHelp()
		return nil
	default:
		return fmt.Errorf("unknown command %q (use pairroom help)", args[0])
	}
}

func runProviders(args []string) error {
	configPath := preparseValue(args, "--config")
	flags := flag.NewFlagSet("pairroom providers", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configFlag := flags.String("config", configPath, "PairRoom JSON configuration file")
	databaseFlag := flags.String("database", "", "absolute CC Switch database path (default: ~/.cc-switch/cc-switch.db)")
	jsonFlag := flags.Bool("json", false, "emit a redacted machine-readable report")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	cfg, err := config.Load(*configFlag)
	if err != nil {
		return err
	}
	database := cfg.CCSwitch.Database
	if strings.TrimSpace(*databaseFlag) != "" {
		database = *databaseFlag
	}
	reader, err := ccswitch.NewReader(database)
	if err != nil {
		return err
	}
	report, err := reader.Catalog(context.Background())
	if err != nil {
		return err
	}
	if *jsonFlag {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	fmt.Printf("PairRoom CC Switch profiles (%s, schema %d)\n", report.CCSwitchVersion, report.Schema)
	if len(report.Profiles) == 0 {
		fmt.Println("  no profiles found")
	}
	for _, profile := range report.Profiles {
		status := "selectable"
		if !profile.Supported {
			status = "disabled: " + profile.DisabledReason
		}
		fmt.Printf("  %-12s %-24s models=%-24s %s\n", profile.Runtime, profile.Name, strings.Join(profile.Models, ","), status)
	}
	return nil
}

func runVersion(args []string) error {
	flags := flag.NewFlagSet("pairroom version", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	jsonFlag := flags.Bool("json", false, "emit build metadata as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *jsonFlag {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(version.BuildInfo())
	}
	fmt.Println(versionSummary())
	return nil
}

// versionSummary renders the human-readable version line as the most recent
// tag plus commit count and short SHA (e.g. "pairroom v1.1.0+116.21517d1").
func versionSummary() string {
	return "pairroom " + version.Describe()
}

func slotAgentConfig(actor model.ActorID, slot config.Agent, runtimes config.RuntimeTemplates) agent.Config {
	selection := slot.Selection(actor)
	template := runtimes.For(selection.Runtime)
	return agent.Config{
		Actor:                  actor,
		ClientVersion:          version.Current,
		Command:                template.Command,
		CommandArgs:            append([]string(nil), template.Args...),
		Runtime:                selection.Runtime,
		Provider:               string(selection.Provider.Source),
		Model:                  selection.Model,
		Effort:                 selection.Effort,
		PermissionMode:         selection.PermissionMode,
		ApprovalPolicy:         selection.ApprovalPolicy,
		Sandbox:                selection.Sandbox,
		AdditionalInstructions: selection.Instructions,
		OrdinaryReviewerPolicy: selection.OrdinaryReviewerPolicy,
	}
}

func pairSlotConfigs(fileCfg config.File) (agent.Config, agent.Config) {
	claude := slotAgentConfig(model.ActorClaude, fileCfg.Claude, fileCfg.Runtimes)
	codex := slotAgentConfig(model.ActorCodex, fileCfg.Codex, fileCfg.Runtimes)
	claude.PeerRuntime = codex.Runtime
	codex.PeerRuntime = claude.Runtime
	return claude, codex
}

func applySlotCLI(cfg *config.Agent, runtime, modelName, effort, permission, approval, sandbox, instructions string) {
	if runtime != "" {
		cfg.Runtime = string(model.ParseRuntimeKind(runtime))
	}
	cfg.Model = modelName
	cfg.Effort = effort
	cfg.PermissionMode = permission
	cfg.ApprovalPolicy = approval
	cfg.Sandbox = sandbox
	cfg.Instructions = strings.TrimSpace(instructions)
	switch model.ParseRuntimeKind(cfg.Runtime).Canonical() {
	case model.RuntimeClaude:
		cfg.ApprovalPolicy, cfg.Sandbox = "", ""
	case model.RuntimeCodex:
		cfg.PermissionMode = ""
	case model.RuntimeGrok:
		cfg.ApprovalPolicy = ""
	}
}

func configuredAgentResolver(fileCfg config.File, mock bool) (*service.AgentResolver, error) {
	reader, err := ccswitch.NewReader(fileCfg.CCSwitch.Database)
	if err != nil {
		return nil, err
	}
	return service.NewAgentResolver(service.AgentResolverConfig{Defaults: fileCfg.DefaultSelections(), Runtimes: fileCfg.Runtimes, CCSwitch: reader, Mock: mock})
}

// runService starts the process-wide Management Shell. Room runtimes are
// activated lazily and remain isolated behind their own loopback listeners.
// The legacy single-Room `serve` command is deliberately preserved below.
func runService(args []string) (resultErr error) {
	configPath := preparseValue(args, "--config")
	fileCfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("pairroom service", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configFlag := flags.String("config", configPath, "JSON configuration file")
	listenFlag := flags.String("listen", fileCfg.Listen, "Management Shell listen address (numeric loopback only)")
	rootFlag := flags.String("data-root", "", "absolute service data root (default: OS user config directory/pairroom)")
	tokenFlag := flags.String("token", fileCfg.Token, "Management API bearer token (generated when omitted)")
	limitFlag := flags.Int("runtime-limit", service.DefaultRuntimeLimit, "maximum simultaneously active Room runtimes")
	idleFlag := flags.Duration("idle-timeout", 15*time.Minute, "suspend an idle Room runtime after this duration")
	shutdownFlag := flags.Duration("shutdown-timeout", 10*time.Minute, "maximum graceful-shutdown wait for active Turns")
	recoverLockFlag := flags.Bool("recover-stale-lock", false, "explicitly replace a crash-stale service.lock after verifying the recorded owner PID is gone")
	daemonControlFlag := flags.String("daemon-control-file", "", "internal daemon graceful-shutdown control file")
	mockFlag := flags.Bool("mock", false, "run deterministic mock agents instead of vendor CLIs")
	noBrowserFlag := flags.Bool("no-browser", false, "do not open the Management Shell in a browser")
	autoStartFlag := flags.Bool("auto-start", fileCfg.AutoStart, "start both agents when a Room runtime activates")
	stallWarningFlag := flags.Int("stall-warning-seconds", fileCfg.StallWarningSeconds, "warn when a working agent emits no runtime event; -1 disables")
	claudeRuntime := flags.String("claude-runtime", fileCfg.Claude.Runtime, "Agent 1 runtime: claude, codex, or grok")
	claudeCommand := flags.String("claude-command", fileCfg.Runtimes.Claude.Command, "Claude Code executable template")
	claudeModel := flags.String("claude-model", fileCfg.Claude.Model, "Agent 1 model override")
	claudeEffort := flags.String("claude-effort", fileCfg.Claude.Effort, "Agent 1 reasoning-effort override")
	claudePermission := flags.String("claude-permission-mode", fileCfg.Claude.PermissionMode, "Agent 1 permission mode")
	claudeInstructions := flags.String("claude-instructions", fileCfg.Claude.Instructions, "Agent 1 additional instructions")
	codexRuntime := flags.String("codex-runtime", fileCfg.Codex.Runtime, "Agent 2 runtime: claude, codex, or grok")
	codexCommand := flags.String("codex-command", fileCfg.Runtimes.Codex.Command, "Codex executable template")
	grokCommand := flags.String("grok-command", fileCfg.Runtimes.Grok.Command, "Grok Build executable template")
	codexModel := flags.String("codex-model", fileCfg.Codex.Model, "Agent 2 model override")
	codexEffort := flags.String("codex-effort", fileCfg.Codex.Effort, "Agent 2 reasoning-effort override")
	codexApproval := flags.String("codex-approval-policy", fileCfg.Codex.ApprovalPolicy, "Agent 2 approval policy")
	codexSandbox := flags.String("codex-sandbox", fileCfg.Codex.Sandbox, "Agent 2 sandbox mode")
	codexInstructions := flags.String("codex-instructions", fileCfg.Codex.Instructions, "Agent 2 additional instructions")
	ccSwitchDatabase := flags.String("cc-switch-db", fileCfg.CCSwitch.Database, "absolute CC Switch database path")
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
	if !isLoopbackListen(*listenFlag) {
		return errors.New("pairroom service must listen on a numeric loopback address; use an SSH tunnel for remote access")
	}
	if *limitFlag < service.MinRuntimeLimit || *limitFlag > service.MaxRuntimeLimit {
		return fmt.Errorf("runtime-limit must be between %d and %d", service.MinRuntimeLimit, service.MaxRuntimeLimit)
	}
	if *idleFlag <= 0 {
		return errors.New("idle-timeout must be greater than zero")
	}
	if *shutdownFlag <= 0 {
		return errors.New("shutdown-timeout must be greater than zero")
	}
	if *daemonControlFlag != "" && !filepath.IsAbs(*daemonControlFlag) {
		return errors.New("daemon-control-file must be absolute")
	}
	if *stallWarningFlag != -1 && (*stallWarningFlag < 30 || *stallWarningFlag > 86400) {
		return errors.New("stall-warning-seconds must be -1 or between 30 and 86400")
	}

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	rootCtx, stopRoot := context.WithCancel(signalCtx)
	defer stopRoot()
	if *daemonControlFlag != "" {
		controlPath := filepath.Clean(*daemonControlFlag)
		if err := os.Remove(controlPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear daemon control file: %w", err)
		}
		go watchDaemonControlFile(rootCtx, controlPath, stopRoot)
	}
	serviceLock, err := service.AcquireServiceLock(*rootFlag, *recoverLockFlag)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, serviceLock.Close())
	}()
	registry, err := service.OpenRegistry(rootCtx, service.RegistryConfig{Root: serviceLock.Root()})
	if err != nil {
		return err
	}
	fileCfg.Runtimes.Claude.Command = *claudeCommand
	fileCfg.Runtimes.Codex.Command = *codexCommand
	fileCfg.Runtimes.Grok.Command = *grokCommand
	fileCfg.CCSwitch.Database = *ccSwitchDatabase
	applySlotCLI(&fileCfg.Claude, *claudeRuntime, *claudeModel, *claudeEffort, *claudePermission, fileCfg.Claude.ApprovalPolicy, fileCfg.Claude.Sandbox, *claudeInstructions)
	applySlotCLI(&fileCfg.Codex, *codexRuntime, *codexModel, *codexEffort, fileCfg.Codex.PermissionMode, *codexApproval, *codexSandbox, *codexInstructions)
	if err := fileCfg.Validate(); err != nil {
		return err
	}
	agentResolver, err := configuredAgentResolver(fileCfg, *mockFlag)
	if err != nil {
		return err
	}
	var provisioner service.BindingProvisioner = service.NewNativeProvisioner(service.NativeProvisionerConfig{
		Resolver: agentResolver,
	})
	if *mockFlag {
		provisioner = service.SyntheticProvisioner{}
	}
	factory := service.EmbeddedRuntimeFactory(registry, service.EmbeddedRuntimeConfig{
		ListenHost: "127.0.0.1", Mock: *mockFlag, AutoStart: *autoStartFlag,
		StallWarningSeconds: *stallWarningFlag,
		Resolver:            agentResolver,
	})
	runtimes, err := service.NewRuntimeManager(registry, factory, service.RuntimeManagerConfig{
		Limit: *limitFlag, IdleTimeout: *idleFlag,
	})
	if err != nil {
		return err
	}
	management, err := service.NewManagementServer(service.ManagementServerConfig{
		Registry: registry, Runtimes: runtimes, Provisioner: provisioner, Token: *tokenFlag, AgentResolver: agentResolver,
	})
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runtimes.Shutdown(shutdownCtx)
		return err
	}
	listener, err := net.Listen("tcp", *listenFlag)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runtimes.Shutdown(shutdownCtx)
		return fmt.Errorf("listen for Management Shell: %w", err)
	}
	managementURL := management.BrowserURL(listener.Addr())
	fmt.Printf("PairRoom Service %s\n", version.Describe())
	fmt.Printf("  management: %s\n", managementURL)
	fmt.Printf("  data root:  %s\n", registry.Root())
	fmt.Printf("  runtimes:   %d active maximum, idle timeout %s\n", *limitFlag, idleFlag.String())
	if *mockFlag {
		fmt.Println("  mode:       mock")
	} else {
		fmt.Println("  mode:       native Claude Code / Codex / Grok Build slots")
	}

	serverErrors := make(chan error, 1)
	go func() { serverErrors <- management.Serve(listener) }()
	if !*noBrowserFlag {
		go func() {
			time.Sleep(180 * time.Millisecond)
			if err := openbrowser.Open(managementURL); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}()
	}

	var serveErr error
	select {
	case <-rootCtx.Done():
	case serveErr = <-serverErrors:
	}
	// Shutdown is deliberately ordered: stop accepting management requests and
	// wait for in-flight provisioning/lifecycle handlers first, then drain Room
	// runtimes without interrupting active native Turns.
	managementCtx, cancelManagement := context.WithTimeout(context.Background(), *shutdownFlag)
	managementErr := management.Shutdown(managementCtx)
	cancelManagement()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), *shutdownFlag)
	shutdownErr := runtimes.Shutdown(shutdownCtx)
	cancelShutdown()
	return errors.Join(serveErr, managementErr, shutdownErr)
}

func watchDaemonControlFile(ctx context.Context, path string, stop context.CancelFunc) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(path); err == nil {
				stop()
				return
			}
		}
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
	listenFlag := flags.String("listen", fileCfg.Listen, "HTTP listen address (numeric loopback only)")
	dataFlag := flags.String("data-dir", "", "room state directory (default: per-repository user config directory)")
	tokenFlag := flags.String("token", fileCfg.Token, "optional API bearer token for loopback defense in depth")
	mockFlag := flags.Bool("mock", false, "run deterministic mock agents instead of vendor CLIs")
	noBrowserFlag := flags.Bool("no-browser", false, "do not open the room in a browser")
	autoStartFlag := flags.Bool("auto-start", fileCfg.AutoStart, "start both agents when the room opens")
	stallWarningFlag := flags.Int("stall-warning-seconds", fileCfg.StallWarningSeconds, "warn when a working agent emits no runtime event; -1 disables")
	claudeRuntime := flags.String("claude-runtime", fileCfg.Claude.Runtime, "Agent 1 runtime: claude, codex, or grok")
	claudeCommand := flags.String("claude-command", fileCfg.Runtimes.Claude.Command, "Claude Code executable template")
	claudeModel := flags.String("claude-model", fileCfg.Claude.Model, "Agent 1 model override")
	claudeEffort := flags.String("claude-effort", fileCfg.Claude.Effort, "Agent 1 reasoning-effort override")
	claudePermission := flags.String("claude-permission-mode", fileCfg.Claude.PermissionMode, "Agent 1 permission mode")
	claudeInstructions := flags.String("claude-instructions", fileCfg.Claude.Instructions, "Agent 1 additional instructions")
	codexRuntime := flags.String("codex-runtime", fileCfg.Codex.Runtime, "Agent 2 runtime: claude, codex, or grok")
	codexCommand := flags.String("codex-command", fileCfg.Runtimes.Codex.Command, "Codex executable template")
	grokCommand := flags.String("grok-command", fileCfg.Runtimes.Grok.Command, "Grok Build executable template")
	codexModel := flags.String("codex-model", fileCfg.Codex.Model, "Agent 2 model override")
	codexEffort := flags.String("codex-effort", fileCfg.Codex.Effort, "Agent 2 reasoning-effort override")
	codexApproval := flags.String("codex-approval-policy", fileCfg.Codex.ApprovalPolicy, "Agent 2 approval policy")
	codexSandbox := flags.String("codex-sandbox", fileCfg.Codex.Sandbox, "Agent 2 sandbox mode")
	codexInstructions := flags.String("codex-instructions", fileCfg.Codex.Instructions, "Agent 2 additional instructions")
	ccSwitchDatabase := flags.String("cc-switch-db", fileCfg.CCSwitch.Database, "absolute CC Switch database path")
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
	if !isLoopbackListen(*listenFlag) {
		return errors.New("pairroom serve must listen on a numeric loopback address; use an SSH tunnel for remote access")
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
	if *stallWarningFlag != -1 && (*stallWarningFlag < 30 || *stallWarningFlag > 86400) {
		return errors.New("stall-warning-seconds must be -1 or between 30 and 86400")
	}

	token := *tokenFlag

	eventStore, err := store.Open(dataDir)
	if err != nil {
		return err
	}
	attachmentStore, err := attachment.Open(dataDir, repo)
	if err != nil {
		_ = eventStore.Close()
		return err
	}
	workspaceManager, err := workspace.New(repo, dataDir)
	if err != nil {
		_ = eventStore.Close()
		return err
	}
	fileCfg.Runtimes.Claude.Command = *claudeCommand
	fileCfg.Runtimes.Codex.Command = *codexCommand
	fileCfg.Runtimes.Grok.Command = *grokCommand
	fileCfg.CCSwitch.Database = *ccSwitchDatabase
	applySlotCLI(&fileCfg.Claude, *claudeRuntime, *claudeModel, *claudeEffort, *claudePermission, fileCfg.Claude.ApprovalPolicy, fileCfg.Claude.Sandbox, *claudeInstructions)
	applySlotCLI(&fileCfg.Codex, *codexRuntime, *codexModel, *codexEffort, fileCfg.Codex.PermissionMode, *codexApproval, *codexSandbox, *codexInstructions)
	if err := fileCfg.Validate(); err != nil {
		_ = eventStore.Close()
		return err
	}
	agentResolver, err := configuredAgentResolver(fileCfg, *mockFlag)
	if err != nil {
		_ = eventStore.Close()
		return err
	}
	defaults := agentResolver.DefaultSelections()
	claudeCfg, err := agentResolver.Resolve(context.Background(), model.ActorClaude, defaults[model.ActorClaude], defaults[model.ActorCodex].Runtime, repo, dataDir)
	if err != nil {
		_ = eventStore.Close()
		return fmt.Errorf("resolve Agent 1: %w", err)
	}
	codexCfg, err := agentResolver.Resolve(context.Background(), model.ActorCodex, defaults[model.ActorCodex], defaults[model.ActorClaude].Runtime, repo, dataDir)
	if err != nil {
		_ = eventStore.Close()
		return fmt.Errorf("resolve Agent 2: %w", err)
	}
	engine, err := room.New(room.Config{
		Name: *nameFlag,
		Repo: repo,
		Settings: model.RoomSettings{
			StallWarningSeconds: *stallWarningFlag,
		},
		Store:         eventStore,
		ClaudeFactory: agent.SlotFactory(*mockFlag, claudeCfg.Runtime),
		CodexFactory:  agent.SlotFactory(*mockFlag, codexCfg.Runtime),
		ClaudeConfig:  claudeCfg,
		CodexConfig:   codexCfg,
		Attachments:   attachmentStore,
		Workspaces:    workspaceManager,
		AutoStart:     *autoStartFlag,
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
	fmt.Printf("PairRoom %s\n", version.Describe())
	fmt.Printf("  room: %s\n", roomURL)
	fmt.Printf("  repo: %s\n", repo)
	fmt.Printf("  data: %s\n", dataDir)
	if *mockFlag {
		fmt.Println("  mode: mock")
	} else {
		fmt.Println("  mode: native Claude Code / Codex / Grok Build slots")
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
	claudeCommand := flags.String("claude-command", fileCfg.Runtimes.Claude.Command, "Claude Code executable")
	codexCommand := flags.String("codex-command", fileCfg.Runtimes.Codex.Command, "Codex executable")
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
		PairRoom: version.Describe(),
		OS:       runtime.GOOS + "/" + runtime.GOARCH,
		Repo:     repo,
		Git:      probeCommand("git", "--version"),
		Runtimes: make(map[string]doctorRuntimeReport, 2),
	}
	fileCfg.Runtimes.Claude.Command = *claudeCommand
	fileCfg.Runtimes.Codex.Command = *codexCommand
	claudeCfg, codexCfg := pairSlotConfigs(fileCfg)
	claudeCfg.Command = *claudeCommand
	codexCfg.Command = *codexCommand
	claudeCfg.Repo = repo
	codexCfg.Repo = repo
	for _, cfg := range []agent.Config{claudeCfg, codexCfg} {
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

func runVerify(args []string) error {
	flags := flag.NewFlagSet("pairroom verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repoFlag := flags.String("repo", ".", "repository used to resolve the default room data directory")
	dataFlag := flags.String("data-dir", "", "room state directory")
	jsonFlag := flags.Bool("json", false, "emit a machine-readable report")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	dataDir, err := resolveDataDir(*repoFlag, *dataFlag)
	if err != nil {
		return err
	}
	report := archive.Verify(dataDir)
	if *jsonFlag {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
	} else {
		fmt.Printf("PairRoom data verification\n")
		fmt.Printf("  data:        %s\n", report.DataDir)
		fmt.Printf("  schema:      %d\n", report.SchemaVersion)
		fmt.Printf("  events:      %d (%d..%d)\n", report.EventCount, report.FirstSequence, report.LastSequence)
		fmt.Printf("  attachments: %d (%d referenced)\n", report.AttachmentCount, report.ReferencedAttachments)
		for _, warning := range report.Warnings {
			fmt.Printf("  warning:     %s\n", warning)
		}
		for _, value := range report.Errors {
			fmt.Printf("  error:       %s\n", value)
		}
		if report.OK {
			fmt.Println("  result:      OK")
		} else {
			fmt.Println("  result:      FAILED")
		}
	}
	if !report.OK {
		return errors.New("room data verification failed")
	}
	return nil
}

func runBackup(args []string) error {
	flags := flag.NewFlagSet("pairroom backup", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repoFlag := flags.String("repo", ".", "repository used to resolve the default room data directory")
	dataFlag := flags.String("data-dir", "", "room state directory")
	outputFlag := flags.String("output", "", "destination .tar.gz path")
	jsonFlag := flags.Bool("json", false, "emit the backup manifest as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*outputFlag) == "" {
		return errors.New("--output is required")
	}
	dataDir, err := resolveDataDir(*repoFlag, *dataFlag)
	if err != nil {
		return err
	}
	manifest, err := archive.Backup(dataDir, *outputFlag)
	if err != nil {
		return err
	}
	if *jsonFlag {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(manifest)
	}
	output, _ := filepath.Abs(*outputFlag)
	fmt.Printf("PairRoom backup created\n  output: %s\n  files:  %d\n", output, len(manifest.Files))
	return nil
}

func runRestore(args []string) error {
	flags := flag.NewFlagSet("pairroom restore", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repoFlag := flags.String("repo", ".", "repository used to resolve the default room data directory")
	dataFlag := flags.String("data-dir", "", "destination room state directory")
	inputFlag := flags.String("input", "", "source PairRoom backup .tar.gz")
	forceFlag := flags.Bool("force", false, "replace a non-empty destination after full archive validation")
	jsonFlag := flags.Bool("json", false, "emit the restored verification report as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*inputFlag) == "" {
		return errors.New("--input is required")
	}
	dataDir, err := resolveDataDir(*repoFlag, *dataFlag)
	if err != nil {
		return err
	}
	report, err := archive.Restore(*inputFlag, dataDir, *forceFlag)
	if err != nil {
		return err
	}
	if *jsonFlag {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	fmt.Printf("PairRoom backup restored\n  data:   %s\n  events: %d\n", report.DataDir, report.EventCount)
	return nil
}

func runDiagnostics(args []string) error {
	flags := flag.NewFlagSet("pairroom diagnostics", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repoFlag := flags.String("repo", ".", "repository used to resolve the default room data directory")
	dataFlag := flags.String("data-dir", "", "room state directory")
	outputFlag := flags.String("output", "", "destination diagnostics .tar.gz path")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*outputFlag) == "" {
		return errors.New("--output is required")
	}
	dataDir, err := resolveDataDir(*repoFlag, *dataFlag)
	if err != nil {
		return err
	}
	if err := archive.Diagnostics(dataDir, *outputFlag, runtime.GOOS, runtime.GOARCH); err != nil {
		return err
	}
	output, _ := filepath.Abs(*outputFlag)
	fmt.Printf("PairRoom diagnostics created\n  output: %s\n", output)
	return nil
}

func resolveDataDir(repoValue, dataValue string) (string, error) {
	if strings.TrimSpace(dataValue) != "" {
		absolute, err := filepath.Abs(dataValue)
		if err != nil {
			return "", fmt.Errorf("resolve data directory: %w", err)
		}
		return filepath.Clean(absolute), nil
	}
	repo, err := canonicalDirectory(repoValue)
	if err != nil {
		return "", err
	}
	return defaultDataDir(repo)
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
	runtimes := make(map[model.ActorID]model.RuntimeKind, 2)
	for _, actor := range model.SlotActors() {
		if probe := report.Runtimes[string(actor)].Probe; probe != nil {
			runtimes[actor] = probe.Runtime
		}
	}
	identities := model.ParticipantIdentities(runtimes)
	for _, actor := range model.SlotActors() {
		entry := report.Runtimes[string(actor)]
		label := model.SlotLabel(actor)
		if entry.Error != "" || entry.Probe == nil {
			fmt.Printf("%-16s ✗ %s\n", label, entry.Error)
			continue
		}
		probe := entry.Probe
		if probe.Runtime != "" {
			label = identities[actor].DisplayName
		}
		versionText := probe.Version
		if versionText == "" {
			versionText = probe.VersionLine
		}
		fmt.Printf("%-16s ✓ %s (%s)\n", label, versionText, probe.Path)
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
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
		fragment := url.Values{}
		fragment.Set("token", token)
		result.Fragment = fragment.Encode()
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
	fmt.Print(`PairRoom — native Claude Code, Codex, and Grok Build collaboration service

Usage:
  pairroom daemon <command>      Install and manage pairroom service in the OS service manager
  pairroom service [options]     Start the multi-Project, multi-Room Management Shell
  pairroom serve [options]       Start the legacy single-Room daemon and Room View
  pairroom doctor [options]      Verify Git and vendor CLI installations
  pairroom providers [options]   Inspect the read-only sanitized CC Switch Profile catalog
  pairroom verify [options]      Strictly verify room data integrity
  pairroom backup [options]      Create a verified room-data backup
  pairroom restore [options]     Restore and verify a room-data backup
  pairroom diagnostics [options] Create a redacted diagnostics bundle
  pairroom protocol [options]    Print the versioned agent collaboration contract
  pairroom version               Print version

Quick start:
  pairroom service
  pairroom daemon install
  pairroom service --mock
  pairroom serve --repo /path/to/project

Run "pairroom service -help" for service-capacity and runtime options.
Run "pairroom daemon -help" for background service management.
Run "pairroom serve -help" for legacy single-Room options.
`)
}
