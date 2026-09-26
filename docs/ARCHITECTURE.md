# Architecture

PairRoom coordinates native coding harnesses without replacing their model/tool loops, credentials, or session stores. Ownership depends on **Room host mode**, not whether the Management Service happens to be embedded in Desktop. [Concepts](CONCEPTS.md) defines user-facing behavior; this page owns component and state boundaries.

## Components and ownership

```text
Desktop or browser
        |
Management Service ---- Project registry / Room lifecycle / user preferences
        |
        +---- Embedded Room HTTP/SSE ---- Room Engine ---- native adapters
        |                                      |          Claude / Codex / Grok
        |                                Room Event Log
        |
        +---- Native HTTP/SSE ---- relay Engine ---- per-slot inboxes
                                       |                   |
                                 Room Event Log       CLI / approved hooks
                                                           |
                                                   user-owned sessions
```

| Component | Source | Owns |
|---|---|---|
| CLI | `cmd/pairroom/` | Entry points, configuration and lifecycle |
| Management Service | `internal/service/` | Projects, provisioning, Binding uniqueness, lifecycle/capacity, gateway and Native HTTP/SSE |
| Embedded Room Engine | `internal/room/` | Single Turn ownership, admission/FIFO, approvals and projections |
| Embedded adapters | `internal/agent/` | Vendor processes/session transport, typed steering/submission, native events |
| Native relay | `internal/relay/` | Durable publication, per-slot FIFO, receipts, generation authentication, audit and replay-built indexes |
| Native client/hooks | `internal/relayclient/` | Session/workspace discovery, private local state, publication reconciliation and collection |
| Wake and review evidence | `internal/claudewake/`, `internal/review/` | Private Claude inbox transport and bounded opt-in Git observations; neither grants execution authority |
| Protocol and prompts | `internal/protocol/`, `internal/prompt/` | Versioned contracts, stable instructions, dynamic envelopes |
| Configuration | `internal/config/`, `internal/model/`, `internal/ccswitch/` | Strict selection/configuration and read-only supported Provider resolution |
| Persistence/media | `internal/store/`, `internal/attachment/`, `internal/archive/` | JSONL integrity/replay, verified attachments and bounded backup/restore |
| API/browser assets | `internal/server/`, `internal/webui/` | Authentication, HTTP/SSE and shared client projections |
| Desktop | [separate module](../desktop/README.md) | Window/tray/login and packaging over the same Service |

The root module uses the latest stable Go release and the approved pinned CGo-free SQLite dependency closure. Wails/GUI dependencies stay in the separate desktop module. Desktop is not a second backend or product UI implementation.

## Durable facts and derived state

