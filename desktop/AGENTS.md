# PairRoom Desktop Agent Contract

## Scope

This contract applies under `desktop/`. Read it together with the repository root `AGENTS.md`.

## Module boundary

- `desktop/` is an isolated Go 1.25 module pinned to Wails v3. The root PairRoom module remains Go 1.23 and standard-library-only; never move Wails or GUI dependencies into the root `go.mod`.
- The desktop application is a native host, not a second PairRoom backend or frontend. Reuse the existing Management Shell, Room View, `internal/service`, Registry, Runtime Manager, provider configuration, locking, and native Agent adapters.
- Keep PairRoom lifecycle, authentication, numeric-loopback validation, `service.lock`, and graceful native-Turn draining in Go. Do not duplicate those contracts in JavaScript or platform-specific packaging scripts.
- Treat an installed PairRoom daemon as the sole owner of the default data root: desktop startup may start and connect to it, but must fail closed when it is unavailable instead of starting a competing embedded Service. Explicit `PAIRROOM_DESKTOP_URL`, `PAIRROOM_DESKTOP_CONFIG`, or `PAIRROOM_DESKTOP_DATA_ROOT` selections remain caller-owned overrides.
- Reusing an authenticated installed daemon never transfers ownership to the desktop app. An embedded Service is owned by the current desktop process and must shut down Management requests, drain Runtimes, and release its lock in that order.

## Wails and generated files

- Wails is pinned in `go.mod`; treat v3 API changes as explicit dependency upgrades requiring Windows, macOS, and Linux validation.
- Platform Taskfiles and non-templated packaging files under `build/darwin/`, `build/linux/`, and `build/windows/` are maintained source synchronized with the pinned Wails release. Update them deliberately when the Wails version changes.
- `python scripts/prepare-build.py` refreshes templated metadata and icon assets from the pinned Wails CLI. Generated plist, desktop, nfpm, icon, NSIS helper, `bin/`, and `dist/` outputs remain untracked through `.gitignore`.
- `build/config.yml`, `build/Taskfile.yml`, `Taskfile.yml`, and the scripts under `scripts/` are maintained source.
- Keep the startup page minimal. Product UI changes belong to the existing embedded PairRoom Web assets, not `desktop/frontend/`.
- Windows `wails3 task build` must stay GUI-subsystem (`-H windowsgui`) so `bin/PairRoom.exe` does not allocate a log console. `CONSOLE=true` is the explicit diagnostic exception.
- Packaged desktop builds must ship the PairRoom CLI with the host. On Windows the CLI cannot share a directory with `PairRoom.exe` (NTFS is case-insensitive); keep it at `desktop/bin/cli/pairroom.exe` in the build tree and `$INSTDIR\bin\pairroom.exe` in the installer. Unix packages may keep a `pairroom` sibling. CI collects the installer/app bundle, not a host binary without the CLI. If no daemon is installed, the host installs one from that bundled CLI instead of embedding a competing Service.

## Verification

From `desktop/`, run at minimum:

```bash
go mod tidy
go test -count=1 ./...
python scripts/prepare-build.py
wails3 task build
```

The dedicated GitHub workflow verifies the desktop module on pull requests and `main`. Linux, Windows, macOS arm64, and macOS amd64 packaging runs only for `v*` tags (or manual `workflow_dispatch`). Do not claim signed or notarized production installers unless the corresponding platform signing steps actually ran.
