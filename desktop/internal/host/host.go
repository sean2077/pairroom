package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/desktop/internal/access"
	"github.com/sean2077/pairroom/internal/ccswitch"
	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/daemon"
	"github.com/sean2077/pairroom/internal/service"
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
	mode     Mode
	access   access.Access
	dataRoot string

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
			value, root, installed, err := connectInstalledDaemon(ctx)
			if err == nil && installed {
				return &Host{mode: ModeExternal, access: value, dataRoot: root}, nil
			}
			if err != nil {
				return nil, err
			}
		}
	}
	// Launching the UI is not consent to install a persistent system service.
	// With no installed daemon, this process owns and drains the Service.
	return startEmbedded(ctx, options)
}

// connectInstalledDaemon makes the installed daemon the sole owner for the
// default data root. A desktop launch may recover a crash-stale lock, start or
// restart a stopped or zombie daemon, and wait for its authenticated endpoint.
// It never starts an embedded competitor when daemon metadata says an
// installation exists.
func connectInstalledDaemon(ctx context.Context) (access.Access, string, bool, error) {
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
			return access.Access{}, "", true, fmt.Errorf("inspect installed PairRoom daemon without metadata: %w", managerErr)
		}
		status, statusErr := manager.Status()
		if statusErr != nil {
			return access.Access{}, "", true, fmt.Errorf("inspect installed PairRoom daemon without metadata: %w", statusErr)
		}
		if status != nil && status.Installed {
			return access.Access{}, "", true, errors.New("PairRoom daemon service is installed but daemon metadata is missing; run `pairroom daemon install --force` to repair it")
		}
		return access.Access{}, "", false, nil
	}
	if err != nil {
		return access.Access{}, "", true, fmt.Errorf("read installed PairRoom daemon metadata: %w", err)
	}
	manager, err := newDaemonManager()
	if err != nil {
		return access.Access{}, "", true, fmt.Errorf("inspect installed PairRoom daemon: %w", err)
	}
	status, err := manager.Status()
	if err != nil {
		return access.Access{}, "", true, fmt.Errorf("read installed PairRoom daemon status: %w", err)
	}
	if status == nil || !status.Installed {
		return access.Access{}, "", true, errors.New("PairRoom daemon metadata exists but its service is not installed; run `pairroom daemon install --force` or remove the stale metadata")
	}
	if err := ctx.Err(); err != nil {
		return access.Access{}, "", true, err
	}
	root := daemonDataRoot(meta)
	recoveredStale, liveOwner, err := recoverInstalledDaemonLock(root)
	if err != nil {
		return access.Access{}, "", true, err
	}
	if value, ok, err := access.DiscoverDaemonForRoot(ctx, root); err != nil {
		return access.Access{}, "", true, fmt.Errorf("discover installed PairRoom daemon: %w", err)
	} else if ok {
		return value, root, true, nil
	}

	started := false
	// A live lock owner may be the daemon in its brief Task Scheduler startup
	// window. Wait for its authenticated endpoint instead of starting a second
	// owner or rejecting a legitimate launch race.
	if !liveOwner && status.Running && recoveredStale {
		if err := manager.Restart(); err != nil {
			return access.Access{}, "", true, fmt.Errorf("restart installed PairRoom daemon after crash-stale lock recovery: %w", err)
		}
		started = true
	} else if !liveOwner && !status.Running {
		if err := manager.Start(); err != nil {
			return access.Access{}, "", true, fmt.Errorf("start installed PairRoom daemon: %w", err)
		}
		started = true
	}
	for {
		value, ok, err := access.DiscoverDaemonForRoot(ctx, root)
		if err != nil {
			return access.Access{}, "", true, fmt.Errorf("discover installed PairRoom daemon after startup: %w", err)
		}
		if ok {
			return value, root, true, nil
		}
		if err := ctx.Err(); err != nil {
			return access.Access{}, "", true, daemonUnavailableError(meta, status, started, err)
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
	hint := "run `pairroom daemon restart`"
	if info, found, err := service.InspectServiceLock(root); found && err == nil && info.PID > 0 {
		lockDetail = fmt.Sprintf("; service.lock reports pid %d started %s", info.PID, info.StartedAt.Format(time.RFC3339))
		if running, probeErr := service.ServiceLockOwnerRunning(info); probeErr == nil && running {
			hint = "run `pairroom daemon stop` and wait for graceful drain if that process is the installed daemon"
		}
	}
	binary := ""
	if meta != nil {
		binary = meta.BinaryPath
	}
	return fmt.Errorf("installed PairRoom daemon is %s but its authenticated Management Shell did not become available (data root %s, binary %s%s); %s: %w", state, root, binary, lockDetail, hint, cause)
}

// recoverInstalledDaemonLock removes a crash-stale lock after the recorded PID
// is confirmed gone so the installed daemon can become the sole owner. A live
// owner is left untouched.
func recoverInstalledDaemonLock(root string) (recovered bool, liveOwner bool, err error) {
	info, found, err := service.InspectServiceLock(root)
	if err != nil {
		return false, false, fmt.Errorf("inspect installed PairRoom service lock: %w", err)
	}
	if !found || info.PID <= 0 {
		return false, false, nil
	}
	running, probeErr := service.ServiceLockOwnerRunning(info)
	if probeErr != nil {
		return false, false, fmt.Errorf("verify installed PairRoom service lock owner pid %d: %w", info.PID, probeErr)
	}
	if running {
		return false, true, nil
	}
	if err := service.RecoverServiceLock(root); err != nil {
		return false, false, fmt.Errorf("recover crash-stale PairRoom service.lock (pid %d, started %s): %w", info.PID, info.StartedAt.Format(time.RFC3339), err)
	}
	return true, false, nil
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
	lock, err := service.AcquireServiceLock(dataRoot, true)
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

	ccSwitchReader, err := ccswitch.NewReader(fileConfig.CCSwitch.Database)
	if err != nil {
		cancel()
		return nil, err
	}
	agentResolver, err := service.NewAgentResolver(service.AgentResolverConfig{
		Defaults: fileConfig.DefaultSelections(), Runtimes: fileConfig.Runtimes,
		CCSwitch: ccSwitchReader, Mock: options.Mock,
	})
	if err != nil {
		cancel()
		return nil, err
	}

	var provisioner service.BindingProvisioner = service.NewNativeProvisioner(service.NativeProvisionerConfig{
		Resolver: agentResolver,
	})
	if options.Mock {
		provisioner = service.SyntheticProvisioner{}
	}

	factory := service.EmbeddedRuntimeFactory(registry, service.EmbeddedRuntimeConfig{
		ListenHost:          "127.0.0.1",
		Mock:                options.Mock,
		AutoStart:           fileConfig.AutoStart,
		StallWarningSeconds: fileConfig.StallWarningSeconds,
		Resolver:            agentResolver,
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
			// safe way for another Service to share this data root until the
			// recorded owner PID is gone and crash-stale recovery can run.
			cleanupLock = false
		}
		return err
	}
	management, err := service.NewManagementServer(service.ManagementServerConfig{
		Registry:      registry,
		Runtimes:      runtimes,
		Provisioner:   provisioner,
		Token:         fileConfig.Token,
		AgentResolver: agentResolver,
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
		dataRoot:   lock.Root(),
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

// BrowserURL is the authenticated Management URL without the desktop-window
// marker, suitable for handing to an external browser.
func (h *Host) BrowserURL() string {
	if h == nil {
		return ""
	}
	return h.access.BrowserURL
}

// DataRoot is the Service data root this Host owns or observes. An explicit
// endpoint override carries no root ownership, so it reports "".
func (h *Host) DataRoot() string {
	if h == nil {
		return ""
	}
	return h.dataRoot
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
	// Service to race it. The next Desktop or daemon start recovers the lock
	// only after this process is confirmed gone.
	if h.lock != nil && result == nil {
		result = errors.Join(result, h.lock.Close())
	}
	h.closeErr = result
	if result == nil {
		h.closed = true
	}
	return result
}
