# Storage and recovery

## Durable vs ephemeral

| Kind | Examples | After restart |
|---|---|---|
| Durable | Room metadata, Message, FIFO delivery / processing projection, collaboration instructions, permission profile, Turn summary, resolved approval, Binding, attachment metadata | Replayed from the Event Log / registry |
| User configuration | Named Agent pairs and default ID | Read from `agent-pair-profiles.json` under the Service data root, independently of Registry-index rebuild |
| Direct LAN client state | Per-Room certificate, admission, original delivery/wake receipts and verified evidence cache | Read from the private per-user client store, independently of any local Service |
| Native session ownership | Exact Runtime/session reservation across hosted and direct Room associations | Preserve the private per-user identity store; it is not a disposable locator index |
| Ephemeral | native process, current stdout connection, vendor request ID, active owner, transient text delta | Not restored |

Room-owned FIFO entries are persistent only while PairRoom can prove they did not cross the native submission boundary. Any input that may already have produced side effects without a confirmed ownership result is not executed again automatically.

## Event Log

The current writer and bounded read-compatibility window are:

| Format | New writes | Existing local reads |
|---|---|---|
| Room Store / provisioning | 13/6 | 12/5 |
| Registry checkpoint | 4 | 3 |

A Room uses an append-only JSONL store. Metadata schema is checked before Event Log replay, then current-schema events are replayed in order to rebuild the projection. Readers accept the explicit pairs **Store 12/provisioning 5** for existing local Rooms and **Store 13/provisioning 6** for new Rooms. Schema ≤11 remains retired and future formats fail before replay, repair, or mutation. A Service provisions new Rooms as schema 13/provisioning 6 with explicit `host_mode`; a Room created by standalone `pairroom serve`, outside any Service registry, writes Room metadata schema 13 only, with no provisioning fact and no `host_mode`, and a Service refuses to adopt such a store. LAN sharing and its awaiting-peer selection require 13/6; the older local pair cannot contain LAN permissions. Existing local event logs and metadata are read unchanged. Registry checkpoint 3 remains readable; newly written derived checkpoints use 4. Missing metadata is not inferred for a published Room. Mismatched pairs and illegal or retired-actor events fail explicitly instead of guessing a repair.

The format declarations and writer/reader boundaries live in `internal/version/version.go`, `internal/service/provision.go`, `internal/service/registry.go`, and `internal/store/`; model events and apply code own their contents. `make docs-check` compares the numeric window above and the current Architecture, Troubleshooting, and Upgrading summaries with those source declarations.

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

