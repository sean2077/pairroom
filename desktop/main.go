package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

type desktopController struct {
	hostMu sync.Mutex
	host   *host.Host

	quitting atomic.Bool
}

func (c *desktopController) setHost(value *host.Host) {
	c.hostMu.Lock()
	c.host = value
	c.hostMu.Unlock()
}

func (c *desktopController) shutdown(ctx context.Context) error {
	c.hostMu.Lock()
	value := c.host
	c.host = nil
	c.hostMu.Unlock()
	if value == nil {
		return nil
	}
	return value.Shutdown(ctx)
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
		Name:                       "pairroom-main",
		Title:                      "PairRoom",
		Width:                      1180,
		Height:                     760,
		MinWidth:                   900,
		MinHeight:                  600,
		URL:                        "/frontend/",
		BackgroundColour:           application.NewRGB(11, 16, 32),
		DefaultContextMenuDisabled: true,
		DevToolsEnabled:            false,
		JS:                         desktopWindowBridge,
	})
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
		go func() {
			startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			value, err := host.Start(startCtx, host.Options{})
			if err != nil {
				showStartupError(window, err)
				return
			}
			controller.setHost(value)
			navigateWindow(window, value.URL())
		}()
	})

	runErr := app.Run()
	controller.quitting.Store(true)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
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
