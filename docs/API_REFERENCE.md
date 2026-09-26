# HTTP and event API reference

PairRoom's browser UI uses a local HTTP API and SSE. CLI / UI is the preferred entry. Clients that call the API directly should treat it as a local control plane that evolves with PairRoom releases, not as a permanently stable public SaaS API.

## Safety boundary

- Every built-in listener accepts numeric loopback addresses only. Wildcard, LAN, and hostname binds are rejected, not enabled by setting a token;
- For remote access, use SSH local port forwarding to the loopback listener and retain the normal browser-session or bearer authentication;
- The API must not return Provider secrets or absolute host paths of attachments;
- A destructive request must name the Project / Room identity and obey archive, active Turn, and Binding preconditions.

## Resource families

The Management API owns Project and Room registration, immutable per-Room Agent selection, Binding, archive, backup, and Service diagnostics. The Room API owns message, Turn, participant, independent permissions, approval, retry, cancel, attachment, and the event stream. In-app Room tabs use the Management same-origin surface: `/api/v1/rooms/{room}/surface/…` uses the Management Session, and the server injects the Runtime bearer. `PATCH /api/v1/runtime-policy` only adjusts the concurrent Runtime cap. `POST /api/v1/rooms/{room}/open-browser` opens the system browser after the Runtime is ready. An archived Room has no surface and cannot be opened externally.

