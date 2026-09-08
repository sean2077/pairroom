# PairRoom Desktop (Wails v3)

`desktop/` is a native Windows, macOS, and Linux host built with Wails v3. It is an isolated Go module that reuses PairRoom's existing Go control plane directly.

## Architecture

The desktop process does not launch a second-language sidecar and does not reimplement daemon discovery, service locks, session bindings, Room runtimes, or graceful Turn draining.

Startup follows this order:

1. validate and reuse `PAIRROOM_DESKTOP_URL` when explicitly supplied;
2. discover an installed `pairroom daemon`, recover a crash-stale `service.lock` after the recorded PID is gone, start or restart it when needed, and wait for its authenticated Management URL;
3. only when no daemon is installed, start the existing PairRoom Service in-process on an ephemeral numeric-loopback listener;
4. if an installation exists but cannot become reachable, fail closed with repair guidance instead of starting a competing Service.

The Wails layer owns only native desktop concerns:

- a single application instance;
- the main webview window;
- hide-to-tray behavior;
- explicit native quit;
- explicit, native launch-at-login settings;
- platform packaging.

The root and desktop modules use Go 1.25. The root permits only the pinned CGo-free SQLite dependency closure used for read-only CC Switch access; Wails and its GUI dependencies remain confined to `desktop/go.mod`.

## Launch at login

Opening Desktop never installs a daemon or enables startup registration. In
**Settings → Desktop → Launch at login**, explicitly enable or disable launching
PairRoom Desktop when you sign in. This uses Wails' native OS registration
(Windows Run key, macOS login item/LaunchAgent, or Linux XDG autostart), not
`pairroom daemon install`. The OS registration persists the choice; it is read
again when opening this settings section, and failed changes remain visible.
The setting is unavailable in ordinary browsers. It does not remove or
reconfigure a daemon installed previously; daemon administration remains an
explicit CLI operation.

## Development

Install Go 1.25 and the pinned Wails CLI:

```bash
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.16
```

Prepare generated packaging assets and build the host platform:

```bash
cd desktop
go mod tidy
python scripts/prepare-build.py
wails3 task build
```

From the repository root, the same workflows are available as `make desktop-build` and `make desktop-package`. `desktop-package` creates the production package for the current host platform under `desktop/bin/`; it requires the pinned Wails CLI and the platform packaging tools listed by the Wails toolchain. Set `DESKTOP_PYTHON` or `DESKTOP_WAILS` when those executables are not on the default command path.

### Update the installed desktop from source

Quit PairRoom using **Quit PairRoom** in its tray menu, then from the repository root:

```bash
make desktop-update
```

This builds the current checkout in production mode, then replaces the host and
bundled CLI together. Windows and Linux build directly without generating NSIS
or Linux distribution packages; macOS builds and ad-hoc signs the complete app
bundle. The macOS CLI lives under `Contents/Helpers/pairroom`, not beside
`Contents/MacOS/PairRoom`, so case-insensitive volumes cannot overwrite the host.
Go 1.25, the pinned Wails CLI, Python, and native build dependencies are
still required. There is no release download or automatic `git pull`.

Existing Windows NSIS installations are discovered through uninstall metadata
and the standard user/machine paths. macOS checks `~/Applications` and
`/Applications`. Linux checks `~/.local/lib/pairroom-desktop`, `~/.local/bin`,
and the package's `/usr/local/bin`. A missing or ambiguous installation fails
with guidance instead of silently installing a second copy. For custom paths:

```bash
make desktop-update DESKTOP_INSTALL_DIR="C:/Program Files/PairRoom contributors/PairRoom"
# macOS: specify the parent of PairRoom.app, not the bundle itself.
make desktop-update DESKTOP_INSTALL_DIR="$HOME/Applications"
```

Use the same directory to retain existing shortcuts and launch-at-login paths.
An explicit directory also permits copying a standalone local build for the first
time, but does not create shortcuts, install WebView2, or update package-manager
metadata; use a desktop package for initial installation. A downloaded AppImage
is a portable package, not a native installation directory: rebuild it with
`make desktop-package` rather than replacing it with an unbundled executable.

The updater stages both binaries before touching the installation and rolls back
replacement failures. It preserves user data, OS startup registration, the
Windows uninstaller, and unrelated files. It never kills processes or changes
daemon state. If Windows locks a binary, quit the desktop (or gracefully stop
an explicitly installed daemon holding the bundled CLI) and retry. Protected
installation directories require write permission. Reopen Desktop after updating;
an already-running Unix process continues using its previous binary until quit.
`DESKTOP_PYTHON` and `DESKTOP_WAILS` can point to alternate tool locations.

On Windows, `wails3 task build` links a GUI-subsystem `bin/PairRoom.exe`, so launching it from Explorer does not open a log console. Use `wails3 task build CONSOLE=true` only when you need stdout attached to a terminal.

Ctrl+click a web link in the Management Shell or a Room View to open it in the system default browser (Cmd+click also works on macOS). Download links retain their normal behavior.

Run tests:

```bash
cd desktop
go test ./...
go run ./scripts/verify-assets.go
```

The desktop host accepts these optional environment variables:

- `PAIRROOM_DESKTOP_URL`: authenticated numeric-loopback Management URL to reuse;
- `PAIRROOM_DESKTOP_CONFIG`: PairRoom JSON configuration for an explicitly embedded Service;
- `PAIRROOM_DESKTOP_DATA_ROOT`: absolute Service data root for an explicitly embedded Service.

Quit never stops an external daemon. Startup may restart an installed daemon only after recovering a crash-stale lock whose recorded PID is gone. An embedded Service is shut down in the existing safe order: stop Management admission, drain Room runtimes without interrupting active native Turns, then release `service.lock`.

## Packages

`.github/workflows/desktop-wails.yml` verifies the desktop module on pull requests and `main`. PR checks also rebuild and update temporary native installations. Release installer/app-bundle artifact collection runs only for `v*` tags (and manual `workflow_dispatch`), then attaches the tag artifacts to the GitHub Release as `pairroom-desktop-vX.Y.Z-…`:

- Linux amd64: AppImage and Debian package (the `.deb` includes `/usr/local/bin/pairroom`);
- Windows amd64: NSIS setup (`pairroom-desktop-vX.Y.Z-windows-amd64-setup.exe`) that installs `PairRoom.exe` and `bin\pairroom.exe`;
- macOS arm64: `.app.zip` with the CLI at `Contents/Helpers/pairroom` and host at `Contents/MacOS/PairRoom`;
- macOS amd64: `.app.zip` with the CLI at `Contents/Helpers/pairroom` and host at `Contents/MacOS/PairRoom`.

Release packages are unsigned development artifacts until Windows code signing and Apple Developer ID signing/notarization actually run.
