# Getting started

This guide covers only “from zero to the first collaboration Turn”. Concepts, full configuration, and operations live in other documents.

## 1. Prerequisites

- a local Git repository;
- for CLI / browser mode, a Go toolchain matching the root `go.mod`;
- when building the desktop host from source, the Wails v3 toolchain and platform dependencies listed in `desktop/README.md`;
- for real mode, independently runnable and signed-in native CLIs for the runtimes you select (`claude`, `codex`, and/or `grok`);
- a browser or PairRoom Desktop that can reach the local loopback Service.

First-time use should start in Mock mode. It verifies PairRoom scheduling, UI, Event Log, and recovery without consuming model quota.

## 2. Choose a launch entry

### CLI install

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh | sh
pairroom service --mock
```

On Windows, download `pairroom-cli-vX.Y.Z-windows-amd64.exe`. The desktop installer is `pairroom-desktop-vX.Y.Z-windows-amd64-setup.exe`.

### PairRoom Desktop

Install the desktop package for your platform and start PairRoom. The desktop host first validates and reuses an explicit Management URL. If it finds an installed daemon, it starts or connects to it. The package includes the `pairroom` CLI: if no daemon exists, the desktop host installs and connects with that CLI instead of leaving a `PairRoom.exe` with no `pairroom`. If a daemon is installed but unreachable, the desktop host stops and shows repair guidance; it does not start a second Service. Source/test entry points without a bundled CLI can still start an embedded Service when no daemon is installed.

Build from source:

```bash
make desktop-build
make desktop-package
```

Both targets run for the current host platform. Packaged artifacts write to `desktop/bin/`. To run desktop module tests alone: `cd desktop && go test -count=1 ./...`.

The desktop main window still loads the existing Management Shell. There is no separate desktop business state. Closing the window only hides to the tray; **Quit PairRoom** from the tray exits the application.

### CLI + browser

```bash
go run ./cmd/pairroom service --mock
```

PairRoom listens on loopback by default. If a browser does not open automatically, the terminal prints the Management Shell address. Exact options come from the command itself:

```bash
go run ./cmd/pairroom service --help
```

Both entries share the same Project, Room, Binding, Event Log, Runtime, and authentication semantics.

## 3. Create a Project and Room

In the Management Shell:

1. Register the target repository as a Project;
2. Create a Room;
3. Confirm Agent 1 and Agent 2 Bindings (JSON keys `claude` / `codex`; each slot may run Claude Code, Codex, or Grok Build);
4. Assign Driver / Reviewer, or leave both as Peer;
5. Open the Room from the sidebar; it becomes an in-app tab. Use **Open in browser** for a separate browser window.

A Project is a repository-level management record. A Room is a long-lived collaboration context. Unregistering a Project does not delete the repository, and archiving a Room does not permanently delete Room data.

## 4. Complete the first Turn

Start with one Agent and a small verifiable task, for example:

```text
Read the current repository and describe the test entry points. Do not modify files.
```

The Room should show, in order:

```text
message accepted
  -> native Turn started
  -> tool / text / approval events
  -> native Turn completed
  -> Room owner released
```

An unaddressed message goes only to the current Driver. To verify that both Agents can collaborate in sequence, you can say:

```text
Greet each other and introduce yourselves.
```

The Driver must `@codex` or `@claude` in the reply; introducing itself only to the human does not start the other Agent. PairRoom hands the reply to the peer only after the current Turn ends.

For staged collaboration, describe roles and actions directly:

```text
Claude plans first; Codex reviews the plan; after I approve, Codex implements; then Claude audits.
```

PairRoom compiles the stages into a sequential Workflow instead of letting two runtimes free-chat.

## 5. Steering, next Turn, and cancel

- Ordinary input to the current owner can take the current Turn's steering path;
- Explicit `next_turn` or input to the other Agent enters the Room FIFO;
- An explicit `@claude`, `@codex`, or `@peer` in an Agent reply is delivered to that peer after the current Turn ends; `@human`/`@user` leaves the decision with the user;
- Cancelling a message still in the FIFO removes only that message;
- Input already submitted to a native runtime may require interrupting the whole current native Turn.

See [Concepts](CONCEPTS.md) for the full semantics.

## 6. Switch to real Agents

First verify the selected native CLIs in the target repository:

```bash
claude --version
codex --version
grok --version
```

Then drop `--mock` and, if needed, set model, Provider, permission, and sandbox in configuration or the UI. Empty `provider`, `model`, `effort`, and `instructions` inherit each selected native CLI's user/global configuration. PairRoom does not replace CLI login or credential management.

## 7. End correctly

- Pause for now: leave the Room; the Runtime may be reclaimed by idle policy;
- Close the desktop main window: hide to tray; active Turns keep running;
- Quit the desktop app: an embedded Service shuts down in Management → Runtime drain → lock release order; an external daemon keeps running;
- Stage complete: archive the Room; archive stops the current Agent Turn first;
- No longer needed: follow the UI / API permanent-delete flow, and keep a backup first.

Next, read [Configuration](CONFIGURATION.md) and [Operations](OPERATIONS.md).
