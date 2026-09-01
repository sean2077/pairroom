# PairRoom Desktop Agent Contract

## Scope

This contract applies under `desktop/`. Read it together with the repository root `AGENTS.md`.

## Module boundary

- `desktop/` is an isolated Go 1.25 module pinned to Wails v3. The root PairRoom module remains Go 1.23 and standard-library-only; never move Wails or GUI dependencies into the root `go.mod`.
- The desktop application is a native host, not a second PairRoom backend or frontend. Reuse the existing Management Shell, Room View, `internal/service`, Registry, Runtime Manager, provider configuration, locking, and native Agent adapters.
- Keep PairRoom lifecycle, authentication, numeric-loopback validation, `service.lock`, and graceful native-Turn draining in Go. Do not duplicate those contracts in JavaScript or platform-specific packaging scripts.
- Reusing an authenticated installed daemon never transfers ownership to the desktop app. An embedded Service is owned by the current desktop process and must shut down Management requests, drain Runtimes, and release its lock in that order.

## Wails and generated files

- Wails is pinned in `go.mod`; treat v3 API changes as explicit dependency upgrades requiring Windows, macOS, and Linux validation.
- `python scripts/prepare-build.py` generates `build/darwin/`, `build/linux/`, and `build/windows/` from the pinned Wails CLI. These directories, `bin/`, and `dist/` are generated and must remain untracked.
- `build/config.yml`, `build/Taskfile.yml`, `Taskfile.yml`, and the scripts under `scripts/` are maintained source.
- Keep the startup page minimal. Product UI changes belong to the existing embedded PairRoom Web assets, not `desktop/frontend/`.

## Verification

From `desktop/`, run at minimum:

```bash
go mod tidy
go test -count=1 ./...
python scripts/prepare-build.py
wails3 task build
```

The dedicated GitHub workflow is authoritative for Linux, Windows, macOS arm64, and macOS amd64 packaging. Do not claim signed or notarized production installers unless the corresponding platform signing steps actually ran.
