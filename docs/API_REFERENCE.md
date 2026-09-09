# HTTP and event API reference

PairRoom's browser UI uses a local HTTP API and SSE. CLI / UI is the preferred entry. Clients that call the API directly should treat it as a local control plane that evolves with PairRoom releases, not as a permanently stable public SaaS API.

## Safety boundary

- Every built-in listener accepts numeric loopback addresses only. Wildcard, LAN, and hostname binds are rejected, not enabled by setting a token;
- For remote access, use SSH local port forwarding to the loopback listener and retain the normal browser-session or bearer authentication;
- The API must not return Provider secrets or absolute host paths of attachments;
- A destructive request must name the Project / Room identity and obey archive, active Turn, and Binding preconditions.

## Resource families

The Management API owns Project and Room registration, immutable per-Room Agent selection, Binding, archive, backup, and Service diagnostics. The Room API owns message, Turn, participant, independent permissions, approval, retry, cancel, attachment, and the event stream. In-app Room tabs use the Management same-origin surface: `/api/v1/rooms/{room}/surface/…` uses the Management Session, and the server injects the Runtime bearer. `PATCH /api/v1/runtime-policy` only adjusts the concurrent Runtime cap. `POST /api/v1/rooms/{room}/open-browser` opens the system browser after the Runtime is ready. An archived Room has no surface and cannot be opened externally.

## Agent catalog and Room creation

`GET /api/v1/agent-catalog` and `POST /api/v1/agent-catalog/refresh` return all three Runtime entries with availability diagnostics, sanitized CC Switch Profile summaries, local model suggestions, disabled reasons, the current two Service defaults, and canonical `collaboration_default` instructions for the creation preview. The response never contains raw Profile configuration, endpoints, headers, tokens, API keys, or Runtime arguments. Refresh is explicit, and Room creation still re-resolves the selected Profile server-side instead of trusting the catalog returned to the browser.

`POST /api/v1/projects/{project}/rooms` accepts an optional `name` and complete two-slot `bindings` map plus an optional complete `agents` map keyed by historical ActorIDs `claude` and `codex`. Omitting `agents` snapshots the saved default Agent pair profile, falling back to both current Service defaults only when no default profile exists. An optional `agent_pair_profile_id` explicitly selects a saved pair. Supplying both a non-empty profile ID and `agents` is rejected, as are missing profile IDs and partial/null `agents`. A complete explicit `agents` map bypasses the saved default; this is how the browser sends temporary overrides. A selection has this shape:

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

The created Room returns the immutable `agents` map. There is no Agent-reconfiguration endpoint. Schema-v1 Rooms instead return `legacy_defaults: true` and no `agents` map.

## Project and Room display order

`GET /api/v1/service` includes `navigation_order` with `{"schema":1,"projects":[],"rooms":{}}` when no preference has been saved. Ranked IDs come first; newly discovered items retain Registry creation order. Active and archived Rooms remain separate display groups. All Management clients of the same Service share the order.

`PATCH /api/v1/navigation-order` uses normal Management bearer or browser-session/CSRF authentication. Submit a single anchored move, not a replacement array:

```json
{"kind":"room","id":"room-to-move","target_id":"another-room","position":"before"}
```

`kind` is `project` or `room`; `position` is `before` or `after`. Distinct existing IDs are required. Rooms must belong to the same Project and active/archived group. A move changes display order only, never Project membership, lifecycle, runtime capacity, session state, or Room events. It may be performed while a Room is running. Concurrent moves are serialized against the latest saved order, retaining unrelated/filtered items. Newly discovered IDs are appended and deleted IDs pruned when saving. Success returns the updated order; malformed/invalid moves return 400, unavailable preference storage returns 503.

Preferences live in `navigation-order.json`, separate from the rebuildable Registry. On a corrupt/unreadable file, the Service snapshot still works but sets `navigation_order_error: true`; ordering is disabled until the file is repaired. Back up that file before replacing it. Ordinary Service health is not a claim that this optional preference file is valid.

The Projects page uses one row per Project. Drag the row itself to reorder; right-click a row (or focus a control in it and press Alt+↑/Alt+↓) for Move up/Move down. The sidebar and Project-detail Room lists offer the same controls. Escape cancels a drag. Diagnostics and the existing Service support export share **Settings → Diagnostics**; old `#/diagnostics[/room]` links redirect there without starting a check.

## Agent pair profiles

