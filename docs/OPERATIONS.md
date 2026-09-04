# Operations

## Runtime shapes

- PairRoom Desktop: Wails v3 native Window / Tray host; reuses a daemon or starts the Service in-process;
- `pairroom service`: the normal multi-Project / multi-Room management entry;
- `pairroom serve`: single-repository compatibility entry;
- `pairroom daemon`: manage the Service as a local background process;
- `--mock`: deterministic verification mode that does not start vendor CLIs.

From a source checkout, `make dev` stops an installed daemon, recovers a crash-stale lock only after the recorded PID is gone, starts the current-tree Management Service, and opens the Management Shell. `make stop` is the stop-only helper. Do not leave `make dev` running at the same time as a started daemon.

All built-in entries default to numeric loopback. Before exposing any other interface, configure a token and assess the risk of local repositories, Agent credentials, and attachments.

## Desktop lifecycle

On startup the desktop host chooses a single Service owner in this order:

1. Validate that `PAIRROOM_DESKTOP_URL` points at an authenticated numeric-loopback PairRoom Service;
2. Discover an installed daemon; if it is stopped, the desktop host starts it and waits for the current authenticated Management URL;
3. If there is no daemon, but a bundled `pairroom` CLI sits next to the desktop host, run `pairroom daemon install` with it and then connect;
4. Start an embedded Service in the desktop process only when there is neither a daemon nor a bundled CLI. If a daemon is installed but unreachable, stay fail closed.

Behavior boundaries:

- Close the main window: hide to the tray; do not stop Runtime or Agents;
- Launch the app again: the single-instance handler focuses the existing window;
- Quit while using an external daemon: exit only the GUI; the daemon and active Turns keep running;
- Windows daemon: Service logs go to the rotating log file and do not keep a taskbar console; use `pairroom daemon logs` to inspect output;
- Quit while using an embedded Service: stop accepting Management requests, wait for Runtimes to drain at a native-Turn boundary, then release the Registry and `service.lock`;
- stale lock: the desktop host stays fail closed and does not recover implicitly.

If desktop startup reports a `service.lock` conflict, run `pairroom daemon status` first. Only after status is stopped and the PID recorded in the lock no longer exists, run `pairroom daemon start --recover-stale-lock`. The desktop host will not delete the lock or seize the data root for the user.

Desktop packages are built only for `v*` release tags (or manual `workflow_dispatch`) and attached to the same GitHub Release: CLI assets are `pairroom-cli-vX.Y.Z-…`, desktop assets are `pairroom-desktop-vX.Y.Z-…` (Windows `-setup.exe`, Linux `.deb`/`.AppImage`, macOS `.app.zip`). Pull requests and `main` only run desktop module verification. These packages remain unsigned by default. Windows code signing and Apple Developer ID signing / notarization can be claimed only after they actually run in the production release environment.

## Daily checks

Observe four layers of state, not only chat text:

1. whether the Service / Room runtime is active;
2. participant state and native session binding;
3. message delivery / processing;
4. Turn summary, tool activity, approval, and system notice.

“No Runtime event for a while” is only a reminder. Long commands, compacted context, or unexposed vendor steps can be silent temporarily. An ordinary diagnostic error is not necessarily a terminal boundary.

## Project, Archive, Delete

- **Unregister Project**: remove the Management Service registration; do not delete the user's Git repository. Handle Rooms that still belong to the Project first;
- **Archive Room**: stop the current Agent Turn and suspend the Runtime, keeping Room data for audit or restore;
- **Permanent delete**: delete PairRoom-managed data; confirm archive, backup, Binding, and active-runtime preconditions first;
- **Deleting the repository** is never an implied side effect of PairRoom Project unregister / Room delete.

Available actions are decided by the preconditions returned by the current UI / CLI / API. Automation must not ignore conflict responses.

## Capacity and idle reclaim

The Service can limit the number of concurrently active Rooms and reclaim Runtimes by idle policy. Reclaim only stops processes and frees resources; it does not delete a durable Room. The next activation rebuilds the adapter, but the Room FIFO does not survive across processes.

## Backup

Create and verify a backup before:

- upgrading across a breaking release;
- permanently deleting a Room;
- moving the PairRoom data root;
- manually repairing an Event Log;
- changing session / Binding policy.

Backup success is defined by manifest / checksum verification, not by a compression command's exit code alone.

## Graceful shutdown

A normal exit should stop active adapters, settle in-flight projections, close the store, and clean reviewer workspaces. After a forced kill, the next start fails closed; it does not guess whether the previous native operation completed.

An embedded Service used by the desktop host follows the same shutdown contract. The Wails window lifecycle cannot bypass Room Runtime drain.

## Logs and diagnostics

Logs must not contain API keys, Authorization headers, or absolute attachment paths. When reporting a problem, include:

- PairRoom build version;
- OS, entry type (Desktop / daemon / foreground), and versions of the selected native CLIs;
- Room / message / Turn ID;
- related system notices and terminal events;
- redacted configuration;
- a minimal Mock or read-only reproduction.

Common handling is in [Troubleshooting](TROUBLESHOOTING.md).