Room Turn, approval, steering, pagination and retry contracts below describe embedded hosting. Native hosting has its own [relay contracts](#native-host-mode); do not apply embedded `intent`, pagination or process controls to a native Room.

## Agent catalog and Room creation

`GET /api/v1/agent-catalog` and `POST /api/v1/agent-catalog/refresh` return all three Runtime entries with availability diagnostics, sanitized CC Switch Profile summaries, local model suggestions, disabled reasons, the current two Service defaults, and canonical `collaboration_default` instructions for the creation preview. The response never contains raw Profile configuration, endpoints, headers, tokens, API keys, or Runtime arguments. Refresh is explicit, and Room creation still re-resolves the selected Profile server-side instead of trusting the catalog returned to the browser.

`POST /api/v1/projects/{project}/rooms` accepts an optional `name` and complete two-slot `bindings` map plus an optional complete `agents` map keyed by canonical ActorIDs `slot1` and `slot2`. HTTP accepts numeric `1`/`2` input aliases only at request parsing; legacy runtime-named strings are rejected. Omitting `agents` snapshots the saved default Agent pair profile, falling back to both current Service defaults only when no default profile exists. An optional `agent_pair_profile_id` explicitly selects a saved pair. Supplying both a non-empty profile ID and `agents` is rejected, as are missing profile IDs and partial/null `agents`. A complete explicit `agents` map bypasses the saved default; this is how the browser sends temporary overrides. A selection has this shape:

```json
{
  "runtime": "codex",
  "provider": {"source": "cc-switch", "app_type": "codex", "profile_id": "profile-id"},
  "model": "custom-model-id",
  "effort": "high",
  "instructions": "Review compatibility boundaries.",
  "permission_mode": "",
  "approval_policy": "on-request",
  "sandbox": "workspace-write"
}
```

The created Room returns the immutable `agents` map. There is no Agent-reconfiguration endpoint.

## Project and Room display order

`GET /api/v1/service` includes `navigation_order` with `{"schema":1,"projects":[],"rooms":{}}` when no preference has been saved. Ranked IDs come first; newly discovered items retain Registry creation order. Active and archived Rooms remain separate display groups. All Management clients of the same Service share the order.

`PATCH /api/v1/navigation-order` uses normal Management bearer or browser-session/CSRF authentication. Submit a single anchored move, not a replacement array:

```json
{"kind":"room","id":"room-to-move","target_id":"another-room","position":"before"}
```

`kind` is `project` or `room`; `position` is `before` or `after`. Distinct existing IDs are required. Rooms must belong to the same Project and active/archived group. A move changes display order only, never Project membership, lifecycle, runtime capacity, session state, or Room events. It may be performed while a Room is running. Concurrent moves are serialized against the latest saved order, retaining unrelated/filtered items. Newly discovered IDs are appended and deleted IDs pruned when saving. Success returns the updated order; malformed/invalid moves return 400, unavailable preference storage returns 503.

Preferences live in `navigation-order.json`, separate from the rebuildable Registry. On a corrupt/unreadable file, the Service snapshot still works but sets `navigation_order_error: true`; ordering is disabled until the file is repaired. Back up that file before replacing it. Ordinary Service health is not a claim that this optional preference file is valid.

The Projects page uses one row per Project, and clicking the row opens that Project; the Project-name link stays the keyboard and screen-reader entry point, and the row's own buttons (recheck, add Room) keep their separate actions. Drag the row itself to reorder; right-click a row (or focus a control in it and press Alt+↑/Alt+↓) for Move up/Move down. Escape cancels a drag without opening the row. The sidebar and Project-detail Room lists offer the same reorder controls but do not navigate on row click; there the Move up/Move down entries are part of the shared Room context menu, which also carries rename, close room tab and archive, and Alt+↑/Alt+↓ still moves the focused row. Closing a Room tab only closes that view: it does not archive, delete or suspend a Room, append a Room event, change display order or stop a running Turn. Diagnostics and the existing Service support export share **Settings → Diagnostics**; old `#/diagnostics[/room]` links redirect there without starting a check.

## Agent pair profiles

All routes use the existing Management bearer or browser-session/CSRF boundary. They operate on Service-scoped templates, not native CC Switch Profiles or existing Rooms.

| Method/path | Request and result |
|---|---|
| `GET /api/v1/agent-pair-profiles` | Read the catalog, including the optional default ID |
| `POST /api/v1/agent-pair-profiles` | Create `{name, agents, is_default}`; return the updated catalog with HTTP 201 |
| `PUT /api/v1/agent-pair-profiles/{profile}` | Replace the named profile's `{name, agents, is_default}`; preserve its ID; return catalog with HTTP 200 |
| `PATCH /api/v1/agent-pair-profiles/default` | Set `{"profile_id":"pair-id"}` or explicitly clear with `{"profile_id":""}`; return catalog |
| `DELETE /api/v1/agent-pair-profiles/{profile}` | Delete; also clear the default if it referenced this profile; return catalog |

The catalog shape is `{"schema":1,"default_profile_id":"","profiles":[{"id":"pair-id","name":"Daily pair","agents":{"slot1":{...},"slot2":{...}}}]}`. `agents` is the same complete two-slot `AgentSelection` map used for Room creation. POST/PUT require a name and both selections; `is_default` defaults to false and replaces that profile's default status, including clearing it on PUT. Names are trimmed, non-blank, case-insensitively unique, and limited to 160 UTF-8 bytes without control characters. The catalog allows up to 100 profiles.

Saving validates the selection structure without materializing Providers or probing native sessions. Removed/offline Provider references can be retained and repaired; creation and activation still perform their existing Provider validation. Unknown fields, including credentials, command configuration, Bindings, and collaboration, are rejected. No native credential material is stored or returned. Missing profile IDs return 404; duplicate names/capacity return 409; invalid input returns 400; unreadable/corrupt/unsupported profile storage returns 503 rather than being overwritten or silently ignored. Profile changes do not write Room events or reconfigure existing Rooms.

To create an embedded Room using a saved pair, omit `agents` and include:

```json
{"agent_pair_profile_id":"pair-id","bindings":{"slot1":{"mode":"new"},"slot2":{"mode":"new"}}}
```

Omit both `agents` and `agent_pair_profile_id` to use the saved default or Service defaults. Explicit full `agents` always wins by being the only selection source; do not send a profile ID alongside it. Profile contents are copied once, not linked to the Room.

## Room names and native session correspondence

Omitting `name`, or supplying only ordinary spaces, generates `Room-<short Room ID>` once during the creation transaction. The returned `Room.name` and Event Log contain that same value. No model call or client-side naming authority is involved. Supplied names are trimmed, at most 160 UTF-8 bytes, and must not contain control characters. Invalid names return HTTP 400 before provisioning or draining a runtime.

`PATCH /api/v1/rooms/{room}` accepts only `{"name":"New name"}`. The existing safe-boundary path waits for active work to finish, suspends the runtime, then appends `service.room.renamed` before updating the registry. It never interrupts a Turn. A blank rename is rejected; an unchanged name is a no-op and does not suspend the runtime or append another event. Room ID, data directory, native session IDs, Bindings, collaboration, permissions, and message history do not change. The endpoint uses the normal Management authentication/CSRF boundary.

Each Management Room projection includes `runtime_names`, keyed by the stable `slot1` / `slot2` slots. Values are desired names, computed from `<Room name> · <current @handle> · <short Room ID>`; they do **not** prove native synchronization. Duplicate runtimes keep their `0`/`1` handle suffixes. Long display names are shortened to fit the shared 100-Unicode-scalar title budget while preserving the handle and ID suffix. Short IDs are display hints, never lookup keys.

Participant `runtime` adds `session_name` (desired native title) and `session_name_status`:

| Status | Meaning |
|---|---|
| `pending` | Native session naming has not completed |
| `configured` | Claude's advertised `--name` option was supplied at launch; not a separate title-read acknowledgement |
| `synced` | Codex/Grok acknowledged their metadata rename request |
| `unsupported` | The CLI did not expose the naming option/method |
| `failed` | Naming failed; the original session remains usable, with retry on next activation |
| `simulated` | Mock runtime only; no native title was changed |

Participant `runtime` also carries `provider` — the internal Provider label, either `native` or the reference form `cc-switch:<app_type>/<profile_id>` — and, for a CC Switch Profile resolved by this release or later, `provider_name`: the Profile display name, at most 160 UTF-8 bytes, with control characters stripped and credentials redacted against the Profile's full secret set. Browser UIs display `provider_name` and keep the raw label in a tooltip; the label is a stable machine reference, never a lookup key or a display name. Events recorded before `provider_name` existed omit it, and a UI then falls back to the label. `effort` mirrors the creation-time selection and is empty when the slot inherits the native default.

Native titles are applied on activation/session opening, including permission-driven restarts, only after the Room binding exists. Provisioning validators do not rename sessions. Dormant or archived Rooms do not spawn a native process merely to rename one: their native titles catch up when next activated. An active Room rename uses the safe-boundary suspension above; reactivate normally afterward. Names use native metadata, not prompts or direct writes to vendor session storage. See [Architecture](ARCHITECTURE.md#room-and-native-session-names) for adapter support.

## Creation-only collaboration and independent permissions

Room creation also accepts optional `collaboration`. Omission or `{"mode":"default"}` persists canonical version-2 flexible Lead/Executor instructions. Custom input is, for example:

```json
{"collaboration":{"mode":"custom","instructions":"Agent 2 plans. Agent 1 implements. Both challenge unsupported assumptions."}}
```

The server trims outer whitespace and requires non-blank UTF-8 without NUL, at most 16 KiB. Only `default` and `custom` are accepted. Default prose cannot be overwritten; choose custom instead. The response includes `{version, mode, instructions}` and the same record is stored in `room.created` and provisioning schema 5. PATCH does not accept mode or instruction changes. Retired Store/provisioning formats are rejected, not migrated implicitly; see [Protocol](PROTOCOL.md).

Participant snapshots add `responsibility` (`lead`, `executor`, or generic `participant`) and `permission_profile`. Modern `role` remains `peer` solely for old response readers; it is not a collaboration selector. Runtime policy fields describe the effective native policy.

`PUT /api/v1/participants/{actor}/permissions` accepts `{"profile":"configured"}`, `{"profile":"read-only"}`, or `{"profile":"yolo"}`. Configured restores the creation-time Agent policy, read-only uses native plan/read-only restrictions, and YOLO requests bypass/full access. The Room rejects pending Turns, queued input, and pending approvals. Invalid profiles return 400; unsafe transitions or runtime failures return 409. On success, read a fresh snapshot. `participant.permissions.requested` records intent before process effects; `participant.permissions.updated` commits effective policy after stopping the old process. A restart failure leaves the committed policy, not a broader fallback.

Permissions are not collaboration modes. They do not change the saved instructions, Runtime, Provider, model, or exact materialized session. The former `/participants/{actor}/role` route is removed (404). `target_role` submissions are rejected (400); choose one stable `to` slot or its exact displayed runtime handle. `@driver`, `@reviewer`, `@lead`, and `@executor` are not handle aliases.

## Errors

Error responses retain the English `error` field and add a stable `code`. Errors that can be safely localized may include `params` or `details`; these never contain Profile secrets. Clients localize recognized codes and display the original `error` for unknown or native diagnostics.

Status requests return the current projection. Message submission records the user Message before driving native execution; its HTTP success is not proof of a completed Turn. Judge execution from message processing, Turn summary, or SSE events. Other action receipts follow their endpoint contract: an approval resolution, for example, is recorded after the adapter response succeeds.

`POST /api/v1/messages` accepts one starting Agent and an optional `intent` of `steer` or `queue`; omission defaults to `steer`. Removed intent values and removed Room settings are rejected by the strict request decoder. Participant snapshots expose the stable slot `id`, runtime-derived `display_name`, and exact `mention_handle`. `PUT /api/v1/settings` currently accepts only `stall_warning_seconds`.

## SSE and reconnect

Durable events carry a monotonic sequence and can be resumed after disconnect. High-frequency text delta / command output and other transient telemetry may be non-persistent; token-by-token replay is not guaranteed after disconnect. After reconnect, clients should fetch a snapshot again, then continue from the durable sequence. The Room browser closes its obsolete stream and coalesces concurrent snapshot requests; failed reads use bounded backoff. It does not automatically retry message submissions.

`GET /api/v1/snapshot?message_limit=250` returns the newest messages and `message_window` pagination metadata while retaining current Room/runtime state. `message_limit` accepts integers from 0 to 1000; zero or omission retains the full-transcript response. Invalid values return HTTP 400. Older messages are available through `GET /api/v1/messages?before_seq={oldest_seq}&limit=100`, in chronological order and strictly before the cursor.

A windowed snapshot's `turns` holds the 40 most recently updated Turn summaries, each participant's current Turn, and every Turn correlated with a returned message (a summary left incomplete by an adapter that exited without a Turn boundary is not pinned); `turn_window` reports `{total, loaded}`. The full-transcript response keeps every Turn. A `messages` page adds `turns` for the Turns correlated with its messages; clients merge them by `id` without replacing a newer `updated_at`. Snapshot `events` omit `turn.summary.updated`, whose current projection is `turns`, and bound each runtime event's `text` and `data` to 4 KiB (an oversized `data` becomes `{"truncated":true,"head":…}`); the in-memory tail is also bounded to 4 MiB of payload, so an older cursor receives the documented reset. `export?format=json&include_events=1` keeps the unprojected tail.

A Turn item with `source_seqs` carries only a short `detail` preview. `GET /api/v1/turns/{turn}/items/{item}` (path segments URL-encoded) returns `{"evidence":[{seq,kind,name,text,data,created_at,truncated}]}` read from those durable records after verifying each belongs to that participant, Turn and item; one response is bounded to 4 MiB and marks a shortened record `truncated`. An unknown Turn or item is 404; an item without `source_seqs` returns an empty list because its evidence is inline. The Management Room gateway forwards only this read-only form. A `turn.summary.updated` SSE event with sequence 0 is a live, non-durable summary update: apply it to `turns` only, and never treat it as a durable fact or cursor.

`GET /api/v1/events?since={latest_seq}` resumes after that durable sequence. A non-empty `Last-Event-ID` header takes precedence over `since` on native EventSource reconnects; malformed cursors return HTTP 400. If the cursor is ahead of the Room or older than its retained event tail, the server emits `event: reset` with `{"reason":"snapshot_required","latest_seq":...}` and closes the stream. Fetch a fresh snapshot before reconnecting; do not interpret this as a Turn completion. Transient events and reset notifications never advance the durable SSE ID.

## Explicit retries

`POST /api/v1/messages/{id}/retry` returns HTTP 202 with a new auditable Message; it does not modify the failed source Message. Only an unsuccessful terminal target can be retried. While a direct retry child for that source and participant is `waiting` or `working`, another request returns HTTP 409 before creating or submitting another Message. Once the child settles, a subsequent explicit retry is allowed. This is pending-work exclusion, not exactly-once execution across crashes or arbitrarily delayed requests. Do not automatically repeat an ambiguous/failed POST; inspect durable Message state and ask for explicit retry when necessary.

## Native approval responses

`POST /api/v1/approvals/{id}` resolves a pending request with a JSON body containing `decision` and, for Claude questions, `answers`. Concurrent submissions for the same approval are rejected before calling the native adapter. The browser disables that request until its durable resolution arrives; it does not automatically retry failed writes.

Claude Code and Codex retain their native `accept`, `acceptForSession`, `decline`, and `cancel` decisions where supported. Claude `AskUserQuestion` acceptance requires one non-blank answer keyed by each exact native `question` text, with no unknown keys. Malformed, ambiguous, or incomplete question answers are rejected without consuming the request; free-form answers and the original native tool input remain unchanged.

For `grok.permission`, use the advertised native `detail.options` names and IDs rather than guessing scope:

```json
{"decision":"option:the-exact-native-optionId"}
```

PairRoom passes that exact, unique `optionId` to ACP. `cancel` returns ACP's cancelled outcome, not a remembered rejection. Legacy `accept` selects an unambiguous `allow_once`; `acceptForSession` selects an unambiguous native `allow_always` (whose scope is defined by Grok, not a PairRoom session promise). Neither grants a different scope if that kind is unavailable. Legacy `decline` selects `reject_once` or cancels if no unique one-time rejection exists. Invalid grant/option choices leave the approval pending. `grok.planExit` accepts only `accept`, `decline`, or `cancel`.

## Current source route inventory

The following method/path patterns are extracted from production HTTP registrations in `internal/server/` and `internal/service/`, including named constants. Test URLs, query examples, and rejected path-traversal inputs are not API routes. Patterns without a method are the same-origin surface gateway; their allowed operations are enforced by its handler.

<!-- generated:routes -->
- `/api/v1/rooms/{room}/surface`
- `/api/v1/rooms/{room}/surface/{path...}`
- `DELETE /api/v1/agent-pair-profiles/{profile}`
- `DELETE /api/v1/attachments/{id}`
- `DELETE /api/v1/projects/{project}`
- `DELETE /api/v1/rooms/{room}`
- `DELETE /api/v1/rooms/{room}/native-bindings/{slot}`
- `DELETE /api/v1/session`
- `GET /api/v1/agent-catalog`
- `GET /api/v1/agent-pair-profiles`
- `GET /api/v1/attachments/{id}`
- `GET /api/v1/events`
- `GET /api/v1/export`
- `GET /api/v1/git/diff`
- `GET /api/v1/git/status`
- `GET /api/v1/health`
- `GET /api/v1/messages`
- `GET /api/v1/rooms/{room}/wake-config`
- `GET /api/v1/service`
- `GET /api/v1/session`
- `GET /api/v1/snapshot`
- `GET /api/v1/turns/{turn}/items/{item}`
- `PATCH /api/v1/agent-pair-profiles/default`
- `PATCH /api/v1/navigation-order`
- `PATCH /api/v1/rooms/{room}`
- `PATCH /api/v1/runtime-policy`
- `POST /api/v1/agent-catalog/refresh`
- `POST /api/v1/agent-pair-profiles`
- `POST /api/v1/approvals/{id}`
- `POST /api/v1/attachments`
- `POST /api/v1/diagnostics`
- `POST /api/v1/maintenance/room-deletions/retry`
- `POST /api/v1/messages`
- `POST /api/v1/messages/{id}/cancel`
- `POST /api/v1/messages/{id}/retry`
- `POST /api/v1/participants/{actor}/{action}`
- `POST /api/v1/projects`
- `POST /api/v1/projects/{project}/refresh`
- `POST /api/v1/projects/{project}/rooms`
- `POST /api/v1/relay/{room}/{slot}/{action}`
- `POST /api/v1/rooms/batch-archive`
- `POST /api/v1/rooms/batch-delete`
- `POST /api/v1/rooms/{room}/activate`
- `POST /api/v1/rooms/{room}/archive`
- `POST /api/v1/rooms/{room}/native-bindings/{slot}`
- `POST /api/v1/rooms/{room}/open-browser`
- `POST /api/v1/rooms/{room}/restore`
- `POST /api/v1/rooms/{room}/suspend`
- `POST /api/v1/rooms/{room}/wake-config`
- `POST /api/v1/session`
- `PUT /api/v1/agent-pair-profiles/{profile}`
- `PUT /api/v1/participants/{actor}/permissions`
- `PUT /api/v1/settings`
<!-- /generated:routes -->

## Client compatibility principles

1. Tolerate added fields in JSON responses; send only documented request fields, because request decoding rejects unknown fields;
2. Do not derive the state machine from UI copy;
3. Re-read the projection after a destructive operation;
4. Do not treat a transient event as a durable receipt;
5. Read [Upgrading](UPGRADING.md) before a release upgrade.

Every Management `/api/…` response, including relay and rejected requests, carries `X-PairRoom-Version` with the Service release (for example `5.6.0`, without build metadata). The relay CLI uses it to name a CLI/Service release mismatch without an extra request; the version is not an authorization or compatibility negotiation.

## Explicit Service diagnostics

`POST /api/v1/diagnostics` is protected by the existing Management authentication, same-origin, and browser CSRF checks. There is no GET-triggered probe. An environment request is `{"mode":"environment"}`; an actual model check requires `{"mode":"runtime","actor":"slot2","confirm":true}`; a read-only Native Room report requires `{"mode":"native","room_id":"<room>"}`. `actor` is the stable slot (`slot1` or `slot2`), not a Runtime kind; numeric `1`/`2` are parse-time aliases. The environment and runtime modes optionally accept `room_id`; otherwise the saved default Agent pair profile or Service defaults are used. Native mode requires `room_id` and reports an already active Native Room without activating a suspended runtime; an inactive or non-Native Room returns 409. A Room's stored selections never follow later default changes. Invalid Room selections are rejected rather than replaced with Service defaults.

Only those fields are accepted: clients cannot submit executable paths, environment, credentials, session IDs, or replacement selections. Invalid requests return 400; missing Rooms return 404; concurrent diagnostic requests and invalid Room selections return 409. Checks are single-flight per Service, have bounded deadlines/output, and honor request cancellation. A failed check is evidence in an HTTP 200 report, not an HTTP transport failure. Responses use `Cache-Control: no-store`.

The response is `{schema:1, version, platform, generated_at, mode, scope, checks:[...]}`. Scope is `service_defaults`, `default_profile`, or `room`; no identity/path is exported. Native mode returns the secret-free observations document described under Native inspection instead of `checks`. Checks contain `id`, `status` (`pass`, `warn`, `fail`, or `skipped`), a fixed `code`, `duration_ms`, and optional Runtime/slot/numeric version. Installation, native startup, and matching completed model response are distinct evidence. Unselected missing CLIs warn; Mock and untested model responses remain skipped. Cleanup warnings do not invalidate a received response, but must be resolved before repeatedly starting new checks. Raw errors and process/model output are never part of this report.

Diagnostics create no durable Room events, do not activate/suspend/resume existing Rooms, and never change native login or CC Switch configuration. A live check can use Provider quota and invoke native global hooks/MCP while starting its disposable session; it is not a sandbox, repository-specific smoke test, or exhaustive tool test. See [CLI reference](CLI_REFERENCE.md#installation-versus-runtime-availability) for operational boundaries.

## Native host mode

`POST /api/v1/projects/{project}/rooms` accepts immutable `host_mode: "embedded" | "native"` (default embedded). New Rooms use Store 12/provisioning 5 in both modes. Native creation retains two Agent selections but does not apply providers/models/effort/permissions or spawn adapters; Claude Code, Codex and Grok Build are supported. Existing adapter-session Binding requests are rejected. The native surface uses the same scoped Management gateway but a relay-specific snapshot/UI.

Management-authenticated `POST /api/v1/rooms/{room}/native-bindings/{slot}` accepts a public bind ID, credential hash, the official session ID captured from the harness environment, and explicit replacement intent; association is immediate. `DELETE` revokes the binding without claiming to stop native work. The response contains public metadata and bootstrap instructions, never credentials. Model-facing long-lived secrets are not part of this API.

`POST /api/v1/relay/{room}/{slot}/{action}` has separate relay authentication, not browser-cookie or management-token authority. It requires `Authorization: Relay <secret>` and the CLI's binding/generation/official-session headers; credentials are loaded from an owner-only file, not command arguments. Actions are `inspect`, `confirm`, `report`, `publication`, `send`, `wait`, `ack`, `status`, `summary`, `history`, `doctor`, `peer`, `failure`, `park`, `unbind`, and `upload`. `history` is a read-only evidence page and `doctor` is a read-only capability/cooldown report for an already active Room; neither claims work, requeues it, activates a suspended runtime, or contacts a model. `summary` is the body-free transport inspection used by `relay status --brief` / `relay reconcile --brief`: inbox counts and at most eight unknown-delivery recovery IDs, without message bodies, native session/transcript references, or the audit log. Relay actions require a live binding; an incomplete pre-upgrade binding permits only inspection/revocation and this body-free summary, and requires explicit replacement; `confirm` re-checks the hook's official session identity against the bind-time capture. Every operation rechecks the live generation. A same-ID `send` whose delivered payload differs from the accepted original returns HTTP 409 with `code:"send_payload_conflict"`; nothing new is published. Body limit is 256 KiB UTF-8 before JSON escaping; HTTP JSON is bounded to 2 MiB. Wait is bounded to 30 seconds and performs no claim while idle. With `park: true`, an empty inbox returns `{"claim":null}` at once unless a peer reply to this slot is expected ([Protocol](PROTOCOL.md#native-host-protocol-v8)). For Grok with `park: true`, wait instead probes readiness without claiming: `{"claim":null,"foreground_required":true}` means run a foreground wait, not delivery or acknowledgement. This leaves the full FIFO input queued and avoids Grok's clipped hook feedback. Only foreground collection or a non-Grok hook acknowledges after writing the full envelope to stdout.

Within a native Room surface, `GET api/v1/snapshot` returns `{room, relay, protocol, config_notice, summary, identities}`. `relay` contains public bindings, ordered messages, audit entries and a sequence cursor. `GET api/v1/events` emits `native` SSE invalidations; clients refresh snapshots, never replay commands. `POST api/v1/messages` accepts `{id,to,text,attachment_ids,quote_id}`. Quoted messages must belong to the same Room. `POST api/v1/messages/{id}/cancel` is queued-only; `/retry` requires an unknown source and generates a new ID. `POST api/v1/participants/{slot}/park` accepts `{enabled}`. Uploads return the attachment object directly; downloads retain existing authenticated image validation. There is no native Interrupt/start/restart/permission control.

For routine display, `GET api/v1/snapshot?tail=1` returns at most 300 complete recent messages and 80 recent audit entries, with `relay.total_messages` and `relay.total_audit` describing the full retained history. Message and quote text share a 1 MiB budget; the newest message is always retained intact. This bounds text, not total JSON bytes including metadata. Unparameterized snapshots and `GET api/v1/export` remain complete; export ignores `tail=1`. The bounded view is not an inbox, an audit-retention limit, or evidence that older pending work disappeared.

A native send ID is bound to its accepted delivered payload. Retrying it with different text, target, attachments or quoted context returns HTTP 409, not a success receipt for unrelated content. An identical retry returns its original receipt using accepted attachment metadata even if image files later become unavailable; new publication and collection still validate image bytes. Use a new ID only for an intentional new publication. Browser validation measures UTF-8 bytes before upload or publication; an oversized draft remains editable and is never silently clipped. After uncertain publication, retain the original ID and payload rather than treating a later error as proof that nothing was accepted.

`GET api/v1/health` retains Room authentication and returns `{ok:true,host_mode:"native"}` for a healthy relay. A fatal Event Log writer or listener state returns HTTP 503 with `ok:false`, `code:"runtime_not_ready"` and a fixed, non-sensitive error message. It never reports raw storage/listener errors, and healthy relay transport does not establish native model presence or readiness. Fatal writer errors also terminate the SSE stream instead of continuing healthy-looking heartbeats.

Native delivery states are `queued`, `delivering`, `handed_off`, `unknown`, `cancelled`, and `human` (UI escalation). `handed_off` means stdout was written, not native acceptance. Display binding plus last observed activity rather than live-presence claims. Public snapshots and exports never expose credential hashes, raw secrets or claim receipts.

## Native inspection (authenticated Room surface)

- `GET /api/v1/pending?limit=10&cursor=...`: oldest unresolved messages, separate
  from chat tail, with `total`, `has_more`, `next_cursor`, `sequence` and `messages`.
- `GET /api/v1/history?id=...` or `?limit=20&cursor=...&since=...`: single-message
  lookup or newest-first history. Queries reject unknown/duplicate filters and
  incompatible cursors. Invalid native queries use the existing 409 error contract.
- `GET /api/v1/sends/<client-id>`: `{found,message?}` for an original **user**
  publication ID only; peer client IDs cannot match it. Absence is not proof an
  in-flight request cannot still arrive; explicit retries retain the same ID.
- `GET /api/v1/diagnostics`: secret-free session/transport observations and next
  actions; no wake or vendor request is issued. The Management diagnostics POST
  accepts `{"mode":"native","room_id":"..."}` for an already active Native Room.
- `GET /api/v1/review`: capture bounded Git evidence from the Room's trusted project;
  `?id=...` compares that project's current state to a message's review observation.
  Incoming anchor paths are never used as read authority.

These routes also work through the authenticated Management surface gateway.
Relay credentials use POST `history`/`doctor` through the existing relay API.
History, inspection and review do not grant native execution/approval control.
The Native snapshot additionally exposes a body-free `summary`; its sequence
identifies the observation independently of the bounded chat snapshot.
