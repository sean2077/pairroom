# Storage and recovery

## Durable vs ephemeral

| Kind | Examples | After restart |
|---|---|---|
| Durable | Room metadata, message, delivery / processing projection, role, Workflow, Turn summary, resolved approval, Binding, attachment metadata | Replayed from the Event Log / registry |
| Ephemeral | native process, current stdout connection, vendor request ID, active owner, Room FIFO, transient text delta | Not restored |

PairRoom's durable transcript is not a persistent task queue. Any input that may already have produced side effects without a confirmed terminal is not executed again automatically after restart.

## Event Log

A Room uses an append-only JSONL store. On startup, events are replayed in order to rebuild the projection. Illegal events or unsupported routing state should fail explicitly instead of guessing a repair.

The schema source of truth is `internal/model/types.go`, the event write / apply code, and `internal/store/`, not a hand-written fictional schema file in the docs.

High-frequency transient telemetry may stay off disk so token-by-token fsync does not block the native stdout reader. State transitions that need audit must be durable.

## Restart

After unexpected process exit or restart:

1. native process state is reset;
2. pending delivery / processing is settled into a fail-closed state;
3. connection-local pending approvals expire;
4. the Room FIFO is not rebuilt automatically;
5. the user inspects the workspace and Event Log, then creates a new Retry message.

Retry must generate a new auditable message ID. Reusing an old ID makes late vendor events ambiguous.

## Attachment

The Event Log stores only verified presentation metadata and an opaque attachment ID. Absolute host paths are resolved only at the adapter boundary and do not enter the API transcript. Attachments have count, type, and total-size limits.

## Backup and Restore

Stop or archive the related Room before backup, so “the files were copied” is not mistaken for “external side effects completed”. Restore should verify:

- manifest / checksum;
- that the Project path still exists;
- that the Binding's native session can be resumed;
- that the Event Log can replay completely;
- that old routing / schema is supported by the current release.

Restoring a backup does not start unfinished old FIFO items.

## Corruption handling

Do not edit production JSONL directly. Copy the data directory first, keep the original failure evidence, then use diagnostics / backup verification to locate the first invalid event. A Room that cannot be migrated safely should be rebuilt, not continued after skipping middle events.
