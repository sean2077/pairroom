# Architecture

## Goal

PairRoom is a local control plane above native Agent harnesses. It solves session binding, sequential scheduling, creation-time collaboration instructions, independent tool permissions, approvals, persistence, observation, and recovery. It does not reimplement the Claude Code, Codex, or Grok Build tool loops.

```text
Browser / Wails Desktop / CLI
              |
      HTTP + SSE control plane
              |
Service registry ---- Project / Room / Binding metadata
              |
Room Engine --------- append-only Event Log + projections
              |
      deterministic FIFO scheduler
              |
Agent 1 adapter      Agent 2 adapter
(slot `claude`)      (slot `codex`)
      |                  |
runtime claude,      runtime claude,
codex, or grok       codex, or grok
      |                  |
official native CLI / session / tools / approvals
```

Wails Desktop is only a native Window / Tray / single-instance host. It loads the same Management Shell and reuses the Go Service directly. It is not a new state owner.

## Slot identity vs runtime identity

Durable `ActorID` values identify the two Room slots: `claude` is Agent 1 and `codex` is Agent 2. Those IDs stay stable so existing Event Logs and Bindings remain valid when a slot switches runtime.

`RuntimeKind` selects the native harness bound to a slot: Claude Code, Codex, or Grok Build. Either slot may select any supported runtime, including the same runtime twice.

Every adapter must emit the configured slot actor on events. Never hard-code a vendor as the event actor. Persistence and Binding ownership stay on the slot, while public display names and `mention_handle` values derive from the current runtime assignment. Duplicate runtimes use stable slot-order suffixes `0/1`.

## Room and native session names

`Room.name` is mutable display metadata; `Room.id` and `(ActorID, vendor_session_id)` remain the durable identities. Creation generates a missing name from the Room ID before writing its facts. The existing `service.room.renamed` event owns subsequent names, so registry rebuild and engine replay agree. Runtime name maps are derived read-side projections, never an independent authority. Neither paths nor native identities are renamed.

The native name is `<Room name> · <@handle> · <short Room ID>`, limited to 100 Unicode scalars with the handle and suffix intact. The shared helper in `internal/model/names.go` is used by Management and native adapters. These labels do not enter collaboration instructions or per-turn envelopes. Startup and permission-replacement adapters receive the current durable Room ID/name; provisioning probes intentionally have no Room ID and cannot rename an existing session before binding it.

Claude uses the advertised `--name=<title>` launch option (one argument, including leading-dash names). Codex calls `thread/name/set` with `threadId` and `name` after exact thread creation/resume. Grok calls `x.ai/session/rename` with `sessionId`, `title`, and `cwd` after opening its native session; only method-not-found permits the older `_x.ai/session/rename` spelling. JSON-RPC naming waits have a two-second context deadline. A naming error is visible in RuntimeInfo, not a failed user Turn or a reason to change session identity. Unsupported Claude flags are omitted. No naming operation invokes a model or edits vendor-owned storage directly.

Room rename retains the single-writer lifecycle: close the mutation gate, wait for native work to settle without interrupting, suspend, then append the rename fact. Native metadata catches up on the next activation; a dormant/archived Room does not wake a vendor merely to update its title. This can replace a manually chosen title on a session explicitly bound to the Room. RuntimeInfo distinguishes a desired name, configured CLI argument, acknowledged metadata request, failure, and Mock simulation; it does not claim an unsupported CLI has renamed anything.

