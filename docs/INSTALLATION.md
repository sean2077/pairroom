# Installation

This guide owns the installation facts: which channel fits which platform, prerequisites, per-channel upgrade and uninstall mechanics, and the one-time transition from the older Windows installer (NSIS). Version-to-version data and schema semantics live in [Upgrading](UPGRADING.md); runtime lifecycle, backup, and shutdown live in [Operations](OPERATIONS.md); building from source lives in [Contributing](../CONTRIBUTING.md) and [Desktop development](../desktop/README.md).

## Choose an entry

| Entry | You get | Choose it for |
|---|---|---|
| Desktop package | Desktop application plus a bundled `pairroom` CLI | Daily use: tray, embedded Service, native Settings |
| CLI only | The `pairroom` binary | Headless or SSH-forwarded hosts, daemon-only machines, browser-driven use |

Neither entry requires Go. Both need Git and a local Git repository; for real Agents, each selected native CLI ([Claude Code](https://code.claude.com/docs/en/overview), [Codex](https://github.com/openai/codex), [Grok Build](https://x.ai/news/grok-build-cli)) must be independently installed and authenticated. These are coding applications, not model names. The full readiness table is in [Getting started](GETTING_STARTED.md#prerequisites).

Release packages are unsigned development artifacts until Windows code signing and Apple Developer ID signing/notarization actually run. [Releases](https://github.com/sean2077/pairroom/releases/latest) distinguish `pairroom-cli-…` from `pairroom-desktop-…` assets. Verify downloads against the checksums published with each release: CLI assets, `install.sh`, and source archives are listed in `SHA256SUMS`, and desktop packages in `pairroom-desktop-vX.Y.Z-SHA256SUMS` (for example `sha256sum -c --ignore-missing pairroom-desktop-vX.Y.Z-SHA256SUMS` next to the downloaded package; on Windows compare `Get-FileHash` output with the listed digest). The checksums detect corrupted or substituted downloads relative to that Release page; they are not a code signature.

## Windows desktop

### winget

The desktop package is published to [WinGet](https://learn.microsoft.com/en-us/windows/package-manager/winget/), Microsoft's Windows package manager, as `sean2077.PairRoom` (moniker `pairroom`):

```powershell
winget install PairRoom
winget upgrade PairRoom
winget uninstall PairRoom
```

The manifest is machine-scoped and declares the Microsoft Edge WebView2 Runtime as a package dependency, so winget provisions the runtime before the installer runs.

### Setup.exe from Releases

Download `pairroom-desktop-vX.Y.Z-windows-amd64-setup.exe`; it is not the standalone CLI `.exe`. The [Inno Setup](https://jrsoftware.org/isinfo.php) installer is machine-scoped, defaults to `C:\Program Files\PairRoom contributors\PairRoom`, remembers an initial custom directory for subsequent upgrades, installs `PairRoom.exe` plus `bin\pairroom.exe`, and creates a Start Menu entry.

A missing machine-wide WebView2 Evergreen Runtime is installed before the payload. Internet access is needed only if the runtime is missing; for offline systems, preinstall Microsoft's standalone Evergreen Runtime. Silent runs give the bootstrapper a bounded five-minute budget to provision the runtime, then fail with preinstallation guidance instead of waiting indefinitely. A runtime installation error stops Setup instead of reporting that PairRoom was installed successfully.

Silent install/upgrade and uninstall work without launching the desktop:

```powershell
# Use the downloaded release filename in place of the local build name as needed.
.\pairroom-desktop-vX.Y.Z-windows-amd64-setup.exe /VERYSILENT /SUPPRESSMSGBOXES /NORESTART
& 'C:\Program Files\PairRoom contributors\PairRoom\unins000.exe' /VERYSILENT /SUPPRESSMSGBOXES /NORESTART
```

Quit Desktop from the tray before upgrading or removing it. If an explicitly installed daemon uses the bundled CLI, gracefully stop it first, or run `pairroom daemon uninstall` before removal. The installer never kills a process, installs/removes a daemon, or opts into launch at login. Inno removes only its logged files and shortcuts, preserving Service data and unrelated files. Uninstall removes the current user's native `PairRoom` Run entry only when it points exactly at this installation; it does not edit other users' registrations.

### The `pairroom` command on PATH

By default Setup appends `<install dir>\bin` to the **machine** PATH, so Native relay hooks and each Agent's tool shell can run the bare `pairroom` command. Only shells and harnesses started afterwards see it; restart an Agent session that was already running. Machine PATH entries precede user entries, so this bundled CLI, which matches the Desktop's Service, takes precedence over a copy installed per user.

Opt out with the **Add the pairroom command-line tool to the system PATH** task, or `/MERGETASKS="!addtopath"` for a silent run. The choice is remembered: a later upgrade keeps it, and running Setup with the task cleared removes the entry it added. Setup adds the entry only once, appends it without reordering other entries, and never edits the user PATH. Uninstall removes only that entry.

### First transition from NSIS

The old NSIS uninstaller recursively deletes its installation directory. Installing Inno over it would leave two uninstallers able to remove the same payload. Setup therefore rejects an older registered PairRoom installer, or a destination still containing `uninstall.exe`, before writing files. It never executes an arbitrary registry uninstall command or silently imports installer state.

Back up the Service data folder (available from the tray), quit Desktop and remove any daemon that uses the bundled CLI, then uninstall the old package from Windows Settings. Run the new installer and use the same directory if preserving existing launch paths. The new installer does not purge Service data; restore or re-enable explicit startup/daemon settings as necessary after this one-time transition. Later Inno versions upgrade in place. `make desktop-update` replaces binaries only; it does not convert an old NSIS installation into Inno.

## macOS desktop

Download `pairroom-desktop-vX.Y.Z-darwin-arm64.app.zip` (Apple silicon) or `-darwin-amd64.app.zip` (Intel), unzip, and move `PairRoom.app` to Applications. Desktop requires macOS 12.3 or later, the first release whose WebView provides the Web Locks that Native Room publication needs. The bundle carries the CLI at `Contents/Helpers/pairroom` and the host at `Contents/MacOS/PairRoom`. Because the bundle is unsigned and un-notarized, Gatekeeper requires an explicit first-launch approval (right-click → Open, or allow it in System Settings → Privacy & Security).

The bundled CLI is not on PATH by itself. After the Service starts for the first time, Desktop offers to link `/usr/local/bin/pairroom` to it; macOS asks for an administrator password. **Not Now** stops the offer, and the menu bar item **Install Command Line Tool…** does it later. The link points into the bundle, so replacing `PairRoom.app` in place keeps it current; after moving the app, use **Update Command Line Tool…**. Desktop never replaces an existing `/usr/local/bin/pairroom` it did not create. Restart Agent sessions opened before the link existed. To remove it, delete `/usr/local/bin/pairroom`.

## Linux desktop

- `.deb` (amd64): install with your package manager, e.g. `sudo apt install ./pairroom-desktop-vX.Y.Z-linux-amd64.deb`. It includes the CLI at `/usr/local/bin/pairroom`.
- `.AppImage` (amd64): make the file executable and run it directly; no system installation. It does not expose the bundled CLI on PATH, so install a matching CLI separately before using Native hooks.

## CLI on any platform

Published CLI targets are Linux amd64, Windows amd64, and macOS amd64/arm64. `amd64` means x86-64, including Intel and AMD processors; macOS arm64 is Apple silicon. The installer rejects other Linux/Windows architectures rather than silently selecting a different binary. A source build is a separate path, not a claim that an unlisted release asset exists.

For Linux, macOS, or Git Bash, use a scratch directory and download the CLI installer:

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh -o install-pairroom.sh
```

After the download succeeds, inspect the file in your editor. Only then execute it:

```bash
sh install-pairroom.sh
pairroom version
```

The installer is POSIX `sh` (it runs under dash, busybox `sh`, and bash), and needs `curl` plus either `sha256sum` or `shasum`. It refuses installation without a checksum tool or when the binary's SHA-256 does not match the release `SHA256SUMS`. Downloading before execution also avoids treating a failed download in a shell pipeline as a successful installation.

It installs the latest release unless `PAIRROOM_VERSION` names a tag. The destination is `$PREFIX/bin` when `PREFIX` is set; otherwise it uses `/usr/local/bin` when writable (or running as root), then `~/.local/bin`. It downloads from `sean2077/pairroom` unless `PAIRROOM_REPOSITORY` names another `owner/name` for a fork; it ignores the generic `GITHUB_REPOSITORY` that GitHub Actions sets for the calling repository. These variables affect the CLI installer, not a running Desktop or Service.

Alternatively, download the matching `pairroom-cli-…` asset directly, verify it against the release checksums, make it executable where required, and put it on `PATH`. In Windows PowerShell, use `./pairroom.exe` when running a downloaded binary in the current directory.

## From source

`make install` installs the CLI from the current tree to `GOBIN`; `make desktop-update` rebuilds and updates an existing native desktop installation while preserving user data and launch-at-login registration. Source development is a separate path from running releases: follow [Contributing](../CONTRIBUTING.md) for the development setup and [Desktop development](../desktop/README.md#update-the-installed-desktop-from-source) for desktop requirements and custom paths.

## Verify an installation

Run these from the intended project repository:

```bash
pairroom version
pairroom doctor --repo . --json
```

`version` confirms which CLI release this shell runs. Ordinary `doctor` checks executable/version/protocol availability without calling a model; its default pair is Claude Code and Codex. For another pair, retain the appropriate `--config` and command-path overrides described in [Agent-assisted setup](AGENT_SETUP.md#3-check-git-and-the-runtimes). Installation is not authentication.

For Native, repeat the PATH check in both Agents' tool shells, use the same CLI release as the Service, and run `pairroom relay preflight` before binding. Use `pairroom relay doctor` only after binding to inspect the session and evidence of hook execution. Neither command proves model acceptance; live model checks need separate consent. [Agent-assisted setup](AGENT_SETUP.md#6-native-bind-two-existing-sessions) owns the sequence and [CLI reference](CLI_REFERENCE.md) owns the command contracts.

For the desktop, launch PairRoom and open Management in its window. For a foreground CLI Service, use its printed Management URL; the URL can contain an authentication token, so do not publish it.

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

No channel updates itself. Desktop can optionally tell you when a newer stable release exists: turn on **Settings → Desktop → Updates → Check for updates** (off by default). It links to the release page and never downloads or installs anything, so upgrade with the channel above; see [Desktop development](../desktop/README.md#check-for-updates) for the exact request.

No channel removes the Service data root, Room archives, or native CLI credentials. Quit Desktop and gracefully stop any installed daemon before upgrade or removal; the Windows installer never terminates active work.
