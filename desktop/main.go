package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sean2077/pairroom/desktop/internal/host"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:frontend
var frontend embed.FS

//go:embed assets/icon.png
var applicationIcon []byte

var singleInstanceKey = [32]byte{
	0x70, 0x61, 0x69, 0x72, 0x72, 0x6f, 0x6f, 0x6d,
	0x2d, 0x77, 0x61, 0x69, 0x6c, 0x73, 0x2d, 0x76,
	0x33, 0x2d, 0x64, 0x65, 0x73, 0x6b, 0x74, 0x6f,
	0x70, 0x2d, 0x68, 0x6f, 0x73, 0x74, 0x21, 0x01,
}

const desktopShutdownTimeout = 11 * time.Minute

type desktopController struct {
	hostMu sync.Mutex
	host   *host.Host

	quitting atomic.Bool

	startupMu      sync.Mutex
	startupDone    chan struct{}
	startupCancel  context.CancelFunc
	startupStarted bool

	shutdownMu      sync.Mutex
	shutdownDone    chan struct{}
	shutdownErr     error
	shutdownStarted bool
}

// start launches the asynchronous host bootstrap while retaining enough
// lifecycle state for an early Quit to cancel and join it. A desktop user can
// close the application while daemon discovery or Registry startup is still in
// progress; in that case a host that finishes later must be shut down instead
// of being installed behind the already-closed window.
func (c *desktopController) start(
	ctx context.Context,
	cancel context.CancelFunc,
	start func(context.Context) (*host.Host, error),
	onReady func(*host.Host),
	onError func(error),
) {
	c.startupMu.Lock()
	if c.startupStarted || c.quitting.Load() {
		c.startupMu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	c.startupStarted = true
	done := make(chan struct{})
	c.startupDone = done
	c.startupCancel = cancel
	c.startupMu.Unlock()

	go func() {
		defer func() {
			if cancel != nil {
				cancel()
			}
			c.startupMu.Lock()
			c.startupDone = nil
			c.startupCancel = nil
			close(done)
			c.startupMu.Unlock()
		}()

		value, err := start(ctx)
		if err != nil {
			if !c.quitting.Load() && onError != nil {
				onError(err)
			}
			return
		}
		if !c.setHost(value) {
			if value != nil {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), desktopShutdownTimeout)
				_ = value.Shutdown(cleanupCtx)
				cleanupCancel()
			}
			return
		}
		if !c.quitting.Load() && onReady != nil {
			onReady(value)
		}
	}()
}

func (c *desktopController) setHost(value *host.Host) bool {
	c.hostMu.Lock()
	defer c.hostMu.Unlock()
	if c.quitting.Load() {
		return false
	}
	c.host = value
	return true
}

func (c *desktopController) shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.quitting.Store(true)

	c.shutdownMu.Lock()
	if !c.shutdownStarted {
		c.shutdownStarted = true
		c.shutdownDone = make(chan struct{})
		go c.performShutdown(c.shutdownDone)
	}
	done := c.shutdownDone
	c.shutdownMu.Unlock()

	select {
	case <-done:
		c.shutdownMu.Lock()
		err := c.shutdownErr
		c.shutdownMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *desktopController) performShutdown(done chan struct{}) {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), desktopShutdownTimeout)
	defer cleanupCancel()
	var result error

	c.startupMu.Lock()
	startupDone := c.startupDone
	startupCancel := c.startupCancel
	c.startupMu.Unlock()
	if startupCancel != nil {
		startupCancel()
	}
	if startupDone != nil {
		select {
		case <-startupDone:
		case <-cleanupCtx.Done():
			result = errors.Join(result, cleanupCtx.Err())
		}
	}

	c.hostMu.Lock()
	value := c.host
	c.host = nil
	c.hostMu.Unlock()
	if value != nil {
		result = errors.Join(result, value.Shutdown(cleanupCtx))
	}

	c.shutdownMu.Lock()
	c.shutdownErr = result
	close(done)
	c.shutdownMu.Unlock()
}

