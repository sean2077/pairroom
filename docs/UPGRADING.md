# PairRoom 1.0 Upgrade and Rollback

## Supported upgrade path

PairRoom 1.0 can open room data created by the retained v0.1-v0.9 history. Store migration is replay-based: old events remain append-only, while missing fields receive explicit defaults and the metadata schema advances to the current supported version.

Do not manually edit `metadata.json`, event sequence numbers, attachment manifests, or Turn summaries.

## Before upgrading

1. Stop PairRoom.
2. Record the current binary version.
3. Verify the room:

```bash
pairroom verify --data-dir /path/to/room-data --json
```

4. Create a verified backup:

```bash
pairroom backup \
  --data-dir /path/to/room-data \
  --output pairroom-before-1.0.tar.gz
```

5. Keep the backup outside the repository and room data directory.

## First 1.0 start

```bash
pairroom version --json
pairroom doctor --repo /path/to/repository
pairroom serve \
  --repo /path/to/repository \
  --data-dir /path/to/room-data \
  --auto-start=false
```

Inspect before starting either Agent:

- complete transcript and pagination boundary;
- Driver/Reviewer roles;
- Reviewer workspace kind, source HEAD, dirty flag, snapshot hash, and read-only strength;
- no stale `working`, `waiting`, or pending approval state from the previous process;
- uploaded images and Agent-generated image previews;
- Turn summaries and message correlation;
- browser authentication behavior when using a non-loopback listener.

## Behavior changes since v0.3

- Reviewer no longer defaults to the live Driver tree. PairRoom creates a Git snapshot containing HEAD, dirty tracked changes, and untracked regular files. Unsafe snapshot creation fails instead of silently degrading.
- User messages can explicitly append, wait for the next Turn, or supersede an earlier instruction. Per-target cancellation and retry remain auditable.
- Work Inspector summaries persist across restart.
- `verify`, `backup`, `restore`, and `diagnostics` are available.
- Long conversations load a bounded newest window and fetch older messages with a cursor.
- Browser Token query parameters/Web Storage were removed. A fragment bootstrap now exchanges for an HttpOnly session; browser mutations require CSRF.

## Data directory

Persistent content:

```text
events.jsonl
metadata.json
attachments/
```

Recreatable or excluded from backup:

```text
runtime/
reviewer worktree
browser sessions
temporary uploads
lock/cache files
```

## Rollback

Do not run an older binary against a data directory already written by 1.0.

1. Stop 1.0.
2. Preserve the 1.0 room directory for diagnostics.
3. Restore the pre-upgrade backup into a different directory.
4. Point the old binary at that restored directory.
5. Verify it before starting Agents.

A future-schema rejection is intentional; lowering the schema number does not remove new events and can create an invalid projection.
