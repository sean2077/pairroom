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

Embedded Turn summaries are checkpointed projections. Each `turn.summary.updated` record is a complete summary: creation, native turn start, final text, errors, input failure/cancellation and Turn completion are recorded immediately; tool, plan, diff, usage and command-output updates stay in memory, reach the browser as sequence-zero events, and are recorded at most once per 30 seconds per in-progress Turn, when that participant's adapter stops, or when the Room closes. A crash can therefore lose up to the last interval of an unfinished Turn's summary items; the raw `runtime.event` facts are recorded before projection and remain authoritative. A tool item does not copy its evidence: it keeps a 512-byte preview and `source_seqs`, the sequences of the durable `runtime.event` records that carry its full text and payload, which the inspector reads on demand. The store keeps an in-memory sequence-to-offset index, rebuilt by the open-time repair scan and extended by each append, so such a read seeks to one record. Command output is transient and not recorded separately, so command items keep their bounded output inline, as do tool items whose source append failed. A summary is also bounded to 256 KiB of encoded JSON: plan, diff, final text and error keep their newest whole-rune tails within a shared encoded budget (JSON escaping of markup can otherwise expand them up to sixfold), oversized usage is omitted, and then the oldest inline item payloads, details and finally items are shed; the raw `runtime.event` facts keep the complete values. Logs written with earlier summaries (evidence inline, no `source_seqs`) replay unchanged and are never rewritten; no schema change is involved. An older binary ignores `source_seqs` and shows only the preview for such items.

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

The CLI's workspace footprint is `.pairroom/rooms/<room>/slots/<slot>/state.json`, `credentials` (0600 on POSIX), a `bootstrap` file only when an earlier version left one (current binds do not write it; unbind removes it if present), an empty `collector/` lock-anchor directory, and a disclosed `.pairroom/` line in `.gitignore`. `state.json` contains binding/generation/session identity, sequence watermarks, block count, the last confirmed transcript reference (resynchronized from the Service by an idempotent `bind`), a local last-hook timestamp and a bounded publication backlog: at most one pending `{seq,text,at,unknown}` head plus up to seven `held` replies behind it with the next consecutive sequences. An unconfirmed bind uses one private `bind-attempt.json` containing the candidate state, credentials and replacement intent; it is excluded from active discovery. Failed requests leave committed files intact; same-session bind retry without --create/--replace reuses the original key and replacement intent; a new explicit --replace supersedes the attempt. A replacement starts fresh publication state, so it is refused before any Service call while a pending head or held reply exists; settle them with `reconcile` or drop them with explicit `reconcile --discard` first. Promotion writes credentials/state before removing the attempt and preserves any already-advanced publication state. Unbind also removes the attempt. `collector/` holds no owner, PID, queue or expiry record; the kernel lock on that directory is released when the collecting process dies. No per-message spool files or vendor transcript parsing are used. Private atomic-write temporary files are cleaned after normal errors or the next successful slot access; directory/named-mutex locking introduces no lock file. The Windows named collector mutex is scoped to one Windows login session, so collectors running in two concurrent login sessions of the same machine do not exclude each other. Windows uses its native user/file-access boundary; POSIX permission-bit checks are not claimed as Windows ACL enforcement.

A report sequence and its pending body are saved in one atomic write before HTTP publication, and a Stop hook saves it even when the Service endpoint is missing because the Service is stopped. A Stop reply given while an earlier publication is unresolved is saved in the same way as a held reply behind it. Only the head is ever sent; a held reply has never been attempted, so reporting it under its original sequence once everything before it settles is its first publication, not a replay. Confirmation clears the head, advances the confirmed watermark and promotes the next held reply. On the next hook or `relay status`/`relay reconcile`, query the head's original key: accepted clears it; explicitly absent supplements the same sequence; unavailable or malformed evidence retains an unknown head and sends nothing behind it. No new-ID auto-replay occurs. `reconcile --resend` and `--discard` are explicit decisions about the head only; discard preserves the consumed sequence so a later observed jump records a gap, and the next held reply becomes the head unsent. The backlog holds at most eight replies and its encoded state stays under the 2 MiB read bound minus 64 KiB headroom. Bodies are stored without HTML escaping, so `<`, `>` and `&` cost one byte each, and seven replies of the 256 KiB maximum always fit; the count bound is reached first for replies up to about 240 KiB each; a further Stop reply is refused before it consumes a sequence and the hook reports on stderr that it was not retained. Readers reject held replies without a head, out of order or beyond these bounds. Relay state stays schema 2 and a single-pending state loads unchanged; only current CLIs understand `held`, so relay hooks and commands should use the current CLI. There is no background client reaper: an aged backlog is reconciled at the next CLI/hook opportunity.

A crash before that atomic write leaves no local record and consumes no sequence. It may be undetectable even if later publications arrive, and must never create a fabricated delivery record. A missing-sequence gap is reported only from a real observed jump, not inferred from quiet time or missing native transcript text.

The Service's owner-only `relay-endpoint.json` is ephemeral endpoint discovery for local CLI clients, not a Room credential or a Registry field. It contains the current numeric-loopback URL and a scoped relay-setup token — sufficient for service discovery, Project registration, native Room creation, pair-default reads and the native binding lifecycle, never full Management authority or browser session bootstrap — and must not be exported. Clients re-read it to follow Service port/token changes. Registry checkpoints use strict schema 3 with canonical `slot1`/`slot2` keys and immutable host mode; native credential hashes and relay state never enter it. A retired checkpoint retires the whole Service root rather than being rewritten in place.

Native Room backups do not stop user-owned processes and do not include workspace slot credentials. Stop native work explicitly when consistency matters, preserve the workspace state separately, and inspect side effects before reconnecting restored bindings.

### Claude inbox capability (workspace-private)

`<workspace>/.pairroom/rooms/<room>/slots/<slot>/claude-inbox.json` contains the
local inbox address/token and exact native bind ID, generation and session ID.
It is captured only by confirmed Claude bind/Stop, atomically replaced when its
content changes (an identical private file is left in place), and
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
