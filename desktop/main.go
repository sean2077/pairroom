package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sean2077/pairroom/desktop/internal/host"
	"github.com/sean2077/pairroom/desktop/internal/startup"
	"github.com/sean2077/pairroom/internal/webui"
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

// desktopWindowGate prevents the asynchronous Service bootstrap from trying
// to execute JavaScript before Wails has created the native window. Wails'
// WebviewWindow.ExecJS silently returns when its implementation is not ready;
// without this gate a fast startup can leave the splash page visible forever.
// Actions submitted before the first WindowRuntimeReady event are replayed in
// submission order once the WebView can accept them.
type desktopWindowGate struct {
	mu       sync.Mutex
	ready    bool
	draining bool
	pending  []func()
}

func (g *desktopWindowGate) submit(action func()) {
	if action == nil {
		return
	}
	g.mu.Lock()
	g.pending = append(g.pending, action)
	startDrain := g.ready && !g.draining
	if startDrain {
		g.draining = true
	}
	g.mu.Unlock()
	if startDrain {
		g.drain()
	}
}

func (g *desktopWindowGate) markReady() {
	g.mu.Lock()
	if g.ready {
		g.mu.Unlock()
		return
	}
	g.ready = true
	startDrain := len(g.pending) > 0 && !g.draining
	if startDrain {
		g.draining = true
	}
	g.mu.Unlock()
	if startDrain {
		g.drain()
	}
}

func (g *desktopWindowGate) drain() {
	completed := false
	defer func() {
		if completed {
			return
		}
		// An action should not panic, but if it does, release ownership of the
		// drain loop so a later submission is not permanently blocked.
		g.mu.Lock()
		g.draining = false
		g.mu.Unlock()
	}()
	for {
		action, ok := g.nextAction()
		if !ok {
			completed = true
			return
		}
		action()
	}
}