The Event Log stores only verified presentation metadata and an opaque attachment ID. Absolute host paths are resolved only at the adapter boundary and do not enter the API transcript. Attachments have count, type, and total-size limits. Explicit Native evidence upload accepts UTF-8 logs, scripts, configuration and patches as inert `file` objects (`text/plain`, at most 5 MiB); image validation remains separate. A message carries at most eight attachments and 20 MiB total. LAN ingress and downloaded caches have a 100 MiB per-Room bound on committed content plus manifest metadata, with a separate 32 MiB temporary-write budget that also counts crash leftovers across reopening. Exhaustion refuses further writes and requests inspection; it does not authorize deleting valid cached or uncertain-delivery evidence. Local-only uploads do not inherit LAN storage quotas. [LAN evidence sharing](LAN_NATIVE.md#share-a-useful-bug-report) describes these budgets. Filenames are presentation metadata; opaque IDs choose private content paths. A guest verifies the manifest, byte count, SHA-256 and content type before storing the file under its own private directory and before acquiring the delivery lease. `--ref` continues to describe a local reference and never uploads it.

An upload that no message ever references (an abandoned draft, a failed send, a composer image whose removal request never arrived) is **reclaimed** from the Room's `attachments/` directory once it is older than **7 days**. An active Embedded or Native Room runs a pass about ten seconds after activation and then hourly, removing at most 32 uploads per pass. A candidate is removed only if no message in the complete replayed transcript references it, in any state: Embedded messages and retries, and Native queued, delivering, handed-off, unknown, cancelled and `@user` messages, explicit Retries and routed Stop publications. Quoted images are part of the quoting message. The check and removal run under the same lock as message admission (Embedded `routingMu`, the Native relay lock), so a concurrent send either commits its reference first and keeps the file, or fails to resolve the image and publishes nothing. A Room whose Event Log writer has failed is not reclaimed. Reclamation writes no event and never touches `events.jsonl` or `metadata.json`, so append-only integrity and the Room metadata schema are unchanged; it removes the manifest before the content, as explicit removal does, and a later pass removes content left behind by an interruption.

The seven-day grace exceeds the upload-then-send window by far, because a Native browser keeps an unconfirmed draft, including its attachment IDs, in localStorage and may retry it days later. A draft retried after its upload was reclaimed fails with an explicit "unknown attachment" error and publishes nothing, since reclamation proves no message referenced that upload; Forget the draft and attach the image again in a new message. Reclamation relies on the local clock: a forward jump of more than seven days can reclaim a recent unsent upload. Only the number of reclaimed uploads is logged.

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

Native bindings, publication receipts, observed publication gaps, inbox messages and failure categories are append-only Room events. A local participant's binding fact carries the official `session_id` captured at bind and stores only its credential hash plus public metadata; never raw relay secrets. A remote participant's host-side fact contains its admitted key, Runtime, slot and generation, with no guest native session/transcript identity. Publication acceptance and optional enqueue share one append, so response loss cannot create an accepted-without-inbox gap. Queued work survives restart; any recovered `delivering` becomes `unknown`, which requires explicit Retry unless the original claimer's receipt-matched acknowledgement arrives while no Retry is pending. A ten-second acknowledgement lease bounds the live collector crash window. Terminal `handed_off` means stdout only.

A local binding's workspace footprint is `.pairroom/rooms/<room>/slots/<slot>/state.json`, `credentials` (0600 on POSIX), a `bootstrap` file only when an earlier version left one (current binds do not write it; unbind removes it if present), an empty `collector/` lock-anchor directory, and a disclosed `.pairroom/` line in `.gitignore`. `state.json` contains binding/generation/session identity, sequence watermarks, block count, the last confirmed transcript reference (resynchronized from the Service by an idempotent `bind`), a local last-hook timestamp and a bounded publication backlog: at most one pending `{seq,text,at,unknown}` head plus up to seven `held` replies behind it with the next consecutive sequences. An unconfirmed bind uses one private `bind-attempt.json` containing the candidate state, credentials and replacement intent; it is excluded from active discovery. Failed requests leave committed files intact; same-session bind retry without --create/--replace reuses the original key and replacement intent; a new explicit --replace supersedes the attempt. A replacement starts fresh publication state, so it is refused before any Service call while a pending head or held reply exists; settle them with `reconcile` or drop them with explicit `reconcile --discard` first. Promotion writes credentials/state before removing the attempt and preserves any already-advanced publication state. Unbind also removes the attempt. `collector/` holds no owner, PID, queue or expiry record; the kernel lock on that directory is released when the collecting process dies. No per-message spool files or vendor transcript parsing are used. Private atomic-write temporary files are cleaned after normal errors or the next successful slot access; directory/named-mutex locking introduces no lock file. The Windows named collector mutex is scoped to one Windows login session, so collectors running in two concurrent login sessions of the same machine do not exclude each other. Windows uses its native user/file-access boundary; POSIX permission-bit checks are not claimed as Windows ACL enforcement. Local participants in a Room share one workspace and read each other's `state.json` without the slot lock, so on Windows PairRoom opens slot state, credentials, the Claude inbox capability and `relay-endpoint.json` with read/write/delete sharing and replaces them with POSIX-semantics rename; a reader that does not share delete access (an older CLI, antivirus or an indexer) is retried for at most about one second before the save fails and its temporary file is removed.

A report sequence and its pending body are saved in one atomic write before HTTP publication, and a Stop hook saves it even when the Service endpoint is missing because the Service is stopped. A Stop reply given while an earlier publication is unresolved is saved in the same way as a held reply behind it. Only the head is ever sent; a held reply has never been attempted, so reporting it under its original sequence once everything before it settles is its first publication, not a replay. Confirmation clears the head, advances the confirmed watermark and promotes the next held reply. On the next hook or `relay status`/`relay reconcile` (status settles and publishes the whole backlog, not only the head), query the head's original key: accepted clears it; explicitly absent supplements the same sequence; unavailable or malformed evidence retains an unknown head and sends nothing behind it. No new-ID auto-replay occurs. `reconcile --resend` and `--discard` are explicit decisions about the head only; discard preserves the consumed sequence so a later observed jump records a gap, and the next held reply becomes the head unsent. The backlog holds at most eight replies and its encoded state stays under the 2 MiB read bound minus 64 KiB headroom. Bodies are stored without HTML escaping, so `<`, `>` and `&` cost one byte each, and seven replies of the 256 KiB maximum always fit; the count bound is reached first for replies up to about 240 KiB each; a further Stop reply is refused before it consumes a sequence and the hook reports on stderr that it was not retained. Readers reject held replies without a head, out of order or beyond these bounds. Local relay state keeps schema 2 and a single-pending state loads unchanged; direct LAN state uses schema 3 with the same publication backlog. Only current CLIs understand `held`, so relay hooks and commands should use the current CLI. There is no background client reaper: an aged backlog is reconciled at the next CLI/hook opportunity.

Gemini adds an optional private `gemini_response` cursor (UTF-8 byte count and SHA-256 digest) to local relay state schema 2 and direct LAN state schema 3. It identifies the cumulative AfterAgent prefix already retained in the WAL, without keeping an extra response body. The cursor and its new publication are saved in the same atomic replacement, including held replies; receipt reconciliation preserves the cursor. Fresh user turns replace it regardless of body equality, while a boundary that cannot be retained invalidates it when the state file remains writable. Known normalization or retention failures emit Gemini's successful `continue:false` decision and skip collection, so other hooks cannot continue across an observed lost boundary even when invalidation cannot be saved. This is separate from the unobservable crash-before-write window below. Missing or mismatched continuation cursors fail closed rather than replaying a cumulative response. The exact `[no response text]` fallback is ambiguous with literal model output: PairRoom records an unaddressed receipt but stops automatic continuation rather than guessing its cumulative prefix. Gemini's incoming hook JSON is bounded at 16 MiB to accommodate the initial reply plus eight continuations even with sixfold JSON escaping; each normalized new reply still has the ordinary 256 KiB body limit, and other runtimes retain the 2 MiB hook-input limit. See [Gemini hook boundaries](PROTOCOL.md#gemini-hook-and-acp-boundaries).

A crash before that atomic write leaves no local record and consumes no sequence. It may be undetectable even if later publications arrive, and must never create a fabricated delivery record. A missing-sequence gap is reported only from a real observed jump, not inferred from quiet time or missing native transcript text.

The Service's owner-only `relay-endpoint.json` is ephemeral endpoint discovery for local CLI clients, not a Room credential or a Registry field. It contains the current numeric-loopback URL and a scoped relay-setup token — sufficient for service discovery, catalog and pair-default reads, Project registration, native Room creation and native bind (not unbind), never full Management authority or browser session bootstrap; [Security](../SECURITY.md#native-relay-credentials-and-evidence) lists the exact routes — and must not be exported. Clients re-read it to follow Service port/token changes. New Registry checkpoints use strict schema 4, with bounded read compatibility for existing local schema-3 checkpoints, canonical `slot1`/`slot2` keys and immutable host mode; native credential hashes and relay state never enter it. A retired checkpoint retires the whole Service root rather than being rewritten in place.

Native Room backups do not stop user-owned processes and do not include workspace slot credentials. Stop native work explicitly when consistency matters, preserve the workspace state separately, and inspect side effects before reconnecting restored bindings.

### Direct LAN client state

Existing local workspace bindings keep relay state schema **2**. A direct LAN
binding uses schema **3** and an explicit reference to its private per-user
client identity. It does not reinterpret `endpoint_path` as a remote URL or
replace a local Service's discovery file. The client record retains the host
endpoint and public-key pin, remote Room/slot/generation, original admission
request, and per-Room certificate. Its opaque local Room ID includes the host
identity, so Rooms on different hosts cannot collide. Local native session
metadata and publication recovery remain private. Unknown or inconsistent
formats fail before a network effect; existing local state is not rewritten
into a remote binding.

The client store is `pairroom/lan-clients` under the operating system's user
configuration directory, separate from all Service data roots. A bounded catalog
lets an optional same-user Service project joined bindings for a local
dashboard, human actions and supported wake observation. CLI/hooks and this
observer use the same private record and locks. They do not maintain a second
authoritative Room inbox, and stopping the observer does not stop direct
commands. Distinct native sessions can concurrently use locally hosted Rooms
and several remote hosts without changing a global endpoint.

The separate `pairroom/native-identities` directory under the same user
configuration directory retains each exact Runtime/session's local ownership
reservation: association, bind ID and generation. Pending LAN admissions also
reserve their session; a stopped Service or archived hosted Room retains its
ownership. CLI and Service writers coordinate these private records across
processes, so a second association cannot acquire the same native session.
These records are durable authority for local exclusion, unlike disposable
session locators. Removing them is not a routing repair, and possessing one
does not grant remote Room permission.

Before releasing an incoming envelope, the client verifies/downloads evidence,
acquires the host's claim and saves its original delivery receipt. Successful
collector stdout is recorded before ACK; a later bind/resume, collector or
optional observer can settle only that original receipt without printing the body
again. A failed or uncertain stdout remains unknown. Wake reserves at the host
and saves a local spent receipt before any local effect; spent effects are not
retried automatically. Both journals are bounded and fail closed on exhaustion
instead of discarding unresolved outcomes. No live collector, hook or optional
observer means messages remain in the host queue.

Back up both private per-user stores together with the matching Service roots
and workspace binding state while their writers are stopped. A Service-root
backup does not include these per-user stores; a Room archive does not include
native session credentials. Explicit `join --replace` preserves the previous
client record and receipts at
`lan-clients/<lan-id>/retired/<local-bind-id>/client.json`. Workspace publication
state and credentials are retained at `.pairroom/retired/<local-bind-id>/state.json`
and `credentials`; replacement also archives the original `join-attempt.json`
there. Explicit CLI leave/detach archives the state and credentials before
removing the active slot files, while retaining the original join attempt for
later replacement. Dashboard leave/detach immediately retires discovery through
the authoritative client record even when old workspace files remain. That
retained evidence is never replayed into a new admission.
Retain original identities for recovery. Revocation or archive never triggers
automatic rejoin, and restoring files cannot restore remote permission that
the host has revoked.

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