Upstream contracts: [Claude CLI reference](https://code.claude.com/docs/en/cli-reference), [Codex App Server](https://github.com/openai/codex/blob/6af345407d9c2a568da9d01b6c4b81a9e61495c0/codex-rs/app-server/README.md), and [Grok session administration](https://github.com/xai-org/grok-build/blob/72a61251fcffb464bcc687aeb5a998e5a98ec0c9/crates/codegen/xai-grok-shell/src/extensions/session_admin.rs). Adapter tests use deterministic protocol fixtures, not authenticated vendor E2E.

## State ownership

| State | Authority |
|---|---|
| Project / Room registration and Binding | Service registry and Room service events |
| Per-Room Runtime/Provider/model/policy selection | schema-v3 `service.room.provisioned` event (`Room.agents`; schema-v2 legacy selections remain readable) |
| CC Switch Profile contents and credentials | CC Switch schema-18 database; read afresh for creation validation and activation |
| Collaboration mode and versioned instructions | `room.created` and matching schema-v3 provisioning fact |
| Messages, approvals, effective permission profiles, FIFO delivery state, Turn summary | Room Event Log |
| Current native process / stdout / request ID | Agent adapter |
| live source tree | Shared live workspace for both modern participants, single native Turn owner |
| legacy review filesystem view | Preserved schema-9 Reviewer snapshot |
| Page display | Server projection; neither the browser nor the desktop webview is SSOT |
| native window, tray, and second-launch focus | Wails Desktop host |

UI, prompt, desktop shell, or in-memory cache must not override durable authority.

## Key invariants

### Single owner

A Room has at most one active native Turn owner. Cross-Agent messages and explicit `queue` intent enter the Room FIFO. Same-target `steer` asks the adapter to inject into the active native Turn; unavailable or rejected steering queues the same Message exactly once, while an unknown result fails for explicit Retry. An Agent reply creates another FIFO item only when its visible text contains the other participant's exact current handle. No mention ends relay.

### Diagnostic is not terminal

A Codex generic `error` notification can arrive before `turn/completed`. `RuntimeError` is therefore diagnostic, not a reason to release the owner automatically. On unexpected process exit, the adapter settles outstanding input first, then emits an explicit process-exit boundary.

### Cancellation is stage-aware

- In the FIFO: cancel only the target item;
- Scheduler has reserved, not yet submitted: the submission boundary checks cancellation again;
- Native runtime has accepted: the interrupt scope may widen to the current Agent Turn, but must not clear unrelated Room FIFO items.

### Event-before-effect

Control-plane facts that are user-visible and need audit should be written to the Event Log before driving external side effects. A vendor's temporary request ID is not a durable key across restarts.

### Desktop ownership is explicit

Desktop startup follows a single-owner decision: validated explicit Management URL → installed daemon (started and waited for by the desktop host when needed) → embedded in-process Service only when no daemon is installed. If a daemon is installed but unreachable, fail closed and do not start a competing instance. Reusing an external Service does not transfer ownership; desktop quit must not stop an external daemon. An embedded Service is owned by the desktop process and shuts down in Management shutdown → Runtime drain → Registry / lock release order. No path implicitly recovers a stale `service.lock`.

## Main modules

- `cmd/pairroom/`: CLI and startup assembly;
- `desktop/`: isolated Go 1.25 / Wails v3 module, native host and platform packaging only;
- `internal/service/`: Project / Room lifecycle, Binding, and runtime capacity;
- `internal/ccswitch/`: pinned, query-only CC Switch schema adapter and secret-safe process materialization;
- `internal/webui/`: shared embedded i18next, bilingual catalogs, locale formatting, and theme runtime;
- `internal/room/`: Event Log projection, single-owner scheduler, mention routing, and approvals;
- `internal/agent/`: Claude Code / Codex / Grok Build native protocol adapters; each adapter emits the configured slot actor;
- `internal/server/`: Management Shell, Room View, HTTP, and SSE;
- `internal/store/`: JSONL persistence;
- `internal/archive/`: archive / backup implementation;
- `internal/model/types.go`: cross-layer durable model.

`desktop/go.mod` isolates Wails and GUI dependencies. Both modules require Go 1.25. The root module admits only the pinned CGo-free `modernc.org/sqlite` dependency closure, enforced by a strict module allowlist; this keeps four-platform `CGO_ENABLED=0` CLI releases while enabling read-only CC Switch access. The desktop host must not copy the Management / Room frontend, and must not redo Service lifecycle or authentication in JavaScript.

## Runtime lifecycle

The Service can activate or reclaim a Room runtime according to capacity and idle policy. An in-flight Room HTTP request, including the long-lived `/api/v1/events` stream, is real use: idle suspend does not close a runtime a browser is still reading. That live request is not a Management tab identity; explicit suspend and capacity LRU still apply. Reclaiming a native process does not delete the Room. Reactivation restores the durable projection, session Binding, and Room-owned FIFO entries that never crossed a native boundary. A delivery persisted as `submitting` has unknown native ownership after a crash and fails for explicit Retry instead of being replayed.

Modern Rooms persist `default` Lead/Executor or `custom` instructions once. Both participants use the live workspace; responsibility never selects a sandbox. Native permission changes require both participants idle, an empty FIFO, and no pending approvals. The Room records intent before stopping the adapter, commits the effective profile only after stopping, and restarts with the same Provider/model/instructions and exact materialized session. Replacement is serialized with Room shutdown. Failures never fall back to broader policy. Legacy role/workspace facts are replayed without conversion; no public role-change route remains.

New Rooms snapshot two secret-free `AgentSelection` values. Native ProviderRefs inherit CLI user/global configuration; CC Switch ProviderRefs are resolved into an ephemeral child-process configuration at activation. Already active processes are not mutated when a Profile changes. Grok Build prompt and instruction text stay in a prompt file, never in process argv.

The provisioning event schema is version 3 and requires its collaboration record to match `room.created`. Its reader accepts schema 1 as `Legacy defaults` and schema 2 with its original Agent selections, without rewriting the Event Log or granting new permissions. New stores use schema 10; schema-9 stores remain readable without relabeling. Unknown newer provisioning schemas fail closed, so downgrade requires restoring the pre-upgrade data-root backup rather than allowing an old binary to reinterpret new Room facts. `service-registry.json` uses checkpoint schema 2 and remains a rebuildable index.

## Native interaction ownership

The Room Engine reserves an approval while its native response is being submitted, so concurrent browser/API decisions cannot answer the same request twice. Validation failure releases that reservation without resolving the durable request. Grok permission grants preserve exact advertised option identity and never broaden one-time authorization into a remembered grant. Transport failures are surfaced for explicit recovery, never automatically replayed.

## CC Switch boundary

PairRoom reads only CC Switch v3.20.1/schema 18. The database connection uses `mode=ro` and `PRAGMA query_only=1`; PairRoom never changes the current Profile or invokes CC Switch mutation paths. The safe public catalog contains Profile identity, display name, Runtime, local model suggestions, support state, and disabled reason. Raw `settings_config` and `meta` remain inside the mapper. Only directly materializable API-key configurations cross the boundary: secrets become environment entries for one target child, while safe Provider/model parameters may become CLI overrides. Missing, locked, malformed, deleted, unsupported, or version-mismatched state fails activation without fallback.

## Shared presentation preferences

Management, Room View, and Desktop startup load one embedded i18next 26.4.2 runtime and the same `en`/`zh-CN` semantic-key catalogs. Browser language chooses the first locale, `pairroom.lang` persists later choices, and English is the fallback. User content, host paths, and raw native Runtime output bypass translation. Theme selection uses the shared `pairroom.theme` `system|light|dark` value. Management broadcasts changes to embedded Room surfaces; a standalone Room retains its own control while an embedded Room hides it.
Empty Provider/model/effort/instructions and runtime-policy overrides inherit the selected native CLI's user/global configuration. Codex uses one long-lived App Server with `turn/start`, `turn/steer`, and `turn/interrupt`. Claude Code uses its long-lived streaming session but reports same-Turn steering unavailable. Grok Build uses one long-lived `grok --no-auto-update agent stdio` ACP process with exact session loading, native permission requests, cancellation, and the `x.ai/interject` extension (with a legacy `_x.ai/interject` fallback); PairRoom advertises `terminal=false` so it does not take over Grok's tool execution. Prompt and instruction text never enters Grok process argv.

## Web updates and native windows

SSE carries durable state events and transient telemetry. Pages should update incrementally or batch high-frequency activity, and must not rebuild the entire chat tree on every token. On reconnect, the snapshot is authoritative; leftover transient state in the browser or desktop webview must not be trusted. The Room client permits one snapshot read at a time, discards callbacks from obsolete streams, and preserves local composer edits during resynchronization. A bounded-tail replay gap produces an explicit snapshot-required reset rather than skipping durable events.

Read projections live in `internal/room/projection.go`: windowed snapshots slice before deep-copying messages; SSE copies only the retained event tail; runtime capacity/drain checks query activity under the Engine lock without cloning the transcript. These are read optimizations only: the complete Event Log, visible relay response, and native session context remain unchanged.

The composer has a single in-flight submission boundary, independent of the button DOM. IME confirmation and held Enter keys do not submit. Acceptance clears only the unchanged submitted draft and its attachments/reply; new edits survive. Failed mutations are never retried automatically. Browser draft storage is best-effort and is not an availability requirement.

Management coalesces ordinary polls, but a completed mutation invalidates older in-flight reads and waits for a post-mutation snapshot. Session generations prevent obsolete snapshot/catalog completions or 401 responses from replacing a newer browser session. These are ephemeral presentation guards, not durable state owners. Catalog refresh retains edits and preserves unavailable explicit Provider selections as invalid instead of silently falling back to native configuration. Browser-open readiness comes from the activation response; the server still owns creation and opening of the one-time URL.

Pending approval cards and open Room tabs are reconciled by identity instead of recreated during unrelated updates. Approval drafts remain local and are retained only for the same native request. Management removes archived/deleted Room surfaces from fresh snapshots and binds each surface message to its actual iframe's Room ID. `management.js` owns tab identity/state; `management-ux.js` owns keyboard/ARIA enhancements and reconciles them on `pairroom:tabs-updated`.

The Management Shell is Room-centric: the sidebar groups by Project, and in-app tabs embed an active Room View through the Management same-origin surface gateway (`/api/v1/rooms/{room}/surface/…`). The iframe uses the Management Session Cookie. The gateway injects the Runtime bearer on the server; the Runtime token never enters the DOM. An in-app tab is not a Runtime lease. A live Room HTTP/SSE connection delays idle suspend; background tabs without a live request still obey idle / capacity / LRU / explicit-suspend constraints; switching back to a suspended tab requests activation again. An archived Room cannot open as a tab; restore it first.

**Open in browser** waits until the Runtime is ready, then the Service opens a one-time Room Runtime URL with the system browser. It does not use `window.open`. The Wails host still keeps one main webview and blocks non-PairRoom `window.open` targets outside numeric loopback. Multiple windows are not a durable contract.

## Non-goals

- No distributed multi-node queue;
- No promise of automatic migration for arbitrary old Event Logs;
- No implicit broadcast, automatic A/B rotation, or hidden adapter queue;
- Do not hide native CLI permissions, approvals, or failures;
- Do not treat Mock E2E as a substitute for real vendor E2E;
- Do not maintain a second business UI, Service, or storage format for the desktop host.
