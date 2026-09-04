# PairRoom

**English** · [简体中文](README.zh-CN.md)

<p align="center">
  <img src="docs/images/pairroom-runtime-overview.png" alt="PairRoom Room View: two Agent slots collaborating turn by turn">
</p>

PairRoom is a local collaboration control plane for official Claude Code, Codex, and Grok Build harnesses. Each durable Room has two independent Agent slots. Either slot may select Claude Code, Codex, or Grok Build, and both slots may use the same runtime. PairRoom keeps each native CLI's sessions, tools, approvals, and sandbox; it only organizes the user, two Agents, and the project workspace into an observable, interruptible, and auditable collaboration.

Empty `provider`, `model`, `effort`, and `instructions` inherit the selected native CLI's user/global configuration. Add only explicit PairRoom overrides.

## Core model

PairRoom does not let two Agents wake each other concurrently like an IM group chat. Each Room has one **native Turn owner** at a time:

```text
user
  -> current Agent completes one native Turn
  -> reliable terminal boundary
  -> explicit @peer or HANDOFF + NEXT
  -> next Room FIFO item
```

This is not a mechanical A/B/A/B message rotation. The current Agent can run tools, update a plan, and accept steering inside one native Turn. The other Agent can start only after a reliable Turn-complete boundary.

Key properties:

- **Human authority**: the user can choose the target Agent, override later flow, approve, cancel, or stop;
- **Single owner**: two native runtimes never own execution at the same time, even when both slots use the same runtime;
- **Explicit handoff**: an Agent that addresses `@agent1`, `@agent2`, `@claude`, `@codex`, `@grok`, or `@peer` hands the reply to that peer. `@claude`/`@codex` remain Agent 1/Agent 2 slot aliases and also resolve to a unique matching runtime. When a human asks both Agents to interact, that address must be written; speaking only to the human does not start the other Agent. Without an explicit address, the reply must include both `HANDOFF` and `NEXT`. `@human`/`@user` returns the decision to the user;
- **Fail closed**: a process restart does not replay the in-memory FIFO, avoiding duplicate side effects;
- **Native harness first**: PairRoom does not rewrite the Claude Code, Codex, or Grok Build tool loops or permission models.

## Install

CLI (Linux / macOS / Git Bash):

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh | sh
```

Windows PowerShell:

```powershell
$tag = (Invoke-RestMethod https://api.github.com/repos/sean2077/pairroom/releases/latest).tag_name
curl.exe -fsSL -o pairroom.exe "https://github.com/sean2077/pairroom/releases/download/$tag/pairroom-cli-$tag-windows-amd64.exe"
```

Release assets are named by prefix: `pairroom-cli-vX.Y.Z-…` is the command line, `pairroom-desktop-vX.Y.Z-…` is the desktop package. Windows desktop uses `-setup.exe` (distinct from the CLI `.exe`); Linux uses `.deb` / `.AppImage`, and macOS uses `.app.zip`.

## Quick start

Stop leftover daemon ownership and open the current-tree Management Shell:

```bash
make dev
```

After the Management Shell opens:

1. Register a local Git Project;
2. Create a Room;
3. Choose Driver / Reviewer;
4. Send a single-Agent task, or describe a sequential workflow;
5. Watch Turn, tool activity, approvals, delivery, and error state in Room View.

Before using a real runtime, confirm that each selected CLI (`claude`, `codex`, and/or `grok`) is installed, signed in, and can work independently in the target repository. Empty provider/model/effort/instructions inherit that CLI's global config. Full steps are in [Getting Started](docs/GETTING_STARTED.md).

## Desktop

`desktop/` provides a native Windows, macOS, and Linux entry built with **Wails v3**. It is not a second PairRoom backend or frontend: the desktop host reuses the existing Management Shell, Room View, Service Registry, Runtime Manager, configuration, locks, and native Agent adapters.

On startup, the desktop host:

1. Validates and reuses an explicitly supplied authenticated numeric-loopback Management URL;
2. Discovers an installed `pairroom daemon` and, if it is stopped, starts it and waits for an authenticated Management URL;
3. Starts a PairRoom Service in the current desktop process only when no daemon is installed. If a daemon is installed but unreachable, it fails closed and does not start a second Service.

Closing the main window only hides to the system tray and does not interrupt an active Agent. Explicit quit shuts down only an embedded Service owned by the desktop host, draining along the existing native-Turn boundary; an external daemon is unaffected. Build, dependency, and package notes are in [PairRoom Desktop](desktop/README.md). Browser and CLI entry points remain fully available.

## Documentation

- [Documentation map](docs/README.md)
- [Core concepts and relay semantics](docs/CONCEPTS.md)
- [Configuration and providers](docs/CONFIGURATION.md)
- [CLI reference](docs/CLI_REFERENCE.md)
- [Architecture and invariants](docs/ARCHITECTURE.md)
- [Operations, backup, and restore](docs/OPERATIONS.md)
- [Upgrading](docs/UPGRADING.md)
- [Contributing](CONTRIBUTING.md)

## Development verification

Root module:

```bash
make docs-check
make check
make smoke
```

Desktop module:

```bash
make desktop-build
make desktop-package
```

`make desktop-build` builds the current-platform desktop host and bundled `pairroom` CLI. `make desktop-package` builds the current-platform production installer or app bundle (Windows NSIS, including `PairRoom.exe` and `bin\pairroom.exe`). Artifacts land in `desktop/bin/`. Desktop module tests still run from `desktop/`: `cd desktop && go test -count=1 ./...`.

`docs-check` verifies documentation links, source paths, CLI flags, HTTP routes, and JSON configuration fields so docs cannot silently drift as the code evolves. The desktop module uses a separate Go toolchain and dependency lock and does not change the root module's standard-library-only boundary.

## Status and boundaries

PairRoom is still evolving quickly. Breaking changes to the CLI, HTTP API, Event Log, and Agent protocol are recorded in [CHANGELOG](CHANGELOG.md) and [Upgrading](docs/UPGRADING.md). Current Mock E2E can verify scheduling, persistence, and recovery, but it does not replace real Claude Code / Codex / Grok Build native E2E. Unsigned desktop CI packages also do not replace production signing and macOS notarization.

## License

[MIT](LICENSE)
