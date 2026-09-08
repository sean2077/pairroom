# Storage and recovery

## Durable vs ephemeral

| Kind | Examples | After restart |
|---|---|---|
| Durable | Room metadata, Message, FIFO delivery / processing projection, collaboration instructions, permission profile (legacy role), Turn summary, resolved approval, Binding, attachment metadata | Replayed from the Event Log / registry |
| User configuration | Named Agent pairs and default ID | Read from `agent-pair-profiles.json` under the Service data root, independently of Registry-index rebuild |
| Ephemeral | native process, current stdout connection, vendor request ID, active owner, transient text delta | Not restored |

Room-owned FIFO entries are persistent only while PairRoom can prove they did not cross the native submission boundary. Any input that may already have produced side effects without a confirmed ownership result is not executed again automatically.

## Event Log

A Room uses an append-only JSONL store. Metadata schema is checked before Event Log replay, then current-schema events are replayed in order to rebuild the projection. New stores use schema `10`; schema `9` remains readable without rewriting its metadata or converting its legacy policy. Other schemas fail before replay/repair. A modern Room persists its collaboration instructions with `room.created` and, when managed, matching provisioning schema 3. Illegal events fail explicitly instead of guessing a repair.

The schema source of truth is `internal/model/types.go`, the event write / apply code, and `internal/store/`, not a hand-written fictional schema file in the docs.

High-frequency transient telemetry may stay off disk so token-by-token fsync does not block the native stdout reader. State transitions that need audit must be durable.

## Restart

After unexpected process exit or restart:

1. native process state is reset;
2. `pending` / `queued` delivery is rebuilt into the Room FIFO in Event Log order;
3. `submitting` delivery fails with explicit Retry guidance because native ownership is unknown;
4. unfinished input already marked `started` / `injected` is cancelled without replay;
5. connection-local pending approvals expire;
6. the user inspects the workspace and Event Log before retrying uncertain or accepted work.

Retry must generate a new auditable message ID. Reusing an old ID makes late vendor events ambiguous.

## Attachment

The Event Log stores only verified presentation metadata and an opaque attachment ID. Absolute host paths are resolved only at the adapter boundary and do not enter the API transcript. Attachments have count, type, and total-size limits.

## Backup and Restore

Stop or archive the related Room before backup, so “the files were copied” is not mistaken for “external side effects completed”. Restore should verify:

- manifest / checksum;
- that the Project path still exists;
- that the Binding's native session can be resumed;
- that the Event Log can replay completely;
- that the Room schema is exactly supported by the current release.

Restoring a current-schema backup may restart Room-owned FIFO entries that never crossed the native submission boundary. Accepted or uncertain native work is never replayed automatically.

Agent pair profiles are not part of a Room backup or restore. Preserve the Service's `agent-pair-profiles.json` separately when migrating its user configuration. Writes use a private temporary file, file sync, rename, and directory sync where supported. Reads reject non-regular/symlinked files, oversized data, unknown schemas/fields, invalid pairs, and dangling defaults; profile operations fail closed without replacing the damaged file. Copy it before repair. Explicit full Room selections remain independent of profile-file health.

## Corruption handling

Do not edit production JSONL directly. Copy the data directory first, keep the original failure evidence, then use diagnostics / backup verification to locate the first invalid event. A Room that cannot be migrated safely should be rebuilt, not continued after skipping middle events.
