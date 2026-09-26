# Installation

This guide owns the installation facts: which channel fits which platform, prerequisites, per-channel upgrade and uninstall mechanics, and the one-time Windows NSIS transition. Version-to-version data and schema semantics live in [Upgrading](UPGRADING.md); runtime lifecycle, backup, and shutdown live in [Operations](OPERATIONS.md); building from source lives in [Contributing](../CONTRIBUTING.md) and [Desktop development](../desktop/README.md).

## Choose an entry

| Entry | You get | Choose it for |
|---|---|---|
| Desktop package | Native host app plus a bundled `pairroom` CLI | Daily use: tray, embedded Service, native Settings |
| CLI only | The `pairroom` binary | Headless or SSH-forwarded hosts, daemon-only machines, browser-driven use |

Neither entry requires Go. Both need Git and a local Git repository; for real Agents, each selected native CLI (Claude Code, Codex, Grok Build) must be independently installed and authenticated. The full readiness table is in [Getting started](GETTING_STARTED.md#prerequisites).

Release packages are unsigned development artifacts until Windows code signing and Apple Developer ID signing/notarization actually run. [Releases](https://github.com/sean2077/pairroom/releases/latest) distinguish `pairroom-cli-…` from `pairroom-desktop-…` assets; verify downloads against the checksums published with each release.

## Windows desktop

### winget

The desktop package is published to winget as `sean2077.PairRoom` (moniker `pairroom`):

```powershell
winget install PairRoom
winget upgrade PairRoom
winget uninstall PairRoom
```

The manifest is machine-scoped and declares the Microsoft Edge WebView2 Runtime as a package dependency, so winget provisions the runtime before the installer runs.

### Setup.exe from Releases

Download `pairroom-desktop-vX.Y.Z-windows-amd64-setup.exe`; it is not the standalone CLI `.exe`. The Inno Setup installer is machine-scoped, defaults to `C:\Program Files\PairRoom contributors\PairRoom`, remembers an initial custom directory for subsequent upgrades, installs `PairRoom.exe` plus `bin\pairroom.exe`, and creates a Start Menu entry.

A missing machine-wide WebView2 Evergreen Runtime is installed before the payload. Internet access is needed only if the runtime is missing; for offline systems, preinstall Microsoft's standalone Evergreen Runtime. Silent runs give the bootstrapper a bounded five-minute budget to provision the runtime, then fail with preinstallation guidance instead of waiting indefinitely. A runtime installation error stops Setup instead of reporting that PairRoom was installed successfully.

Silent install/upgrade and uninstall work without launching the desktop:

```powershell
# Use the downloaded release filename in place of the local build name as needed.
.\pairroom-desktop-vX.Y.Z-windows-amd64-setup.exe /VERYSILENT /SUPPRESSMSGBOXES /NORESTART
& 'C:\Program Files\PairRoom contributors\PairRoom\unins000.exe' /VERYSILENT /SUPPRESSMSGBOXES /NORESTART
```

Quit Desktop from the tray before upgrading or removing it. If an explicitly installed daemon uses the bundled CLI, gracefully stop it first, or run `pairroom daemon uninstall` before removal. The installer never kills a process, installs/removes a daemon, changes PATH, or opts into launch at login. Inno removes only its logged files and shortcuts, preserving Service data and unrelated files. Uninstall removes the current user's native `PairRoom` Run entry only when it points exactly at this installation; it does not edit other users' registrations.

### First transition from NSIS

The old NSIS uninstaller recursively deletes its installation directory. Installing Inno over it would leave two uninstallers able to remove the same payload. Setup therefore rejects an older registered PairRoom installer, or a destination still containing `uninstall.exe`, before writing files. It never executes an arbitrary registry uninstall command or silently imports installer state.

Back up the Service data folder (available from the tray), quit Desktop and remove any daemon that uses the bundled CLI, then uninstall the old package from Windows Settings. Run the new installer and use the same directory if preserving existing launch paths. The new installer does not purge Service data; restore or re-enable explicit startup/daemon settings as necessary after this one-time transition. Later Inno versions upgrade in place. `make desktop-update` replaces binaries only; it does not convert an old NSIS installation into Inno.

## macOS desktop

Download `pairroom-desktop-vX.Y.Z-darwin-arm64.app.zip` (Apple silicon) or `-darwin-amd64.app.zip` (Intel), unzip, and move `PairRoom.app` to Applications. The bundle carries the CLI at `Contents/Helpers/pairroom` and the host at `Contents/MacOS/PairRoom`. Because the bundle is unsigned and un-notarized, Gatekeeper requires an explicit first-launch approval (right-click → Open, or allow it in System Settings → Privacy & Security).

## Linux desktop

- `.deb` (amd64): install with your package manager, e.g. `sudo apt install ./pairroom-desktop-vX.Y.Z-linux-amd64.deb`. It includes the CLI at `/usr/local/bin/pairroom`.
- `.AppImage` (amd64): make the file executable and run it directly; no system installation.

## CLI on any platform

For Linux, macOS, or Git Bash, the CLI installer is:

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh -o install-pairroom.sh
# Inspect install-pairroom.sh before executing it.
sh install-pairroom.sh
pairroom version
```

Alternatively, download the matching `pairroom-cli-…` asset directly, verify it against the release checksums, make it executable where required, and put it on `PATH`. In Windows PowerShell, use `./pairroom.exe` when running a downloaded binary in the current directory.

## From source

`make install` installs the CLI from the current tree to `GOBIN`; `make desktop-update` rebuilds and updates an existing native desktop installation while preserving user data and launch-at-login registration. Source development is a separate path from running releases: follow [Contributing](../CONTRIBUTING.md) for the development setup and [Desktop development](../desktop/README.md#update-the-installed-desktop-from-source) for desktop requirements and custom paths.

## Verify an installation

```bash
pairroom version
pairroom doctor
```

Command boundaries are documented in the [CLI reference](CLI_REFERENCE.md). [Agent-assisted setup](AGENT_SETUP.md) sequences these checks, including PATH in each Agent's tool shell, for a coding Agent to run with the user. For the desktop, launch PairRoom and open the Management URL it prints; the URL can contain an authentication token, so do not publish it.

## Upgrade and uninstall mechanics

Read [Upgrading](UPGRADING.md) before changing versions; it owns backup, compatibility, verification, and rollback semantics. Per-channel mechanics:

| Channel | Upgrade | Uninstall | Service data |
|---|---|---|---|
| winget | `winget upgrade PairRoom` | `winget uninstall PairRoom` | Preserved |
| Windows setup.exe | Run the newer installer | `unins000.exe` (quiet flags above) | Preserved |
| macOS `.app` | Replace the app bundle | Delete `PairRoom.app` | Preserved |
| Linux `.deb` | Install the newer package | Remove via package manager | Preserved |
| Linux `.AppImage` | Replace the file | Delete the file | Preserved |
| CLI `install.sh` | Re-run the installer | Remove the binary from `PATH` | Preserved |
| Source (`make desktop-update` / `make install`) | Re-run the target | Replace or remove the built binaries | Preserved |

No channel removes the Service data root, Room archives, or native CLI credentials. Quit Desktop and gracefully stop any installed daemon before upgrade or removal; the Windows installer never terminates active work.
