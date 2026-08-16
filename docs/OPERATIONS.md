# PairRoom Operations Guide

## 1. Stable deployment boundary

PairRoom is a single-user local coordination service. The recommended deployment uses the multi-Project/multi-Room control plane:

```text
Browser on the same machine
        │
127.0.0.1:7332
        │
PairRoom daemon
        ├── Project / Room runtimes
        ├── official claude CLI
        └── official codex app-server
```

Do not expose the daemon directly to the public internet. PairRoom has authentication and CSRF controls but does not provide TLS, account management, tenant isolation, or remote administration.

## 2. Preflight

```bash
pairroom version --json
pairroom doctor --repo /path/to/repository
```

Complete official Claude Code and Codex login in their own CLIs before starting PairRoom. `doctor` probes executable and protocol availability but does not create a model turn or inspect repository contents.

## 3. Start and stop

Run the control plane in the foreground:

```bash
pairroom service
```

Install the same control plane as an operating-system-managed background service:

```bash
pairroom daemon install --runtime-limit 4 --idle-timeout 20m
pairroom daemon status
pairroom daemon logs -f
pairroom daemon stop
pairroom daemon start
```

Linux uses systemd, macOS uses launchd, and Windows uses the current user's Task Scheduler. `daemon install` forwards Service options, stores absolute paths, captures PATH and proxy variables, disables browser launch, and combines stdout/stderr in the configured log. Logs rotate at 10 MiB with three backups by default; use `--log-max-size` and `--log-max-backups` to change the policy. Use `--force` only to replace an existing service definition.

For a deterministic local demonstration, either run the foreground Service or install it with `--mock`:

```bash
pairroom service --mock
pairroom daemon install --mock
```

Use SIGINT/SIGTERM for a foreground process or `pairroom daemon stop` for an installed service. Both paths stop accepting management work, wait for active Turns, close Room runtimes, and release `service.lock`. If a crash leaves the lock behind, verify that the old process is gone before running `pairroom daemon start --recover-stale-lock`; recovery is never automatic.

## 4. Remote access

PairRoom rejects wildcard, LAN, public, and hostname listener addresses. Forward a local port over SSH to the server-side loopback listener:

```bash
ssh -L 7332:127.0.0.1:7332 host-running-pairroom
```

Then open `http://127.0.0.1:7332` locally. Do not change PairRoom's listener to `0.0.0.0`, a LAN address, or a hostname; both `service` and `serve` reject those values. The browser exchanges any URL-fragment bootstrap token for a short-lived HttpOnly session, while SSH protects the remote transport.

## 5. Service and Room data

The default Service data root is the operating-system user configuration directory under `pairroom`. It contains the rebuildable Service registry plus one durable directory per Room:

```text
service.lock                 exclusive Service owner
service-registry.json        rebuildable Project/Room index
daemon.json                 non-secret daemon metadata
logs/service.log            combined daemon output
rooms/<room-id>/events.jsonl append-only Room history
rooms/<room-id>/attachments/ immutable message images
rooms/<room-id>/runtime/     prompts and ephemeral runtime state
```

Treat the whole room directory as sensitive project data.

## 6. Integrity, backup, and restore

Verify a stopped or live room:

```bash
pairroom verify --data-dir /path/to/room-data --json
```

Create a self-verifying backup:

```bash
pairroom backup \
  --data-dir /path/to/room-data \
  --output pairroom-room-backup.tar.gz
```

Restore into a new directory first:

```bash
pairroom restore \
  --input pairroom-room-backup.tar.gz \
  --data-dir /path/to/restored-room
pairroom verify --data-dir /path/to/restored-room
```

The restore path rejects traversal, links, duplicates, undeclared files, excessive sizes, and SHA-256 mismatches before replacing a destination.

## 7. Diagnostics

```bash
pairroom diagnostics \
  --data-dir /path/to/room-data \
  --output pairroom-diagnostics.tar.gz
```

Diagnostics contain structure, counts, event headers, build/platform information, and integrity results. They intentionally omit conversation text and image bytes. Inspect the archive before sharing it.

## 8. Upgrade and rollback

1. Stop PairRoom with `pairroom daemon stop` or the foreground terminal interrupt.
2. Run `verify`.
3. Create a backup.
4. Install the new binary.
5. Run `version --json` and `doctor`.
6. Open the room with `--auto-start=false` first.
7. Inspect history, roles, workspace boundaries, and pending state.
8. Start the Agents, then restart the installed service with `pairroom daemon start` when applicable.

Do not alternate old and new binaries against the same data directory. Restore the pre-upgrade backup into a separate path for rollback.

## 9. Failure response

- **Browser says unauthorized:** reopen the complete startup URL to exchange a fresh browser session.
- **Agent appears stalled:** inspect Runtime events, then interrupt or restart only that participant.
- **Reviewer snapshot failed:** fix Git worktree, dirty patch, untracked symlink, or filesystem permission errors; PairRoom does not silently fall back to a live writable reviewer tree.
- **Integrity verification failed:** stop the daemon, preserve the entire directory, and restore the latest verified backup. Do not edit event sequence numbers by hand.
- **Vendor protocol changed:** update the official CLI, run `doctor`, and reproduce in a non-critical repository before filing a scrubbed report.
