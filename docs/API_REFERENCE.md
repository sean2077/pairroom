# HTTP and event API reference

PairRoom's browser UI uses a local HTTP API and SSE. CLI / UI is the preferred entry. Clients that call the API directly should treat it as a local control plane that evolves with PairRoom releases, not as a permanently stable public SaaS API.

## Safety boundary

- Every built-in listener accepts numeric loopback addresses only. Wildcard, LAN, and hostname binds are rejected, not enabled by setting a token;
- For remote access, use SSH local port forwarding to the loopback listener and retain the normal browser-session or bearer authentication;
- The API must not return Provider secrets or absolute host paths of attachments;
- A destructive request must name the Project / Room identity and obey archive, active Turn, and Binding preconditions.

## Resource families

The Management API owns Project and Room registration, immutable per-Room Agent selection, Binding, archive, backup, and Service diagnostics. The Room API owns message, Turn, participant, role, approval, retry, cancel, attachment, and the event stream. In-app Room tabs use the Management same-origin surface: `/api/v1/rooms/{room}/surface/…` uses the Management Session, and the server injects the Runtime bearer. `PATCH /api/v1/runtime-policy` only adjusts the concurrent Runtime cap. `POST /api/v1/rooms/{room}/open-browser` opens the system browser after the Runtime is ready. An archived Room has no surface and cannot be opened externally.

## Agent catalog and Room creation

`GET /api/v1/agent-catalog` and `POST /api/v1/agent-catalog/refresh` return all three Runtime entries with availability diagnostics, sanitized CC Switch Profile summaries, local model suggestions, disabled reasons, and the current two Service defaults. The response never contains raw Profile configuration, endpoints, headers, tokens, API keys, or Runtime arguments. Refresh is explicit, and Room creation still re-resolves the selected Profile server-side instead of trusting the catalog returned to the browser.

`POST /api/v1/projects/{project}/rooms` accepts the existing `name` and complete two-slot `bindings` map plus an optional complete `agents` map keyed by historical ActorIDs `claude` and `codex`. Omitting `agents` snapshots both current Service defaults. Sending only one slot is rejected. A selection has this shape:

```json
{
  "runtime": "codex",
  "provider": {"source": "cc-switch", "app_type": "codex", "profile_id": "profile-id"},
  "model": "custom-model-id",
  "effort": "high",
  "instructions": "Review compatibility boundaries.",
  "permission_mode": "",
  "approval_policy": "on-request",
  "sandbox": "workspace-write",
  "ordinary_reviewer_policy": "enforced"
}
```

The created Room returns the immutable `agents` map. There is no Agent-reconfiguration endpoint. Schema-v1 Rooms instead return `legacy_defaults: true` and no `agents` map.

## Errors

Error responses retain the English `error` field and add a stable `code`. Errors that can be safely localized may include `params` or `details`; these never contain Profile secrets. Clients localize recognized codes and display the original `error` for unknown or native diagnostics.

Status requests return the current projection. Command requests first record an auditable event, then drive the native runtime asynchronously. HTTP success means only that the control plane accepted the command. Judge final execution from message processing, Turn summary, or SSE events.

`POST /api/v1/messages` accepts one starting Agent and an optional `intent` of `steer` or `queue`; omission defaults to `steer`. Removed intent values and removed Room settings are rejected by the strict request decoder. Participant snapshots expose the stable slot `id`, runtime-derived `display_name`, and exact `mention_handle`. `PUT /api/v1/settings` currently accepts only `stall_warning_seconds`.

## SSE and reconnect

Durable events carry a monotonic sequence and can be resumed after disconnect. High-frequency text delta / command output and other transient telemetry may be non-persistent; token-by-token replay is not guaranteed after disconnect. After reconnect, clients should fetch a snapshot again, then continue from the durable sequence. The Room browser closes its obsolete stream and coalesces concurrent snapshot requests; failed reads use bounded backoff. It does not automatically retry message submissions.

`GET /api/v1/snapshot?message_limit=250` returns the newest messages and `message_window` pagination metadata while retaining current Room/runtime state. `message_limit` accepts integers from 0 to 1000; zero or omission retains the legacy full-transcript response. Invalid values return HTTP 400. Older messages are available through `GET /api/v1/messages?before_seq={oldest_seq}&limit=100`, in chronological order and strictly before the cursor.

`GET /api/v1/events?since={latest_seq}` resumes after that durable sequence. A non-empty `Last-Event-ID` header takes precedence over `since` on native EventSource reconnects; malformed cursors return HTTP 400. If the cursor is ahead of the Room or older than its retained event tail, the server emits `event: reset` with `{"reason":"snapshot_required","latest_seq":...}` and closes the stream. Fetch a fresh snapshot before reconnecting; do not interpret this as a Turn completion. Transient events and reset notifications never advance the durable SSE ID.

## Current source route inventory

The following method/path patterns are extracted from production HTTP registrations in `internal/server/` and `internal/service/`, including named constants. Test URLs, query examples, and rejected path-traversal inputs are not API routes. Patterns without a method are the same-origin surface gateway; their allowed operations are enforced by its handler.

<!-- generated:routes -->
<details>
<summary>Show current registered methods and routes</summary>

- `/api/v1/rooms/{room}/surface`
- `/api/v1/rooms/{room}/surface/{path...}`
- `DELETE /api/v1/attachments/{id}`
- `DELETE /api/v1/projects/{project}`
- `DELETE /api/v1/rooms/{room}`
- `DELETE /api/v1/session`
- `GET /api/v1/agent-catalog`
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
- `PATCH /api/v1/rooms/{room}`
- `PATCH /api/v1/runtime-policy`
- `POST /api/v1/agent-catalog/refresh`
- `POST /api/v1/approvals/{id}`
- `POST /api/v1/attachments`
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
- `PUT /api/v1/participants/{actor}/role`
- `PUT /api/v1/settings`
</details>
<!-- /generated:routes -->

## Client compatibility principles

1. Tolerate added fields in JSON responses; send only documented request fields, because request decoding rejects unknown fields;
2. Do not derive the state machine from UI copy;
3. Re-read the projection after a destructive operation;
4. Do not treat a transient event as a durable receipt;
5. Read [Upgrading](UPGRADING.md) before a release upgrade.