All routes use the existing Management bearer or browser-session/CSRF boundary. They operate on Service-scoped templates, not native CC Switch Profiles or existing Rooms.

| Method/path | Request and result |
|---|---|
| `GET /api/v1/agent-pair-profiles` | Read the catalog, including the optional default ID |
| `POST /api/v1/agent-pair-profiles` | Create `{name, agents, is_default}`; return the updated catalog with HTTP 201 |
| `PUT /api/v1/agent-pair-profiles/{profile}` | Replace the named profile's `{name, agents, is_default}`; preserve its ID; return catalog with HTTP 200 |
| `PATCH /api/v1/agent-pair-profiles/default` | Set `{"profile_id":"pair-id"}` or explicitly clear with `{"profile_id":""}`; return catalog |
| `DELETE /api/v1/agent-pair-profiles/{profile}` | Delete; also clear the default if it referenced this profile; return catalog |

The catalog shape is `{"schema":1,"default_profile_id":"","profiles":[{"id":"pair-id","name":"Daily pair","agents":{"claude":{...},"codex":{...}}}]}`. `agents` is the same complete two-slot `AgentSelection` map used for Room creation. POST/PUT require a name and both selections; `is_default` defaults to false and replaces that profile's default status, including clearing it on PUT. Names are trimmed, non-blank, case-insensitively unique, and limited to 160 UTF-8 bytes without control characters. The catalog allows up to 100 profiles.

Saving validates the selection structure without materializing Providers or probing native sessions. Removed/offline Provider references can be retained and repaired; creation and activation still perform their existing Provider validation. Unknown fields, including credentials, command configuration, Bindings, and collaboration, are rejected. No native credential material is stored or returned. Missing profile IDs return 404; duplicate names/capacity return 409; invalid input returns 400; unreadable/corrupt/unsupported profile storage returns 503 rather than being overwritten or silently ignored. Profile changes do not write Room events or reconfigure existing Rooms.

To create a Room using a saved pair, omit `agents` and include:

```json
{"agent_pair_profile_id":"pair-id","bindings":{"claude":{"mode":"new"},"codex":{"mode":"new"}}}
```

Omit both `agents` and `agent_pair_profile_id` to use the saved default or Service defaults. Explicit full `agents` always wins by being the only selection source; do not send a profile ID alongside it. Profile contents are copied once, not linked to the Room.

## Room names and native session correspondence

Omitting `name`, or supplying only ordinary spaces, generates `Room-<short Room ID>` once during the creation transaction. The returned `Room.name` and Event Log contain that same value. No model call or client-side naming authority is involved. Supplied names are trimmed, at most 160 UTF-8 bytes, and must not contain control characters. Invalid names return HTTP 400 before provisioning or draining a runtime.

`PATCH /api/v1/rooms/{room}` accepts only `{"name":"New name"}`. The existing safe-boundary path waits for active work to finish, suspends the runtime, then appends `service.room.renamed` before updating the registry. It never interrupts a Turn. A blank rename is rejected; an unchanged name is a no-op and does not suspend the runtime or append another event. Room ID, data directory, native session IDs, Bindings, collaboration, permissions, and message history do not change. The endpoint uses the normal Management authentication/CSRF boundary.

Each Management Room projection includes `runtime_names`, keyed by the stable `claude` / `codex` slots. Values are desired names, computed from `<Room name> · <current @handle> · <short Room ID>`; they do **not** prove native synchronization. Duplicate runtimes keep their `0`/`1` handle suffixes. Long display names are shortened to fit the shared 100-Unicode-scalar title budget while preserving the handle and ID suffix. Short IDs are display hints, never lookup keys.

Participant `runtime` adds `session_name` (desired native title) and `session_name_status`:

| Status | Meaning |
|---|---|
| `pending` | Native session naming has not completed |
| `configured` | Claude's advertised `--name` option was supplied at launch; not a separate title-read acknowledgement |
| `synced` | Codex/Grok acknowledged their metadata rename request |
| `unsupported` | The CLI did not expose the naming option/method |

Participant `runtime` also carries `provider` — the internal Provider label, either `native` or the reference form `cc-switch:<app_type>/<profile_id>` — and, for a CC Switch Profile resolved by this release or later, `provider_name`: the Profile display name, at most 160 UTF-8 bytes, without control characters, and credential-redacted. Browser UIs display `provider_name` and keep the raw label in a tooltip; the label is a stable machine reference, never a lookup key or a display name. Events recorded before `provider_name` existed omit it, and a UI then falls back to the label. `effort` mirrors the creation-time selection and is empty when the slot inherits the native default.
| `failed` | Naming failed; the original session remains usable, with retry on next activation |
| `simulated` | Mock runtime only; no native title was changed |