The Room Event Log is authoritative. Registry/indexes support discovery and ownership checks; snapshots, current-work counters, and Turn summaries are projections. Persist auditable transitions before publishing facts. High-frequency transient telemetry may remain off disk, and a projection may be persisted as bounded checkpoints rather than on every input ([Storage](STORAGE.md#event-log)).

Event sequences start at 1 and remain contiguous. Validate the existing published Room identity before repair or new writes; a missing/empty log is not a fresh Room. Ambiguous append failure closes the writer. Only an incomplete final record is repairable; never skip middle corruption.

Current readers require Store schema 12/provisioning 5 with immutable explicit `host_mode`. Registry checkpoint 3 requires canonical slots and host mode. Retired Service roots fail before recovery, replay, repair, or rewrite; do not infer missing Bindings, selections, or collaboration from current defaults. [Storage](STORAGE.md) owns formats and [Upgrading](UPGRADING.md) owns retirement/rollback.

Agent pair profiles and default ID live in `agent-pair-profiles.json`, separately from the rebuildable `service-registry.json`. Creation copies the pair; later profile edits/deletion do not mutate Rooms. Native selections remain display-only; Embedded Provider materialization retains its validation boundary.

Project/per-Project Room display order lives in `navigation-order.json`, not Room events or scheduling priority. Moves preserve hidden items and prune removed IDs without changing Project membership. Missing IDs use stable Registry creation order. Corrupt preferences are reported without overwriting the file or breaking ordinary Service discovery.

## Service, Project, Room, and Binding

A Project is a canonical Git workspace, not a copied checkout. Provisioning builds privately and publishes only when complete. `service.lock` protects one Service writer per data root, separately from Embedded Turn ownership.

Native Runtime/session identity is globally unique across Bindings, including archived Rooms. Embedded deferred new Bindings materialize only on real acceptance; existing Bindings must resume exactly, and a runtime that reports a different session during a bound Turn (for example in Claude Code `system/init` or `result`) fails that Turn and is stopped rather than replacing the bound ID. Native binds associate immediately from official tool-call session metadata, with generation-scoped credentials and later hook confirmation. Checkpoint/event/uniqueness failure cannot authorize a second owner or silently substitute a session.

Durable actors are `slot1`/`slot2`; RuntimeKind independently selects Claude Code, Codex, or Grok Build. Routing, policy projection, resume, and events must use that selection, not infer a vendor from a slot. `claude`/`codex` remain relay CLI input aliases only.

Neither host mode creates a task worktree or locks the repository against external editors, other Rooms, or native subagents. Native discovery follows the confirmed session binding across cwd changes; current tool paths and task checkout remain distinct. [Workspace discovery](NATIVE_SESSION_WORKSPACE.md) owns locators, cold lookup, conflict handling, and relative file paths.

## Room and native session names

Names are display metadata. Rename preserves IDs/Bindings and uses a safe lifecycle boundary without interrupting a Turn. Embedded activation projects supported native title metadata: Claude reports configured launch naming; Codex/Grok use their native acknowledgement paths. Desired/configured state is not acknowledgement. Never rename through a model Turn or direct vendor-store edit. Native Room rename does not take over the original process/title. [API naming](API_REFERENCE.md#room-names-and-native-session-correspondence) owns projections and statuses.

## Embedded admission and native Turn ownership

Embedded has one Room-owned coordination FIFO, not a hidden adapter queue. At most one participant owns a native Turn; another waits for a reliable terminal boundary or confirmed exit.

A Message is not a Turn. Several accepted same-target inputs can share one Turn. `steer` uses typed native results: unavailable/rejected input queues once; an unknown result fails visibly rather than being treated as not submitted. Explicit `queue` and cross-Agent input wait for ownership.

Cancel precisely removes waiting work. Interruption after acceptance may stop the entire Turn. Newer human instructions invalidate stale not-yet-started Agent relays, not accepted side effects. Retry creates an auditable new Message; concurrent pending retries for the same source/participant are rejected.

Diagnostics, silence, and HTTP receipts are not terminal proof. Late events/callbacks remain bound to their original native request/session/Turn and cannot release or complete a newer owner.

## Agent relay and instructions

Creation stores default/custom collaboration and its instruction version. Default responsibilities are flexible; custom prose replaces them. Activation must not rewrite a Room's policy. Stable instructions own self/peer identity and exact handles; dynamic envelopes carry sender, complete body, attachments, and explicit same-Room quoted context. Correlation IDs stay in transport/persistence; Agent-relay links are not recursively expanded into quotes/history.

An exact peer handle requests automatic relay; no peer handle ends it. Native additionally records no unaddressed Stop body in the Room; `@user` publishes a human result. Explicit Native send/exchange uses command targets, not body mentions, and may intentionally duplicate a later Stop publication. Keep full replies, not summaries or accumulated transcripts. [Protocol](PROTOCOL.md) owns matching exclusions, priority, aliases, and byte budgets.

There is no relay-count ceiling, enforced alternating sequence, or automatic plan-approval gate. Peer agreement is not authorization or test evidence.

## Native adapters

This section describes **Embedded adapters**, not control over Native-hosted sessions.

| Runtime | Transport and boundary |
|---|---|
| Claude Code | Long-lived stream-json/control transport, native initialization, tools/questions, permissions and exact session handling; steering reports unavailable rather than false acceptance |
| Codex | Long-lived app-server; native Turn start/steer/completed and thread identity; generic errors do not release ownership |
| Grok Build | ACP stdio via `grok --no-auto-update agent stdio`; supported interjection and native Turn/session operations; prompts travel over ACP, not argv/files |

New Grok sessions receive `_meta.rules`; exactly loaded sessions receive current bootstrap once in their first PairRoom prompt without replacing the native system prompt. PairRoom advertises `terminal=false` to retain native tool execution; unsupported privileged reverse requests fail closed.

Each adapter reads one stdout record at a time, up to 16 MiB for Codex and Grok Build and 8 MiB for Claude Code. A larger record or any other stdout read failure is fatal rather than skipped, because a dropped response or terminal cannot be recovered: the adapter reports `adapter.stream_error`, stops the vendor process tree, and its exit fails outstanding input and releases the Turn owner with that reason.

Empty overrides retain native inheritance. Supported CC Switch references resolve at Embedded creation/activation without modifying the external current Profile. Secrets enter only the selected child environment, not argv, stored selections, UI/logs, or a second secret store. Failures cannot fall back to another Provider. [Configuration](CONFIGURATION.md) owns mappings; [Support](../SUPPORT.md#compatibility-policy) owns compatibility evidence.

## Permissions and approvals

Embedded defaults both participants to YOLO, independently of Lead/Executor responsibility. An effective policy change requires both participants idle, no queue, and no pending approval. Persist intent before effects; stop the old adapter before committing policy, then replace it without changing native identity/collaboration. Failed stop/replacement cannot broaden access.

Approvals retain native request identity, scope, options, and validation. Invalid answers do not consume requests; stale browser answers cannot approve a different request. Connection/terminal changes expire requests that cannot be reused safely. Unknown privileged requests fail closed. The general adapter bridge may surface an interactive question it cannot answer natively to the human with `@user`, but it never substitutes for vendor approval semantics or leaves an unexposed native prompt. [API approvals](API_REFERENCE.md#native-approval-responses) and [Security](../SECURITY.md) own details.

Native-hosted approvals, permissions, steering, and interruption stay in the original harness. Stored model/Provider/effort/policy values are metadata only; no per-process policy injection or Provider materialization is attempted.

## Restart, capacity, and shutdown

| Boundary | Embedded | Native |
|---|---|---|
| Definitely queued | Rebuild the Room FIFO | Retain per-slot queued input |
| Uncertain submission/delivery | Fail for explicit inspection/Retry | Recovered `delivering` becomes `unknown`; an original matching receipt can settle it while no Retry is pending |
| Accepted input | Cancel unfinished input without replay | `handed_off` is terminal stdout evidence, not model completion; do not re-collect |
| Pending approval | Expire connection-local requests | Original harness owns it |
| Service shutdown | Drain owned native work before closing stores | Reject new publications/claims, permit valid released-envelope acknowledgements while draining; never stop user harnesses |

Capacity limits active **Embedded** adapters, not durable Room count. Idle reclaim cannot preempt an active Turn just to free capacity, and uncertain cleanup retains its capacity claim. Native Rooms are exempt: no vendor process, no capacity queue/slot, no capacity eviction. A browser disconnect or hidden window is not completion evidence.

An Embedded adapter owns its vendor process tree. On Windows each CLI runs in a kill-on-close Job Object, so stopping or interrupting a runtime launched through an npm `.cmd` shim ends the real CLI rather than only `cmd.exe`. Stop and Claude interrupt report success, and settle pending input, only after that tree has exited; a tree still holding its output pipes after a bounded wait is an uncertain stop that keeps the process recorded for a retried stop.

Desktop uses an explicit validated URL, installed daemon, or embedded Service when none exists. It never installs a daemon implicitly or competes with a live lock owner; stale recovery proves PID exit first. [Operations](OPERATIONS.md) owns close/quit/login/archive and backup behavior.

## HTTP, browser, and privacy boundaries

Validate numeric-loopback-only listeners before opening state. Bearer tokens do not allow LAN/hostname binds. Browser bootstrap exchanges credentials for scoped sessions; mutations retain same-origin/CSRF protection. The Management gateway does not expose another Room's credentials or identity.

SSE is a projection stream, not an execution queue. Reconnect from the actual cursor and refresh after a bounded-tail reset; never replay commands to repair a display gap. Incremental rendering preserves drafts, focus, disclosure, and reading position. Stale responses cannot overwrite newer mutations or target another Room.

Native Participants/conversation/Work inspector share these boundaries. Panels and closing a Room tab are view operations, not process/lifecycle changes. Pending/history are independent of the recent chat tail. Displayed selections, last activity, and wake outcomes remain observations, not live presence. Browser original-ID outbox recovery queries receipts without automatic resubmission; it is plaintext private-content storage, not a second server queue. See [Storage](STORAGE.md#native-current-work-and-browser-recovery-projections).

Known Provider secrets are redacted at relevant boundaries, but arbitrary user/Agent content, screenshots, and exports still require review. Cloud inference follows the native Provider path. [Security](../SECURITY.md) owns the threat/data model.

## Diagnostics and Project navigation

Management diagnostics are explicit authenticated operations, not another Room or automatic render-time test. Service diagnostics select stored Room or default-pair settings, gate concurrent checks, and allowlist output. Live tests use fresh native sessions in temporary Git workspaces, reject tool/approval interaction, require correlated completion, and consume no managed Room capacity. Native global hooks/MCP still apply; Mock is unverified. **Settings → Diagnostics** owns these checks and safe support export.

Native relay diagnosis is separate: it inspects active relay/binding/collector/wake observations without vendor calls or activating a suspended Room. CLI diagnosis also checks local hook/version/workspace evidence. Approval and model acceptance are unknown.

Project-name links navigate to Project pages; disclosure expands Rooms separately. Filters/search/archive visibility are Project-local. Shared Room context menus distinguish rename, close-view, ordering, and archive. View closure never appends a Room event or changes ownership/order. [API reference](API_REFERENCE.md) owns routes and mutation semantics.

## Native host mode

`internal/relay/` serializes durable appends before publishing projections. Each slot has its own FIFO; different original sessions may run independently. Binding generation authenticates publication/collection; unbind/replacement revokes old credentials without stopping accepted native work. Archive retains ownership and fails closed on missing Native data.

`internal/relayclient/` associates from `CLAUDE_CODE_SESSION_ID`, `CODEX_SESSION_ID`, or `GROK_SESSION_ID` at bind. Approved hooks re-confirm that identity and may record a transcript reference, but never parse vendor transcripts or implicitly rebind. Session/workspace hints grant neither trust nor file access. Private unconfirmed attempts cannot overwrite active credentials before confirmation. Same-user process access is outside the isolation claim.

Stop publication and receive-side park are independent. Local pending sequence/body is saved atomically and reconciled by its original key after ambiguous results. Collection persists `delivering` before stdout and acknowledges only after output; unknown delivery is not automatically replayed. Grok readiness feedback never contains a claimed envelope, and clipped replies require explicit full-text publication.

Optional wake is Service-authorized, body-free, rate-limited, durably reserved before effects, and never automatically retried after reservation. Unattempted deferred heads may be rechecked. Claude capability data remains in a private matching-generation sidecar; socket `submitted` is not model acceptance. Git review anchors are optional immutable evidence observations, not grants or workflow phases. [Protocol](PROTOCOL.md#native-host-protocol-v8), [Storage](STORAGE.md#native-relay-state), and the [design records](design/README.md) own detailed contracts/rationale.

## Verification and change discipline

Use state-transition tests for successful, rejected, ambiguous, cancelled, restarted, and late-callback paths. Inventories must match source registrations/fields. [Contributing](../CONTRIBUTING.md) defines commands and evidence layers. Native remains experimental: historical plans, UI fixtures, synthetic hooks, and Mock cannot establish current authenticated Claude/Codex/Grok multi-round acceptance or billed-token savings.
