# Storage and recovery

## Durable vs ephemeral

| Kind | Examples | After restart |
|---|---|---|
| Durable | Room metadata, Message, FIFO delivery / processing projection, collaboration instructions, permission profile, Turn summary, resolved approval, Binding, attachment metadata | Replayed from the Event Log / registry |
| User configuration | Named Agent pairs and default ID | Read from `agent-pair-profiles.json` under the Service data root, independently of Registry-index rebuild |
| Ephemeral | native process, current stdout connection, vendor request ID, active owner, transient text delta | Not restored |

Room-owned FIFO entries are persistent only while PairRoom can prove they did not cross the native submission boundary. Any input that may already have produced side effects without a confirmed ownership result is not executed again automatically.

## Event Log

A Room uses an append-only JSONL store. Metadata schema is checked before Event Log replay, then current-schema events are replayed in order to rebuild the projection. Readers accept only schema `12`; schema ≤11 is retired and fails before replay, repair, or mutation. New Rooms, including embedded Rooms, write schema 12 and managed Rooms persist provisioning schema 5 with explicit `host_mode`. Missing metadata is not inferred for a published Room. Mismatched pairs and illegal or retired-actor events fail explicitly instead of guessing a repair.

The schema source of truth is `internal/model/types.go`, the event write / apply code, and `internal/store/`, not a hand-written fictional schema file in the docs.

High-frequency transient telemetry may stay off disk so token-by-token fsync does not block the native stdout reader. State transitions that need audit must be durable.

## Restart

In embedded Rooms, after unexpected process exit or restart:

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

## Native relay state

Native bindings, publication receipts, observed publication gaps, inbox messages and failure categories are append-only Room events. The binding fact carries the official `session_id` captured at bind and stores only its credential hash plus public metadata; never raw relay secrets. Publication acceptance and optional enqueue share one append, so response loss cannot create an accepted-without-inbox gap. Queued work survives restart; any recovered `delivering` becomes `unknown`, which requires explicit Retry unless the original claimer's receipt-matched acknowledgement arrives while no Retry is pending. A ten-second acknowledgement lease bounds the live collector crash window. Terminal `handed_off` means stdout only.

The CLI's workspace footprint is `.pairroom/rooms/<room>/slots/<slot>/state.json`, `credentials` (0600 on POSIX), a `bootstrap` file only when an earlier version left one (current binds do not write it; unbind removes it if present), an empty `collector/` lock-anchor directory, and a disclosed `.pairroom/` line in `.gitignore`. `state.json` contains binding/generation/session identity, sequence watermarks, block count and one bounded pending `{seq,text,created_at,unknown}` publication. An unconfirmed bind uses one private `bind-attempt.json` containing the candidate state, credentials and replacement intent; it is excluded from active discovery. Failed requests leave committed files intact; same-session bind retry without --create/--replace reuses the original key and replacement intent; a new explicit --replace supersedes the attempt. Promotion writes credentials/state before removing the attempt and preserves any already-advanced publication state. Unbind also removes the attempt. `collector/` holds no owner, PID, queue or expiry record; the kernel lock on that directory is released when the collecting process dies. No per-message spool files or vendor transcript parsing are used. Private atomic-write temporary files are cleaned after normal errors or the next successful slot access; directory/named-mutex locking introduces no lock file. The Windows named collector mutex is scoped to one Windows login session, so collectors running in two concurrent login sessions of the same machine do not exclude each other. Windows uses its native user/file-access boundary; POSIX permission-bit checks are not claimed as Windows ACL enforcement.

A report sequence and its pending body are saved in one atomic write before HTTP publication. Confirmation clears pending and advances the confirmed watermark. On the next hook or `relay status`/`relay reconcile`, query the original key: accepted clears it; explicitly absent supplements the same sequence; unavailable or malformed evidence retains an unknown pending result. No new-ID auto-replay occurs. `reconcile --resend` and `--discard` are explicit decisions; discard preserves the consumed sequence so a later observed jump records a gap. There is no background client reaper: an aged pending record is reconciled at the next CLI/hook opportunity.

A crash before that atomic write leaves no local record and consumes no sequence. It may be undetectable even if later publications arrive, and must never create a fabricated delivery record. A missing-sequence gap is reported only from a real observed jump, not inferred from quiet time or missing native transcript text.

The Service's owner-only `relay-endpoint.json` is ephemeral endpoint discovery for local CLI clients, not a Room credential or a Registry field. It contains the current numeric-loopback URL and a scoped relay-setup token — sufficient for service discovery, Project registration, native Room creation, pair-default reads and the native binding lifecycle, never full Management authority or browser session bootstrap — and must not be exported. Clients re-read it to follow Service port/token changes. Registry checkpoints use strict schema 3 with canonical `slot1`/`slot2` keys and immutable host mode; native credential hashes and relay state never enter it. A retired checkpoint retires the whole Service root rather than being rewritten in place.

Native Room backups do not stop user-owned processes and do not include workspace slot credentials. Stop native work explicitly when consistency matters, preserve the workspace state separately, and inspect side effects before reconnecting restored bindings.

### Claude inbox capability (workspace-private)

`<workspace>/.pairroom/rooms/<room>/slots/<slot>/claude-inbox.json` contains the
local inbox address/token and exact native bind ID, generation and session ID.
It is captured only by confirmed Claude bind/Stop, atomically replaced, and
never serialized into the Room Store, Registry, exports or public API. Missing
or stale capability disables that wake attempt, not relay. Unix owner/mode
checks and Windows protected owner-only DACLs guard the file. Treat both it and
crash-left `.claude-inbox-*` temporaries as secrets; unbind removes the sidecar and slot cleanup removes stale
temporaries. See [Claude inbox wake](design/claude-inbox-wake.md).

## Native current-work and browser recovery projections

Current queued/unresolved ordinals, inbox counters, latest human-directed message
and wake observations are rebuilt from existing facts during replay. No Room schema
migration, history deletion or second durable queue is introduced. Wake reservations
remain the durable no-retry and rate-budget facts; future eligible times are derived.

A Native browser stores at most one unconfirmed immutable user publication per Room
under `pairroom.native.outbox.v1.<room>` in same-origin localStorage. Its schema, Room
ID and bounded payload are validated before use. It may contain private message text,
attachment IDs and review metadata, but no bearer, CSRF, relay credential, session ID
or Claude inbox token. Persistence must succeed before POST; quota/corruption prevents
publication rather than silently losing the recovery identity. Reload performs only
a receipt GET. Explicit same-ID retries preserve payload; confirmed matching receipts
clear the record. Explicit Forget removes only browser state, never queued work.
Storage is plaintext origin-local recovery, not encrypted archival or an execution
log. Clearing browser data loses recovery; inspect server history before resending.
