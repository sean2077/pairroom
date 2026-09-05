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

### From source (full Service)

```bash
make dev
```

This stops any leftover daemon, recovers a crash-stale `service.lock` only after the recorded PID is gone, starts the current-tree Management Service, and opens the Management Shell in a browser. Equivalent commands:

```bash
go run ./cmd/pairroom daemon stop
go run ./cmd/pairroom service --recover-stale-lock
```

`make stop` is the stop-only helper (`daemon stop` fails if nothing is installed; `make stop` treats that as clean). Legacy single-Room `make run` / `make demo` still call `pairroom serve`.

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

### CLI + browser (Mock)

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
3. For Agent 1 and Agent 2, select a Runtime, native or supported CC Switch Profile, and optionally edit the Model and advanced Runtime policy. Unavailable Runtimes and Profiles that require OAuth, proxy conversion, or failover remain visible but disabled;
4. Confirm both Bindings (JSON keys `claude` / `codex`). Agent 1 starts as Driver and Agent 2 as Reviewer; both slots may use the same Runtime/Profile;
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

The Driver must include the other participant's exact displayed handle in its reply. Introducing itself only to the human, with no peer handle, does not start the other Agent. If the reply names both `@user` and the peer, the peer handle wins. With unique Claude and Codex runtimes those handles are `@claude` and `@codex`. PairRoom hands the complete reply and attachments to the peer only after the current Turn ends. If the peer then answers without naming the Driver, the greeting ends naturally after two Turns.

For a review, assign Driver and Reviewer directly, then ask the Driver to request independent review only when it has something concrete to inspect:

```text
Implement and verify the change. When the patch is ready, ask the displayed Reviewer handle for an independent review.
```

PairRoom does not compile or approve actor/action stage sequences. Each Agent may finish the user's request; another Turn exists only after an exact Agent handle or a new user Message.

## 5. Steering, queue, and cancel

- `steer` is the default. Same-target input attempts native same-Turn steering; unavailable or rejected steering falls back to the Room FIFO exactly once, while an unknown result requires explicit Retry;
- `queue` always waits in the Room FIFO while a Turn is active and starts immediately when the Room is idle;
- input to the other Agent always waits for the active Turn boundary;
- only the other participant's exact current `mention_handle` in an Agent reply starts another Agent Turn; no mention ends relay; an Agent handle wins over `@user` in the same reply;
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

Then start the real Management Service with `make dev` (not `--mock`) and select the Runtime, ProviderRef, Model, effort, instructions, and permission policy while creating the Room. The complete two-slot selection is immutable after creation. `source: native` and empty overrides inherit each selected CLI's user/global configuration. A CC Switch Profile is re-read at validation and activation; PairRoom never changes CC Switch current state or replaces CLI/Provider credential management. Run `pairroom providers` to inspect the sanitized local catalog.

## 7. End correctly

- Pause for now: leave the Room; the Runtime may be reclaimed by idle policy;
- Close the desktop main window: hide to tray; active Turns keep running;
- Quit the desktop app: an embedded Service shuts down in Management → Runtime drain → lock release order; an external daemon keeps running;
- Stage complete: archive the Room; archive stops the current Agent Turn first;
- No longer needed: follow the UI / API permanent-delete flow, and keep a backup first.

Next, read [Configuration](CONFIGURATION.md) and [Operations](OPERATIONS.md).
