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
	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/service"
	"github.com/sean2077/pairroom/internal/version"
)

const (
	configPathVariable = "PAIRROOM_DESKTOP_CONFIG"
	dataRootVariable   = "PAIRROOM_DESKTOP_DATA_ROOT"
)

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

	closeOnce sync.Once
	closeErr  error
}

func Start(ctx context.Context, options Options) (*Host, error) {
	if !options.DisableExternalDiscovery {
		if value, ok, err := access.FromEnvironment(ctx); err != nil {
			return nil, err
		} else if ok {
			return &Host{mode: ModeExternal, access: value}, nil
		}
		value, ok, err := access.DiscoverDaemon(ctx)
		if err == nil && ok {
			return &Host{mode: ModeExternal, access: value}, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("discover installed PairRoom daemon: %w", err)
		}
	}
	return startEmbedded(ctx, options)
}

func startEmbedded(ctx context.Context, options Options) (_ *Host, resultErr error) {
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
		ClientVersion:  version.Current,
		Command:        fileConfig.Claude.Command,
		CommandArgs:    append([]string(nil), fileConfig.Claude.Args...),
		Env:            copyEnvironment(fileConfig.Claude.RuntimeEnv),
		Provider:       fileConfig.Claude.Provider,
		Model:          fileConfig.Claude.Model,
		PermissionMode: fileConfig.Claude.PermissionMode,
	}
	codex := agent.Config{
		ClientVersion:  version.Current,
		Command:        fileConfig.Codex.Command,
		CommandArgs:    append([]string(nil), fileConfig.Codex.Args...),
		Env:            copyEnvironment(fileConfig.Codex.RuntimeEnv),
		Provider:       fileConfig.Codex.Provider,
		Model:          fileConfig.Codex.Model,
		Effort:         fileConfig.Codex.Effort,
		ApprovalPolicy: fileConfig.Codex.ApprovalPolicy,
		Sandbox:        fileConfig.Codex.Sandbox,
	}

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
		limit = 2
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
	management, err := service.NewManagementServer(service.ManagementServerConfig{
		Registry:    registry,
		Runtimes:    runtimes,
		Provisioner: provisioner,
		Token:       fileConfig.Token,
	})
	if err != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = runtimes.Shutdown(shutdownCtx)
		shutdownCancel()
		cancel()
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = runtimes.Shutdown(shutdownCtx)
		shutdownCancel()
		cancel()
		return nil, fmt.Errorf("listen for desktop Management Shell: %w", err)
	}

	managementAccess, err := access.Parse(management.BrowserURL(listener.Addr()))
	if err != nil {
		_ = listener.Close()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = runtimes.Shutdown(shutdownCtx)
		shutdownCancel()
		cancel()
		return nil, fmt.Errorf("validate desktop Management URL: %w", err)
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
			_ = host.Shutdown(context.Background())
			return nil, fmt.Errorf("desktop Management Shell did not become ready: %w", err)
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
	h.closeOnce.Do(func() {
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
		if h.serveDone != nil {
			select {
			case err := <-h.serveDone:
				result = errors.Join(result, err)
			case <-ctx.Done():
				result = errors.Join(result, ctx.Err())
			}
		}
		if h.lock != nil {
			result = errors.Join(result, h.lock.Close())
		}
		h.closeErr = result
	})
	return h.closeErr
}
