# Operations

## Runtime shapes

- PairRoom Desktop: Wails v3 native Window / Tray host; reuses a daemon or starts the Service in-process;
- `pairroom service`: the normal multi-Project / multi-Room management entry;
- `pairroom serve`: single-repository compatibility entry;
- `pairroom daemon`: manage the Service as a local background process;
- `--mock`: deterministic verification mode that does not start vendor CLIs.

From a source checkout, `make dev` stops an installed daemon, recovers a crash-stale lock only after the recorded PID is gone, starts the current-tree Management Service, and opens the Management Shell. `make stop` is the stop-only helper. Do not leave `make dev` running at the same time as a started daemon.

All built-in listeners require numeric loopback addresses; a token does not enable LAN, wildcard, or hostname binds. For remote access, use SSH local port forwarding and keep the Service bound to loopback. Protect the forwarded endpoint and bootstrap token as access to local repositories, Agent credentials, and attachments.

## Desktop lifecycle

On startup the desktop host chooses a single Service owner in this order:

1. Validate that `PAIRROOM_DESKTOP_URL` points at an authenticated numeric-loopback PairRoom Service;
2. Discover an installed daemon; recover a crash-stale lock after the recorded PID is gone, start or restart the daemon when needed, and wait for the current authenticated Management URL;
3. If no daemon is installed, own an embedded Service in the desktop process. A bundled CLI does not authorize daemon installation. If a daemon is installed but unreachable, stay fail closed.

Explicit data-root/configuration or Mock options select their own embedded Service instead of discovering an unrelated default daemon; an explicit validated Management URL remains the strongest override when external discovery is enabled. Settings → Desktop → Launch at login changes only native login registration. It does not install a daemon. `make desktop-update` replaces the host and bundled CLI without changing login registration or user data.

Behavior boundaries:

- Close the main window: hide to the tray; do not stop Runtime or Agents;
- Launch the app again: the single-instance handler focuses the existing window;
- Quit while using an external daemon: exit only the GUI; the daemon and active Turns keep running;
- Windows daemon: Service logs go to the rotating log file and do not keep a taskbar console; use `pairroom daemon logs` to inspect output;
- Quit while using an embedded Service: stop accepting Management requests, wait for Runtimes to drain at a native-Turn boundary, then release the Registry and `service.lock`;
- crash-stale lock: recover after the recorded PID is gone, then start or restart the installed daemon; a live owner still fails closed;

Desktop startup recovers a crash-stale `service.lock` after confirming the recorded PID is gone, then starts or restarts the installed daemon. A live lock owner still fails closed: run `pairroom daemon status`, then `pairroom daemon stop` and wait for graceful drain if that process is the installed daemon. The desktop host will not start a competing embedded Service or kill a live owner.

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

The Service can limit the number of concurrently active Rooms and reclaim Runtimes by idle policy. Reclaim only stops processes and frees resources; it does not delete a durable Room. The next activation rebuilds the adapter and restores Room-owned FIFO entries that never crossed native submission; accepted or uncertain native work is left for explicit inspection and Retry.

## Backup

Create and verify a backup before:

- upgrading across a breaking release;
- permanently deleting a Room;
- moving the PairRoom data root;
- manually repairing an Event Log;
- changing session / Binding policy.

`pairroom backup` and `pairroom restore` operate on **one Room data directory**, not the multi-Room Service root. For a complete Service rollback, stop/drain the Service and separately preserve its entire data root with an offline filesystem backup; also preserve any explicitly imported Room directories outside that root. Native CLI session stores and the user's Git repository are separate and are not included in a Room archive.

Write backup and diagnostics bundles **outside the source Room data directory**. In-place outputs and symlink aliases into that directory are rejected to prevent replacing Event Logs or attachment data. Restore verifies the file set, hashes, and complete gzip trailer before publishing the target. Backup success is defined by verification, not by a compression command's exit code alone.

## Graceful shutdown

A normal exit should stop active adapters, settle in-flight projections, close the store, and clean any legacy reviewer workspaces. After a forced kill, the next start restores only Room-owned FIFO entries that were still before native submission and fails uncertain submission windows closed; it never guesses whether an accepted native operation completed.

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
