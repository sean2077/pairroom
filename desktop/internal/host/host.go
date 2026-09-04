package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/desktop/internal/access"
	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/daemon"
	"github.com/sean2077/pairroom/internal/execx"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/service"
	"github.com/sean2077/pairroom/internal/version"
)

const (
	configPathVariable  = "PAIRROOM_DESKTOP_CONFIG"
	dataRootVariable    = "PAIRROOM_DESKTOP_DATA_ROOT"
	daemonProbeInterval = 100 * time.Millisecond
)

var newDaemonManager = daemon.NewManager

type Mode string

const (
	ModeExternal Mode = "external-daemon"
	ModeEmbedded Mode = "embedded-service"
)

type Options struct {
	ConfigPath               string
	DataRoot                 string
	Mock                     bool
	DisableExternalDiscovery bool
	RuntimeLimit             int
	IdleTimeout              time.Duration
}

type Host struct {
	mode   Mode
	access access.Access

	management *service.ManagementServer
	runtimes   *service.RuntimeManager
	lock       *service.ServiceLock
	cancel     context.CancelFunc
	serveDone  chan error

	closeMu     sync.Mutex
	closed      bool
	closeErr    error
	serveWaited bool
	serveErr    error
}

func Start(ctx context.Context, options Options) (*Host, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// A caller that selected an explicit data root/configuration is asking for
	// an embedded Service for that root. Keep an explicitly supplied Management
	// URL as the strongest override, but never attach to an unrelated default
	// daemon in this case; a root has one owner.
	explicitEmbedded := options.Mock || options.DisableExternalDiscovery ||
		strings.TrimSpace(options.ConfigPath) != "" || strings.TrimSpace(os.Getenv(configPathVariable)) != "" ||
		strings.TrimSpace(options.DataRoot) != "" || strings.TrimSpace(os.Getenv(dataRootVariable)) != ""
	if !options.DisableExternalDiscovery {
		if value, ok, err := access.FromEnvironment(ctx); err != nil {
			return nil, err
		} else if ok {
			return &Host{mode: ModeExternal, access: value}, nil
		}
		if !explicitEmbedded {
			value, installed, err := connectInstalledDaemon(ctx)
			if err == nil && installed {
				return &Host{mode: ModeExternal, access: value}, nil
			}
			if err != nil {
				return nil, err
			}
			if path, ok := lookupBundledCLI(); ok {
				if err := installFromBundledCLI(ctx, path); err != nil {
					return nil, err
				}
				value, installed, err := connectInstalledDaemon(ctx)
				if err != nil {
					return nil, err
				}
				if !installed {
					return nil, errors.New("bundled PairRoom CLI did not install a daemon")
				}
				return &Host{mode: ModeExternal, access: value}, nil
			}
		}
	}
	return startEmbedded(ctx, options)
}

var lookupBundledCLI = bundledCLIPath
var installFromBundledCLI = installDaemonFromCLI

func bundledCLIPath() (string, bool) {
	executable, err := os.Executable()
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	dir := filepath.Dir(executable)
	for _, path := range bundledCLICandidates(dir) {
		if samePath(path, executable) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		return path, true
	}
	return "", false
}

func bundledCLICandidates(dir string) []string {
	if runtime.GOOS == "windows" {
		// NTFS treats PairRoom.exe and pairroom.exe as the same name.
		return []string{
			filepath.Join(dir, "bin", "pairroom.exe"),
			filepath.Join(dir, "cli", "pairroom.exe"),
		}
	}
	return []string{filepath.Join(dir, "pairroom")}
}

func samePath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func daemonWorkDir(cliPath string) string {
	dir := filepath.Dir(cliPath)
	switch strings.ToLower(filepath.Base(dir)) {
	case "bin", "cli":
		return filepath.Dir(dir)
	default:
		return dir
	}
}

func installDaemonFromCLI(ctx context.Context, path string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, path, "daemon", "install", "--binary", path, "--work-dir", daemonWorkDir(path))
	execx.NoConsole(command)
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	text := strings.TrimSpace(string(output))
	if strings.Contains(text, "already installed") {
		return nil
	}
	if text == "" {
		return fmt.Errorf("install bundled PairRoom daemon: %w", err)
	}
	return fmt.Errorf("install bundled PairRoom daemon: %s (%w)", text, err)
}

