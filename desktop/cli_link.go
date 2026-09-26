package main

import (
	"errors"
	"os"
	"sync"

	"github.com/sean2077/pairroom/desktop/internal/clilink"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// cliLinkOffer puts the CLI bundled in PairRoom.app on PATH, because Native
// relay hooks and Agent tool shells run the bare `pairroom` command. It is
// active only when the host runs from a bundle that carries the CLI (macOS);
// elsewhere every method is a no-op and no menu item is shown.
type cliLinkOffer struct {
	app      *application.App
	cli      string
	stateDir string
	item     *application.MenuItem
	once     sync.Once
}

func newCLILinkOffer(app *application.App) *cliLinkOffer {
	offer := &cliLinkOffer{app: app}
	if executable, err := os.Executable(); err == nil {
		offer.cli = clilink.BundledCLI(executable)
	}
	if dir, err := clilink.StateDir(); err == nil {
		offer.stateDir = dir
	}
	return offer
}

func (o *cliLinkOffer) supported() bool { return o.cli != "" }

// addMenuItem adds the explicit tray entry; it stays available after a
// declined first-launch offer.
func (o *cliLinkOffer) addMenuItem(menu *application.Menu) {
	if !o.supported() {
		return
	}
	o.item = menu.Add("Install Command Line Tool…").
		SetTooltip("Link " + clilink.LinkPath + " to the pairroom CLI in this app so Agents and Native relay hooks can run it. macOS asks for an administrator password.").
		OnClick(func(*application.Context) { go o.install(true) })
	o.sync()
}

func (o *cliLinkOffer) sync() {
	if o.item == nil {
		return
	}
	switch clilink.Inspect(o.cli, clilink.LinkPath) {
	case clilink.StateInstalled:
		o.item.SetLabel("Command Line Tool Installed").SetEnabled(false)
	case clilink.StateStale:
		o.item.SetLabel("Update Command Line Tool…").SetEnabled(true)
	default:
		o.item.SetLabel("Install Command Line Tool…").SetEnabled(true)
	}
}

// offerOnce asks at most once per profile, after the Service has started, and
// only when no link exists yet. A stale link from an older bundle is updated
// by the tray item instead of interrupting startup. Service restarts call it
// again; sync.Once keeps it to one prompt per process.
func (o *cliLinkOffer) offerOnce() {
	o.once.Do(func() {
		if !o.supported() || o.stateDir == "" || clilink.Declined(o.stateDir) ||
			clilink.Inspect(o.cli, clilink.LinkPath) != clilink.StateMissing {
			return
		}
		dialog := o.app.Dialog.Question().
			SetTitle("Install the pairroom command?").
			SetMessage("Native relay hooks and your coding Agents run the pairroom command. Link " + clilink.LinkPath + " to the CLI inside PairRoom.app? macOS will ask for an administrator password. You can do this later from the menu bar icon.")
		install := dialog.AddButton("Install")
		later := dialog.AddButton("Not Now")
		install.OnClick(func() { go o.install(false) })
		later.OnClick(func() {
			if err := clilink.RecordDeclined(o.stateDir); err != nil {
				o.app.Logger.Error("could not record the declined command-line tool offer", "error", err)
			}
		})
		dialog.SetDefaultButton(install).SetCancelButton(later).Show()
	})
}

func (o *cliLinkOffer) install(explicit bool) {
	err := clilink.Install(o.cli, clilink.LinkPath)
	application.InvokeAsync(o.sync)
	switch {
	case err == nil:
		o.app.Dialog.Info().
			SetTitle("pairroom command installed").
			SetMessage(clilink.LinkPath + " now runs the CLI in this PairRoom.app. Restart Agent sessions and terminals that were already open so they find it.").
			Show()
	case errors.Is(err, clilink.ErrCancelled):
		if !explicit && o.stateDir != "" {
			_ = clilink.RecordDeclined(o.stateDir)
		}
	default:
		o.app.Logger.Error("could not install the command-line tool", "error", err)
		o.app.Dialog.Error().SetTitle("Could not install the pairroom command").SetMessage(err.Error()).Show()
	}
}