Native titles are applied on activation/session opening, including permission-driven restarts, only after the Room binding exists. Provisioning validators do not rename sessions. Dormant or archived Rooms do not spawn a native process merely to rename one: their native titles catch up when next activated. An active Room rename uses the safe-boundary suspension above; reactivate normally afterward. Names use native metadata, not prompts or direct writes to vendor session storage. See [Architecture](ARCHITECTURE.md#room-and-native-session-names) for adapter support.

## Creation-only collaboration and independent permissions

Room creation also accepts optional `collaboration`. Omission or `{"mode":"default"}` persists canonical version-2 flexible Lead/Executor instructions. Custom input is, for example:

```json
{"collaboration":{"mode":"custom","instructions":"Agent 2 plans. Agent 1 implements. Both challenge unsupported assumptions."}}
```

The server trims outer whitespace and requires non-blank UTF-8 without NUL, at most 16 KiB. Only `default` and `custom` are accepted. Default prose cannot be overwritten; choose custom instead. The response includes `{version, mode, instructions}` and the same record is stored in `room.created` and provisioning schema 3. PATCH does not accept mode or instruction changes. Old provisioning 1/2 Rooms have no collaboration record and keep their legacy behavior.

Participant snapshots add `responsibility` (`lead`, `executor`, or generic `participant`) and `permission_profile`. Modern `role` remains `peer` solely for old response readers; it is not a collaboration selector. Runtime policy fields describe the effective native policy.

`PUT /api/v1/participants/{actor}/permissions` accepts `{"profile":"configured"}`, `{"profile":"read-only"}`, or `{"profile":"yolo"}`. Configured restores the creation-time Agent policy, read-only uses native plan/read-only restrictions, and YOLO requests bypass/full access. The Room rejects pending Turns, queued input, pending approvals, and legacy Rooms. Invalid profiles return 400; unsafe transitions or runtime failures return 409. On success, read a fresh snapshot. `participant.permissions.requested` records intent before process effects; `participant.permissions.updated` commits effective policy after stopping the old process. A restart failure leaves the committed policy, not a broader fallback.

Permissions are not collaboration modes. They do not change the saved instructions, Runtime, Provider, model, or exact materialized session. The former `/participants/{actor}/role` route is removed (404). `target_role` submissions are rejected (400); choose one stable `to` slot or its exact displayed runtime handle. `@driver`, `@reviewer`, `@lead`, and `@executor` are not handle aliases.

## Errors

Error responses retain the English `error` field and add a stable `code`. Errors that can be safely localized may include `params` or `details`; these never contain Profile secrets. Clients localize recognized codes and display the original `error` for unknown or native diagnostics.

Status requests return the current projection. Message submission records the user Message before driving native execution; its HTTP success is not proof of a completed Turn. Judge execution from message processing, Turn summary, or SSE events. Other action receipts follow their endpoint contract: an approval resolution, for example, is recorded after the adapter response succeeds.

`POST /api/v1/messages` accepts one starting Agent and an optional `intent` of `steer` or `queue`; omission defaults to `steer`. Removed intent values and removed Room settings are rejected by the strict request decoder. Participant snapshots expose the stable slot `id`, runtime-derived `display_name`, and exact `mention_handle`. `PUT /api/v1/settings` currently accepts only `stall_warning_seconds`.

## SSE and reconnect

Durable events carry a monotonic sequence and can be resumed after disconnect. High-frequency text delta / command output and other transient telemetry may be non-persistent; token-by-token replay is not guaranteed after disconnect. After reconnect, clients should fetch a snapshot again, then continue from the durable sequence. The Room browser closes its obsolete stream and coalesces concurrent snapshot requests; failed reads use bounded backoff. It does not automatically retry message submissions.

`GET /api/v1/snapshot?message_limit=250` returns the newest messages and `message_window` pagination metadata while retaining current Room/runtime state. `message_limit` accepts integers from 0 to 1000; zero or omission retains the legacy full-transcript response. Invalid values return HTTP 400. Older messages are available through `GET /api/v1/messages?before_seq={oldest_seq}&limit=100`, in chronological order and strictly before the cursor.

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
<details>
<summary>Show current registered methods and routes</summary>

- `/api/v1/rooms/{room}/surface`
- `/api/v1/rooms/{room}/surface/{path...}`
- `DELETE /api/v1/agent-pair-profiles/{profile}`
- `DELETE /api/v1/attachments/{id}`
- `DELETE /api/v1/projects/{project}`
- `DELETE /api/v1/rooms/{room}`
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
- `GET /api/v1/service`
- `GET /api/v1/session`
- `GET /api/v1/snapshot`
- `PATCH /api/v1/agent-pair-profiles/default`
- `PATCH /api/v1/navigation-order`
- `PATCH /api/v1/rooms/{room}`
- `PATCH /api/v1/runtime-policy`
- `POST /api/v1/agent-catalog/refresh`
- `POST /api/v1/agent-pair-profiles`
- `POST /api/v1/approvals/{id}`
- `POST /api/v1/attachments`
- `POST /api/v1/diagnostics`
- `POST /api/v1/import`
- `POST /api/v1/maintenance/room-deletions/retry`
- `POST /api/v1/messages`
- `POST /api/v1/messages/{id}/cancel`
- `POST /api/v1/messages/{id}/retry`
- `POST /api/v1/participants/{actor}/{action}`
- `POST /api/v1/projects`
- `POST /api/v1/projects/{project}/refresh`
- `POST /api/v1/projects/{project}/rooms`
- `POST /api/v1/rooms/batch-archive`
- `POST /api/v1/rooms/batch-delete`
- `POST /api/v1/rooms/{room}/activate`
- `POST /api/v1/rooms/{room}/archive`
- `POST /api/v1/rooms/{room}/bindings`
- `POST /api/v1/rooms/{room}/open-browser`
- `POST /api/v1/rooms/{room}/restore`
- `POST /api/v1/rooms/{room}/suspend`
- `POST /api/v1/session`
- `PUT /api/v1/agent-pair-profiles/{profile}`
- `PUT /api/v1/participants/{actor}/permissions`
- `PUT /api/v1/settings`

</details>
<!-- /generated:routes -->

## Client compatibility principles

1. Tolerate added fields in JSON responses; send only documented request fields, because request decoding rejects unknown fields;
2. Do not derive the state machine from UI copy;
3. Re-read the projection after a destructive operation;
4. Do not treat a transient event as a durable receipt;
5. Read [Upgrading](UPGRADING.md) before a release upgrade.

## Explicit Service diagnostics

`POST /api/v1/diagnostics` is protected by the existing Management authentication, same-origin, and browser CSRF checks. There is no GET-triggered probe. An environment request is `{"mode":"environment"}`; an actual model check requires `{"mode":"runtime","actor":"codex","confirm":true}`. `actor` is the stable slot (`claude` or `codex`), not a Runtime kind. Both modes optionally accept `room_id`; otherwise the saved default Agent pair profile or Service defaults are used. A Room's stored selections never follow later default changes. Legacy Rooms without explicit selections cannot be live-tested through this endpoint.

Only those fields are accepted: clients cannot submit executable paths, environment, credentials, session IDs, or replacement selections. Invalid requests return 400; missing Rooms return 404; concurrent diagnostic requests and unsupported legacy live checks return 409. Checks are single-flight per Service, have bounded deadlines/output, and honor request cancellation. A failed check is evidence in an HTTP 200 report, not an HTTP transport failure. Responses use `Cache-Control: no-store`.

The response is `{schema:1, version, platform, generated_at, mode, scope, checks:[...]}`. Scope is `service_defaults`, `default_profile`, or `room`; no identity/path is exported. Checks contain `id`, `status` (`pass`, `warn`, `fail`, or `skipped`), a fixed `code`, `duration_ms`, and optional Runtime/slot/numeric version. Installation, native startup, and matching completed model response are distinct evidence. Unselected missing CLIs warn; Mock and untested model responses remain skipped. Cleanup warnings do not invalidate a received response, but must be resolved before repeatedly starting new checks. Raw errors and process/model output are never part of this report.

Diagnostics create no durable Room events, do not activate/suspend/resume existing Rooms, and never change native login or CC Switch configuration. A live check can use Provider quota and invoke native global hooks/MCP while starting its disposable session; it is not a sandbox, repository-specific smoke test, or exhaustive tool test. See [CLI reference](CLI_REFERENCE.md#installation-versus-runtime-availability) for operational boundaries.
