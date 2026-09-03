# PairRoom Desktop (Wails v3)

`desktop/` is a native Windows, macOS, and Linux host built with Wails v3. It is an isolated Go module that reuses PairRoom's existing Go control plane directly.

## Architecture

The desktop process does not launch a second-language sidecar and does not reimplement daemon discovery, service locks, session bindings, Room runtimes, or graceful Turn draining.

Startup follows this order:

1. validate and reuse `PAIRROOM_DESKTOP_URL` when explicitly supplied;
2. discover an installed `pairroom daemon`, start it when it is stopped, and wait for its authenticated Management URL;
3. only when no daemon is installed, start the existing PairRoom Service in-process on an ephemeral numeric-loopback listener;
4. if an installation exists but cannot become reachable, fail closed with repair guidance instead of starting a competing Service.

The Wails layer owns only native desktop concerns:

- a single application instance;
- the main webview window;
- hide-to-tray behavior;
- explicit native quit;
- platform packaging.

The root module remains Go 1.23 and standard-library-only. Wails and its dependencies are confined to the nested `desktop/go.mod`, which uses Go 1.25 as required by Wails v3 beta.16.

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

On Windows, `wails3 task build` links a GUI-subsystem `bin/PairRoom.exe`, so launching it from Explorer does not open a log console. Use `wails3 task build CONSOLE=true` only when you need stdout attached to a terminal.

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

An installed daemon is never stopped by the desktop process. An embedded Service is shut down in the existing safe order: stop Management admission, drain Room runtimes without interrupting active native Turns, then release `service.lock`.

## Packages

`.github/workflows/desktop-wails.yml` validates and packages:

- Linux amd64: AppImage and Debian package (the `.deb` includes `/usr/local/bin/pairroom`);
- Windows amd64: NSIS installer that installs `PairRoom.exe` and `pairroom.exe`;
- macOS arm64: `.app.zip` with `pairroom` next to the host inside `Contents/MacOS`;
- macOS amd64: `.app.zip` with `pairroom` next to the host inside `Contents/MacOS`.

CI packages are unsigned development artifacts. Production Windows signing and Apple Developer ID signing/notarization remain release-credential concerns.
