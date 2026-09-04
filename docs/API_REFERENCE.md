# HTTP and event API reference

PairRoom's browser UI uses a local HTTP API and SSE. CLI / UI is the preferred entry. Clients that call the API directly should treat it as a local control plane that evolves with PairRoom releases, not as a permanently stable public SaaS API.

## Safety boundary

- Bind to loopback by default;
- Non-loopback deployments must configure a token and supply their own trusted network boundary;
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

## SSE and reconnect

Durable events carry a monotonic sequence and can be resumed after disconnect. High-frequency text delta / command output and other transient telemetry may be non-persistent; token-by-token replay is not guaranteed after disconnect. After reconnect, clients should fetch a snapshot again, then continue from the durable sequence.

## Current source route inventory

The following paths / prefixes are extracted from `internal/server/` and `internal/service/`. Dynamic IDs and method constraints follow the handler implementation and tests.

<!-- generated:routes -->
<details>
<summary>Show current routes and prefixes</summary>

- `/api/`
- `/api/v1/`
- `/api/v1/agent-catalog`
- `/api/v1/agent-catalog/refresh`
- `/api/v1/approvals/`
- `/api/v1/approvals/approval-1`
- `/api/v1/approvals/approval-1/extra`
- `/api/v1/attachments`
- `/api/v1/attachments/`
- `/api/v1/attachments/../../etc/passwd`
- `/api/v1/events`
- `/api/v1/events?token=secret`
- `/api/v1/export`
- `/api/v1/export?format=json`
- `/api/v1/export?format=json&include_events=1`
- `/api/v1/export?format=markdown`
- `/api/v1/git/diff`
- `/api/v1/git/status`
- `/api/v1/health`
- `/api/v1/import`
- `/api/v1/maintenance/room-deletions/retry`
- `/api/v1/messages`
- `/api/v1/messages/`
- `/api/v1/messages/message-1/cancel`
- `/api/v1/messages/message-1/retry`
- `/api/v1/messages?before_seq=%d&limit=4`
- `/api/v1/messages?before_seq=nope`
- `/api/v1/participants/`
- `/api/v1/participants/claude/interrupt`
- `/api/v1/participants/claude/role`
- `/api/v1/participants/claude/stop`
- `/api/v1/projects`
- `/api/v1/projects/`
- `/api/v1/rooms/`
- `/api/v1/rooms/batch-archive`
- `/api/v1/rooms/batch-delete`
- `/api/v1/rooms/{room}/surface`
- `/api/v1/rooms/{room}/surface/{path...}`
- `/api/v1/runtime-policy`
- `/api/v1/service`
- `/api/v1/service?token=management-secret`
- `/api/v1/session`
- `/api/v1/settings`
- `/api/v1/snapshot`
- `/api/v1/snapshot?message_limit=1`
- `/api/v1/snapshot?message_limit=3`
- `/api/v1/snapshot?token=secret`
</details>
<!-- /generated:routes -->

## Client compatibility principles

1. Ignore unknown JSON fields;
2. Do not derive the state machine from UI copy;
3. Re-read the projection after a destructive operation;
4. Do not treat a transient event as a durable receipt;
5. Read [Upgrading](UPGRADING.md) before a release upgrade.