func main() {
	controller := &desktopController{}
	var window *application.WebviewWindow

	app := application.New(application.Options{
		Name:        "PairRoom",
		Description: "Claude Code and Codex local collaboration control plane",
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(frontend),
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID:      "com.sean2077.pairroom.desktop",
			EncryptionKey: singleInstanceKey,
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				if window != nil {
					window.Restore()
					window.Show()
					window.Focus()
				}
			},
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})

	window = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:      "pairroom-main",
		Title:     "PairRoom",
		Width:     1180,
		Height:    760,
		MinWidth:  900,
		MinHeight: 600,
		// AssetFileServerFS strips the single embedded frontend directory and
		// serves its contents from the webview root.
		URL:                        "/",
		BackgroundColour:           application.NewRGB(11, 16, 32),
		DefaultContextMenuDisabled: true,
		DevToolsEnabled:            false,
		JS:                         desktopWindowBridge,
	})
	if runtime.GOOS == "windows" {
		// Wails v3's Windows backend only turns WebviewWindowOptions.JS into a
		// document-created script when HTML (rather than URL) is supplied. The
		// desktop uses a URL so the embedded asset handler can serve the page;
		// inject the bridge after each WebView2 navigation instead.
		window.OnWindowEvent(events.Windows.WebViewNavigationCompleted, func(*application.WindowEvent) {
			window.ExecJS(desktopWindowBridge)
		})
	}
	window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		if controller.quitting.Load() {
			return
		}
		window.Hide()
		event.Cancel()
	})

	tray := app.SystemTray.New()
	tray.SetIcon(applicationIcon)
	tray.SetTooltip("PairRoom")
	menu := app.NewMenu()
	menu.Add("Open PairRoom").OnClick(func(*application.Context) {
		window.Restore()
		window.Show()
		window.Focus()
	})
	menu.Add("Quit PairRoom").OnClick(func(*application.Context) {
		requestQuit(app, controller)
	})
	tray.SetMenu(menu)
	tray.OnClick(func() {
		window.Restore()
		window.Show()
		window.Focus()
	})

	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) {
		window.Restore()
		window.Show()
		window.Focus()
	})
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		controller.start(
			startCtx,
			cancel,
			func(ctx context.Context) (*host.Host, error) {
				return host.Start(ctx, host.Options{})
			},
			func(value *host.Host) { navigateWindow(window, value.URL()) },
			func(err error) { showStartupError(window, err) },
		)
	})

	runErr := app.Run()
	controller.quitting.Store(true)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), desktopShutdownTimeout)
	shutdownErr := controller.shutdown(shutdownCtx)
	cancel()
	if err := errors.Join(runErr, shutdownErr); err != nil {
		log.Fatal(fmt.Errorf("PairRoom desktop stopped: %w", err))
	}
}

func requestQuit(app *application.App, controller *desktopController) {
	if !controller.quitting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), desktopShutdownTimeout)
		err := controller.shutdown(shutdownCtx)
		cancel()
		if err != nil {
			app.Logger.Error("desktop shutdown did not drain cleanly", "error", err)
		}
		app.Quit()
	}()
}

func navigateWindow(window *application.WebviewWindow, value string) {
	encoded, _ := json.Marshal(value)
	window.ExecJS("window.location.replace(" + string(encoded) + ");")
}

func showStartupError(window *application.WebviewWindow, err error) {
	encoded, _ := json.Marshal(err.Error())
	window.ExecJS("window.pairroomDesktopError(" + string(encoded) + ");")
}

const desktopWindowBridge = `
(() => {
  function isNumericLoopback(url) {
    if (url.protocol === "about:") return url.href === "about:blank";
    if (url.protocol !== "http:" || !url.port) return false;
    const host = url.hostname.replace(/^\[|\]$/g, "");
    if (host === "::1") return true;
    const parts = host.split(".");
    if (parts.length !== 4 || parts[0] !== "127") return false;
    return parts.every((part) => /^\d{1,3}$/.test(part) && Number(part) <= 255);
  }

  window.open = (raw) => {
    let target;
    try {
      target = new URL(String(raw || "about:blank"), window.location.href);
    } catch (_) {
      return null;
    }
    if (!isNumericLoopback(target)) return null;

    const placeholder = document.implementation.createHTMLDocument("PairRoom");
    const proxy = {
      closed: false,
      document: placeholder,
      focus() { window.focus(); },
      close() { this.closed = true; },
      location: {
        replace(next) {
          let resolved;
          try {
            resolved = new URL(String(next), window.location.href);
          } catch (_) {
            return;
          }
          if (isNumericLoopback(resolved)) window.location.assign(resolved.href);
        }
      }
    };
    if (target.href !== "about:blank") proxy.location.replace(target.href);
    return proxy;
  };
})();
`