// connectInstalledDaemon makes the installed daemon the sole owner for the
// default data root. A desktop launch may start a stopped daemon and wait for
// its authenticated endpoint, but it never starts an embedded competitor when
// daemon metadata says an installation exists.
func connectInstalledDaemon(ctx context.Context) (access.Access, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	meta, err := daemon.LoadMeta()
	if errors.Is(err, os.ErrNotExist) {
		// A platform task can outlive or lose its metadata (for example after a
		// manual cleanup). Inspect the manager before deciding that an embedded
		// owner is safe; otherwise the task could race this desktop process.
		manager, managerErr := newDaemonManager()
		if managerErr != nil {
			return access.Access{}, true, fmt.Errorf("inspect installed PairRoom daemon without metadata: %w", managerErr)
		}
		status, statusErr := manager.Status()
		if statusErr != nil {
			return access.Access{}, true, fmt.Errorf("inspect installed PairRoom daemon without metadata: %w", statusErr)
		}
		if status != nil && status.Installed {
			return access.Access{}, true, errors.New("PairRoom daemon service is installed but daemon metadata is missing; run `pairroom daemon install --force` to repair it")
		}
		return access.Access{}, false, nil
	}
	if err != nil {
		return access.Access{}, true, fmt.Errorf("read installed PairRoom daemon metadata: %w", err)
	}
	manager, err := newDaemonManager()
	if err != nil {
		return access.Access{}, true, fmt.Errorf("inspect installed PairRoom daemon: %w", err)
	}
	status, err := manager.Status()
	if err != nil {
		return access.Access{}, true, fmt.Errorf("read installed PairRoom daemon status: %w", err)
	}
	if status == nil || !status.Installed {
		return access.Access{}, true, errors.New("PairRoom daemon metadata exists but its service is not installed; run `pairroom daemon install --force` or remove the stale metadata")
	}
	if err := ctx.Err(); err != nil {
		return access.Access{}, true, err
	}
	root := daemonDataRoot(meta)
	liveOwner := false
	if info, found, err := service.InspectServiceLock(root); err != nil {
		return access.Access{}, true, fmt.Errorf("inspect installed PairRoom service lock: %w", err)
	} else if found && info.PID > 0 {
		running, probeErr := service.ServiceLockOwnerRunning(info)
		if probeErr != nil {
			return access.Access{}, true, fmt.Errorf("verify installed PairRoom service lock owner pid %d: %w", info.PID, probeErr)
		}
		if running {
			// The process may be the daemon in its brief Task Scheduler startup
			// window. Wait for its authenticated endpoint instead of starting a
			// second owner or rejecting a legitimate launch race.
			liveOwner = true
		} else if !status.Running {
			return access.Access{}, true, fmt.Errorf("PairRoom daemon is stopped but data root %s has a crash-stale service.lock (pid %d, started %s); verify the process is gone, then run `pairroom daemon start --recover-stale-lock`", root, info.PID, info.StartedAt.Format(time.RFC3339))
		}
	}
	if value, ok, err := access.DiscoverDaemonForRoot(ctx, root); err != nil {
		return access.Access{}, true, fmt.Errorf("discover installed PairRoom daemon: %w", err)
	} else if ok {
		return value, true, nil
	}

	started := false
	if !status.Running && !liveOwner {
		if err := manager.Start(); err != nil {
			return access.Access{}, true, fmt.Errorf("start installed PairRoom daemon: %w", err)
		}
		started = true
	}
	for {
		value, ok, err := access.DiscoverDaemonForRoot(ctx, root)
		if err != nil {
			return access.Access{}, true, fmt.Errorf("discover installed PairRoom daemon after startup: %w", err)
		}
		if ok {
			return value, true, nil
		}
		if err := ctx.Err(); err != nil {
			return access.Access{}, true, daemonUnavailableError(meta, status, started, err)
		}
		timer := time.NewTimer(daemonProbeInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func daemonUnavailableError(meta *daemon.Meta, status *daemon.Status, started bool, cause error) error {
	state := "running"
	if started {
		state = "starting"
	} else if status == nil || !status.Running {
		state = "stopped"
	}
	root := daemonDataRoot(meta)
	lockDetail := ""
	if info, found, err := service.InspectServiceLock(root); found && err == nil && info.PID > 0 {
		lockDetail = fmt.Sprintf("; service.lock reports pid %d started %s", info.PID, info.StartedAt.Format(time.RFC3339))
	}
	binary := ""
	if meta != nil {
		binary = meta.BinaryPath
	}
	hint := "run `pairroom daemon restart`"
	if started {
		hint = "run `pairroom daemon status`; if service.lock is stale, verify its recorded PID is gone and then run `pairroom daemon start --recover-stale-lock`"
	}
	return fmt.Errorf("installed PairRoom daemon is %s but its authenticated Management Shell did not become available (data root %s, binary %s%s); %s: %w", state, root, binary, lockDetail, hint, cause)
}

func daemonDataRoot(meta *daemon.Meta) string {
	if meta == nil {
		return ""
	}
	root := strings.TrimSpace(meta.DataRoot)
	if resolved, err := service.ResolveRoot(root); err == nil {
		return resolved
	}
	return root
}

func startEmbedded(ctx context.Context, options Options) (_ *Host, resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	configPath := strings.TrimSpace(options.ConfigPath)
	if configPath == "" {
		configPath = strings.TrimSpace(os.Getenv(configPathVariable))
	}
	dataRoot := strings.TrimSpace(options.DataRoot)
	if dataRoot == "" {
		dataRoot = strings.TrimSpace(os.Getenv(dataRootVariable))
	}

	fileConfig, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	lock, err := service.AcquireServiceLock(dataRoot, false)
	if err != nil {
		return nil, err
	}
	cleanupLock := true
	defer func() {
		if cleanupLock {
			resultErr = errors.Join(resultErr, lock.Close())
		}
	}()
	rootCtx, cancel := context.WithCancel(context.Background())
	registry, err := service.OpenRegistry(rootCtx, service.RegistryConfig{Root: lock.Root()})
	if err != nil {
		cancel()
		return nil, err
	}

	claude := agent.Config{
		Actor:                  model.ActorClaude,
		ClientVersion:          version.Current,
		Command:                fileConfig.Claude.Command,
		CommandArgs:            append([]string(nil), fileConfig.Claude.Args...),
		Env:                    copyEnvironment(fileConfig.Claude.RuntimeEnv),
		Runtime:                fileConfig.Claude.RuntimeKind(model.ActorClaude),
		Provider:               fileConfig.Claude.Provider,
		Model:                  fileConfig.Claude.Model,
		Effort:                 fileConfig.Claude.Effort,
		PermissionMode:         fileConfig.Claude.PermissionMode,
		ApprovalPolicy:         fileConfig.Claude.ApprovalPolicy,
		Sandbox:                fileConfig.Claude.Sandbox,
		AdditionalInstructions: strings.TrimSpace(fileConfig.Claude.Instructions),
	}
	codex := agent.Config{
		Actor:                  model.ActorCodex,
		ClientVersion:          version.Current,
		Command:                fileConfig.Codex.Command,
		CommandArgs:            append([]string(nil), fileConfig.Codex.Args...),
		Env:                    copyEnvironment(fileConfig.Codex.RuntimeEnv),
		Runtime:                fileConfig.Codex.RuntimeKind(model.ActorCodex),
		Provider:               fileConfig.Codex.Provider,
		Model:                  fileConfig.Codex.Model,
		Effort:                 fileConfig.Codex.Effort,
		PermissionMode:         fileConfig.Codex.PermissionMode,
		ApprovalPolicy:         fileConfig.Codex.ApprovalPolicy,
		Sandbox:                fileConfig.Codex.Sandbox,
		AdditionalInstructions: strings.TrimSpace(fileConfig.Codex.Instructions),
	}
	claude.PeerRuntime = codex.Runtime
	codex.PeerRuntime = claude.Runtime

	var provisioner service.BindingProvisioner = service.NewNativeProvisioner(service.NativeProvisionerConfig{
		Claude: claude,
		Codex:  codex,
	})
	if options.Mock {
		provisioner = service.SyntheticProvisioner{}
	}

	factory := service.EmbeddedRuntimeFactory(registry, service.EmbeddedRuntimeConfig{
		ListenHost:          "127.0.0.1",
		Mock:                options.Mock,
		AutoStart:           fileConfig.AutoStart,
		RoutingMode:         fileConfig.RoutingMode,
		MaxAgentHops:        fileConfig.MaxAgentHops,
		StallWarningSeconds: fileConfig.StallWarningSeconds,
		Claude:              claude,
		Codex:               codex,
	})
	limit := options.RuntimeLimit
	if limit < 1 {
		limit = service.DefaultRuntimeLimit
	}
	idleTimeout := options.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 15 * time.Minute
	}
	runtimes, err := service.NewRuntimeManager(registry, factory, service.RuntimeManagerConfig{
		Limit:       limit,
		IdleTimeout: idleTimeout,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	cleanupRuntimes := func() error {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := runtimes.Shutdown(shutdownCtx)
		shutdownCancel()
		if err != nil {
			// Keep the lock when the Runtime drain is uncertain. There is no
			// safe way for another Service to share this data root until an
			// operator explicitly verifies the process and recovers the lock.
			cleanupLock = false
		}
		return err
	}
	management, err := service.NewManagementServer(service.ManagementServerConfig{
		Registry:    registry,
		Runtimes:    runtimes,
		Provisioner: provisioner,
		Token:       fileConfig.Token,
	})
	if err != nil {
		cleanupErr := cleanupRuntimes()
		cancel()
		return nil, errors.Join(err, cleanupErr)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cleanupErr := cleanupRuntimes()
		cancel()
		return nil, errors.Join(fmt.Errorf("listen for desktop Management Shell: %w", err), cleanupErr)
	}

	managementAccess, err := access.Parse(management.BrowserURL(listener.Addr()))
	if err != nil {
		_ = listener.Close()
		cleanupErr := cleanupRuntimes()
		cancel()
		return nil, errors.Join(fmt.Errorf("validate desktop Management URL: %w", err), cleanupErr)
	}
	host := &Host{
		mode:       ModeEmbedded,
		access:     managementAccess,
		management: management,
		runtimes:   runtimes,
		lock:       lock,
		cancel:     cancel,
		serveDone:  make(chan error, 1),
	}
	go func() {
		host.serveDone <- management.Serve(listener)
	}()

	probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
	defer probeCancel()
	for !access.Probe(probeCtx, managementAccess) {
		if err := probeCtx.Err(); err != nil {
			// Host now owns the lock. Do not let the deferred partial-start
			// cleanup release it if shutdown cannot prove that all runtimes
			// have drained.
			cleanupLock = false
			shutdownErr := host.Shutdown(context.Background())
			return nil, errors.Join(fmt.Errorf("desktop Management Shell did not become ready: %w", err), shutdownErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cleanupLock = false
	return host, nil
}

func copyEnvironment(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for name, value := range source {
		result[name] = value
	}
	return result
}

func (h *Host) Mode() Mode {
	if h == nil {
		return ""
	}
	return h.mode
}

func (h *Host) URL() string {
	if h == nil {
		return ""
	}
	return h.access.DesktopURL()
}

func (h *Host) Shutdown(ctx context.Context) error {
	if h == nil || h.mode == ModeExternal {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Serialize shutdown attempts, but do not make a timed-out attempt
	// terminal. RuntimeManager deliberately supports retrying a drain with a
	// fresh context; the Host must retain the same property so a short caller
	// deadline cannot strand an embedded Service behind an unreleasable lock.
	h.closeMu.Lock()
	defer h.closeMu.Unlock()
	if h.closed {
		return h.closeErr
	}

	var result error
	if h.management != nil {
		result = errors.Join(result, h.management.Shutdown(ctx))
	}
	if h.runtimes != nil {
		result = errors.Join(result, h.runtimes.Shutdown(ctx))
	}
	if h.cancel != nil {
		h.cancel()
	}
	if h.serveDone != nil && !h.serveWaited {
		select {
		case err := <-h.serveDone:
			h.serveWaited = true
			h.serveErr = err
		case <-ctx.Done():
			result = errors.Join(result, ctx.Err())
		}
	}
	if h.serveWaited {
		result = errors.Join(result, h.serveErr)
	}
	// Keep the lock on any incomplete or uncertain drain. The process may
	// still have a live Runtime, and removing the lock would permit another
	// Service to race it. A subsequent explicit stale-lock recovery is the
	// safe escape hatch after the process is confirmed gone.
	if h.lock != nil && result == nil {
		result = errors.Join(result, h.lock.Close())
	}
	h.closeErr = result
	if result == nil {
		h.closed = true
	}
	return result
}