func (g *desktopWindowGate) nextAction() (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.ready || len(g.pending) == 0 {
		// Clear the draining marker while still holding the mutex. A submit
		// racing with the end of the queue must either be observed by this
		// check or see draining=false and become the next drainer; clearing
		// after unlocking can strand an action indefinitely.
		g.draining = false
		return nil, false
	}
	action := g.pending[0]
	g.pending = g.pending[1:]
	return action, true
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
	c.hostMu.Lock()
	hasHost := c.host != nil
	c.hostMu.Unlock()
	if hasHost {
		if cancel != nil {
			cancel()
		}
		return
	}
	c.startupMu.Lock()
	if c.startupStarted || c.quitting.Load() {
		c.startupMu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	// Recheck under the startup gate. A prior bootstrap can publish its host
	// between the fast host check above and this lock acquisition; starting a
	// second bootstrap in that window would defeat the single-UI lifecycle.
	c.hostMu.Lock()
	hasHost = c.host != nil
	c.hostMu.Unlock()
	if hasHost {
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
			c.startupStarted = false
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
	windowGate := &desktopWindowGate{}
	var startDesktop func()
	var startupSettings *startup.Settings
	var openBrowser func(string) error

	app := application.New(application.Options{
		RawMessageHandler: func(sender application.Window, message string, origin *application.OriginInfo) {
			if window == nil || sender != window || origin == nil || startupSettings == nil {
				return
			}
			controller.hostMu.Lock()
			currentURL := controller.host.URL()
			controller.hostMu.Unlock()
			if !startup.TrustedOrigin(currentURL, origin.Origin, origin.TopOrigin, runtime.GOOS, origin.IsMainFrame) {
				return
			}
			if target, ok := desktopBrowserTarget(message); ok {
				if err := openBrowser(target); err != nil {
					window.ExecJS("window.alert('Could not open the default browser.');")
				}
				return
			}
			if response, ok := startupSettings.Handle(message); ok {
				encoded, _ := json.Marshal(response)
				window.ExecJS("window.PairRoomDesktop?.receive(" + string(encoded) + ");")
			}
		},
		Name:        "PairRoom",
		Description: "Claude Code and Codex local collaboration control plane",
		Assets: application.AssetOptions{
			// BundledAssetFileServer provides the Wails runtime at
			// /wails/runtime.js as well as the embedded startup assets. The
			// runtime-ready event is required to safely deliver the asynchronous
			// bootstrap result to the WebView.
			Handler: desktopAssetHandler(),
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
				// A first instance may have shown a startup diagnostic while the
				// daemon was still recovering. Reopening the app is also an explicit
				// retry, while the single-instance guard keeps one UI process.
				if startDesktop != nil {
					startDesktop()
				}
			},
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})

	// Reading or rendering Settings never opts a user into autostart.
	startupSettings = startup.New(app.Autostart)
	openBrowser = app.Browser.OpenURL

	window = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:      "pairroom-main",
		Title:     "PairRoom",
		Width:     1180,
		Height:    760,
		MinWidth:  900,
		MinHeight: 600,
		// The bundled asset server strips the single embedded frontend directory
		// and serves its contents from the webview root.
		URL:                        "/",
		BackgroundColour:           application.NewRGB(11, 16, 32),
		DefaultContextMenuDisabled: true,
		DevToolsEnabled:            false,
		JS:                         desktopWindowBridge,
	})
	window.OnWindowEvent(events.Common.WindowRuntimeReady, func(*application.WindowEvent) {
		windowGate.markReady()
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

	var statusMu sync.Mutex
	statusText := "Service: starting…"
	setServiceStatus := func(text string) {
		statusMu.Lock()
		statusText = text
		statusMu.Unlock()
	}
	// Declared before the menu actions that trigger it; assigned once the tray
	// items exist.
	var syncTray func()
	showWindow := func() {
		window.Restore()
		window.Show()
		window.Focus()
		if startDesktop != nil {
			startDesktop()
		}
	}
	currentHost := func() *host.Host {
		controller.hostMu.Lock()
		defer controller.hostMu.Unlock()
		return controller.host
	}

	menu := app.NewMenu()
	menu.Add("Open PairRoom").OnClick(func(*application.Context) { showWindow() })
	openBrowserItem := menu.Add("Open Management in Browser").OnClick(func(*application.Context) {
		target := currentHost().BrowserURL()
		if target == "" {
			return
		}
		if err := openBrowser(target); err != nil {
			app.Logger.Error("could not open the Management Shell in the default browser", "error", err)
		}
	})
	menu.AddSeparator()
	statusItem := menu.Add(statusText).SetEnabled(false)
	restartItem := menu.Add("Restart Service").
		SetTooltip("Drains active Turns and restarts the Service this Desktop owns. An installed daemon is restarted with `pairroom daemon restart`.").
		OnClick(func(*application.Context) {
			controller.hostMu.Lock()
			value := controller.host
			embedded := value.Mode() == host.ModeEmbedded
			if embedded {
				controller.host = nil
			}
			controller.hostMu.Unlock()
			if !embedded {
				return
			}
			setServiceStatus("Service: restarting…")
			syncTray()
			go func() {
				drainCtx, cancel := context.WithTimeout(context.Background(), desktopShutdownTimeout)
				err := value.Shutdown(drainCtx)
				cancel()
				if err != nil {
					app.Logger.Error("embedded Service did not drain cleanly before tray restart", "error", err)
				}
				if startDesktop != nil {
					startDesktop()
				}
			}()
		})
	dataFolderItem := menu.Add("Open Service Data Folder").OnClick(func(*application.Context) {
		root := currentHost().DataRoot()
		if root == "" {
			return
		}
		if err := openLocalFolder(root); err != nil {
			app.Logger.Error("could not open the Service data folder", "error", err)
		}
	})
	menu.AddSeparator()
	menu.Add("Quit PairRoom").OnClick(func(*application.Context) {
		requestQuit(app, controller)
	})
	tray.SetMenu(menu)

	// The tray mirrors Service ownership: disabled actions stay honest about
	// what this Desktop process may control.
	syncTray = func() {
		value := currentHost()
		statusMu.Lock()
		text := statusText
		statusMu.Unlock()
		if value != nil {
			text = "Service: embedded in Desktop"
			if value.Mode() == host.ModeExternal {
				text = "Service: installed daemon"
			}
		}
		statusItem.SetLabel(text)
		openBrowserItem.SetEnabled(value.BrowserURL() != "")
		restartItem.SetEnabled(value.Mode() == host.ModeEmbedded)
		dataFolderItem.SetEnabled(value.DataRoot() != "")
	}
	syncTray()
	tray.OnClick(showWindow)

	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) {
		showWindow()
	})
	startDesktop = func() {
		startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		controller.start(
			startCtx,
			cancel,
			func(ctx context.Context) (*host.Host, error) {
				return host.Start(ctx, host.Options{})
			},
			func(value *host.Host) {
				windowGate.submit(func() { navigateWindow(window, value.URL()) })
				syncTray()
			},
			func(err error) {
				windowGate.submit(func() { showStartupError(window, err) })
				setServiceStatus("Service: unavailable")
				syncTray()
			},
		)
	}
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		startDesktop()
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

func desktopAssetHandler() http.Handler {
	mux := http.NewServeMux()
	webui.Mount(mux)
	mux.Handle("/", application.BundledAssetFileServer(frontend))
	return mux
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

// openLocalFolder reveals a Service data directory with the platform file
// manager. The path comes from local Service state, never from web content, and
// is passed as a single argument so no shell interpretation is involved.
func openLocalFolder(path string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer", path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

func navigateWindow(window *application.WebviewWindow, value string) {
	encoded, _ := json.Marshal(value)
	window.ExecJS("window.location.replace(" + string(encoded) + ");")
}

func showStartupError(window *application.WebviewWindow, err error) {
	// Keep the native bridge locale-neutral. The startup page owns the bilingual
	// explanatory copy; only the diagnostic detail crosses the bridge verbatim.
	message := ""
	if err != nil {
		message = err.Error()
	}
	encoded, _ := json.Marshal(message)
	window.ExecJS("window.pairroomDesktopError(" + string(encoded) + ");")
}

// desktopBrowserTarget limits native browser requests to web links. In particular,
// file URLs and OS application protocols must never reach the shell opener.
func desktopBrowserTarget(message string) (string, bool) {
	if len(message) > 16384 {
		return "", false
	}
	var request struct {
		Kind string `json:"kind"`
		URL  string `json:"url"`
	}
	if json.Unmarshal([]byte(message), &request) != nil || request.Kind != "pairroom.desktop.browser" {
		return "", false
	}
	u, err := url.Parse(request.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", false
	}
	return u.String(), true
}

const desktopWindowBridge = `
(() => {
  if (window !== window.top || window.__pairroomWindowBridge) return;
  window.__pairroomWindowBridge = true;
  const transport = window.chrome?.webview || window.webkit?.messageHandlers?.external;
  const documents = new WeakSet();
  function watchDocument(doc) {
    if (!doc || documents.has(doc)) return;
    documents.add(doc);
    doc.addEventListener('click', (event) => {
      if (!event.isTrusted || event.button !== 0 || !(event.ctrlKey || event.metaKey)) return;
      const link = event.target.closest?.('a[href]');
      if (!link || link.hasAttribute('download') || typeof transport?.postMessage !== 'function') return;
      let target;
      try { target = new URL(link.href, doc.baseURI); } catch (_) { return; }
      if (!['http:', 'https:'].includes(target.protocol) || target.username || target.password) return;
      event.preventDefault();
      event.stopImmediatePropagation();
      transport.postMessage(JSON.stringify({kind: 'pairroom.desktop.browser', url: target.href}));
    }, true);
    function watchFrame(frame) {
      // Room Views are same-origin; never grant a foreign frame a bridge.
      try { watchDocument(frame.contentDocument); } catch (_) {}
    }
    doc.addEventListener('load', (event) => {
      if (event.target.tagName === 'IFRAME') watchFrame(event.target);
    }, true);
    doc.querySelectorAll('iframe').forEach(watchFrame);
  }
  watchDocument(document);

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
