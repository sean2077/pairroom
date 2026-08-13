# PairRoom 1.0 Operations Guide

## 1. Stable deployment boundary

PairRoom 1.0 is a single-user local daemon for one Git repository. The recommended deployment is:

```text
Browser on the same machine
        │
127.0.0.1:7332
        │
PairRoom daemon
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

```bash
pairroom serve --repo /path/to/repository
```

For a deterministic local demonstration:

```bash
pairroom serve --repo /path/to/repository --mock
```

Use SIGINT/SIGTERM or the terminal interrupt command for graceful shutdown. A later start retains the transcript and native session/thread IDs, while unfinished processing and approvals are explicitly settled rather than displayed as permanently active.

## 4. Remote access

Prefer an SSH tunnel or VPN:

```bash
ssh -L 7332:127.0.0.1:7332 host-running-pairroom
```

Then open `http://127.0.0.1:7332` locally. When binding PairRoom to a non-loopback address, it automatically generates a Bearer bootstrap token when one is not configured. The browser exchanges the URL-fragment bootstrap token for a short-lived HttpOnly session. Network traffic remains plaintext unless protected by the tunnel or a trusted TLS proxy.

## 5. Room data

The default data directory is derived from the canonical repository path under the user configuration directory. It contains:

```text
events.jsonl            append-only room history
metadata.json           format and schema metadata
attachments/            immutable message images
runtime/                prompts and ephemeral runtime state
reviewer worktree       disposable isolated review snapshot
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

1. Stop PairRoom.
2. Run `verify`.
3. Create a backup.
4. Install the new binary.
5. Run `version --json` and `doctor`.
6. Open the room with `--auto-start=false` first.
7. Inspect history, roles, workspace boundaries, and pending state.
8. Start the Agents.

Do not alternate old and new binaries against the same data directory. Restore the pre-upgrade backup into a separate path for rollback.

## 9. Failure response

- **Browser says unauthorized:** reopen the complete startup URL to exchange a fresh browser session.
- **Agent appears stalled:** inspect Runtime events, then interrupt or restart only that participant.
- **Reviewer snapshot failed:** fix Git worktree, dirty patch, untracked symlink, or filesystem permission errors; PairRoom does not silently fall back to a live writable reviewer tree.
- **Integrity verification failed:** stop the daemon, preserve the entire directory, and restore the latest verified backup. Do not edit event sequence numbers by hand.
- **Vendor protocol changed:** update the official CLI, run `doctor`, and reproduce in a non-critical repository before filing a scrubbed report.
