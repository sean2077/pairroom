# Architecture

## Goal

PairRoom is a local control plane above native Agent harnesses. It solves session binding, sequential scheduling, role isolation, approvals, persistence, observation, and recovery. It does not reimplement the Claude Code, Codex, or Grok Build tool loops.

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

Every adapter must emit the configured slot actor on events. Never hard-code a vendor as the event actor. Display names may show the selected runtime, but persistence, routing mentions (`@claude` / `@codex` / `@peer`), and Binding ownership stay on the slot.

## State ownership

| State | Authority |
|---|---|
| Project / Room registration and Binding | Service registry and Room service events |
| Per-Room Runtime/Provider/model/policy selection | schema-v2 `service.room.provisioned` event (`Room.agents`) |
| CC Switch Profile contents and credentials | CC Switch schema-18 database; read afresh for creation validation and activation |
| Messages, approvals, roles, Workflow, Turn summary | Room Event Log |
| Current native process / stdout / request ID | Agent adapter |
| live source tree | Driver workspace |
| review filesystem view | Reviewer snapshot |
| Page display | Server projection; neither the browser nor the desktop webview is SSOT |
| native window, tray, and second-launch focus | Wails Desktop host |

UI, prompt, desktop shell, or in-memory cache must not override durable authority.

## Key invariants

### Single owner

A Room has at most one active native Turn owner. Cross-Agent messages, explicit Agent peer mentions, and `next_turn` enter the Room FIFO. The scheduler submits the next item only after a reliable terminal boundary. When a human asks both Agents to interact, work still starts with the current Driver, who must give an explicit peer address. Implicit Agent relay without a direct peer mention still requires a valid `HANDOFF` and `NEXT`.

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
- `internal/room/`: Event Log projection, scheduler, Workflow, and approvals;
- `internal/agent/`: Claude Code / Codex / Grok Build native protocol adapters; each adapter emits the configured slot actor;
- `internal/server/`: Management Shell, Room View, HTTP, and SSE;
- `internal/store/`: JSONL persistence;
- `internal/archive/`: archive / backup implementation;
- `internal/model/types.go`: cross-layer durable model.

`desktop/go.mod` isolates Wails and GUI dependencies. Both modules require Go 1.25. The root module admits only the pinned CGo-free `modernc.org/sqlite` dependency closure, enforced by a strict module allowlist; this keeps four-platform `CGO_ENABLED=0` CLI releases while enabling read-only CC Switch access. The desktop host must not copy the Management / Room frontend, and must not redo Service lifecycle or authentication in JavaScript.

## Runtime lifecycle

The Service can activate or reclaim a Room runtime according to capacity and idle policy. Reclaiming a native process does not delete the Room. Reactivation restores the durable projection and session binding, but does not automatically replay the in-process FIFO.

Role / workspace switches must share the same safety boundary as delivery serialization, so a reviewer snapshot is not captured while the Driver is still mutating the live tree.

New Rooms snapshot two secret-free `AgentSelection` values. Native ProviderRefs inherit CLI user/global configuration; CC Switch ProviderRefs are resolved into an ephemeral child-process configuration at activation. Already active processes are not mutated when a Profile changes. Grok Build prompt and instruction text stay in a prompt file, never in process argv.

The provisioning event schema is version 2. Its reader accepts schema 1 as `Legacy defaults` without rewriting the Event Log. Unknown newer provisioning schemas fail closed, so downgrade requires restoring the pre-upgrade data-root backup rather than allowing an old binary to reinterpret new Room facts. `service-registry.json` uses checkpoint schema 2 and remains a rebuildable index.

## CC Switch boundary

PairRoom reads only CC Switch v3.20.1/schema 18. The database connection uses `mode=ro` and `PRAGMA query_only=1`; PairRoom never changes the current Profile or invokes CC Switch mutation paths. The safe public catalog contains Profile identity, display name, Runtime, local model suggestions, support state, and disabled reason. Raw `settings_config` and `meta` remain inside the mapper. Only directly materializable API-key configurations cross the boundary: secrets become environment entries for one target child, while safe Provider/model parameters may become CLI overrides. Missing, locked, malformed, deleted, unsupported, or version-mismatched state fails activation without fallback.

## Shared presentation preferences

Management, Room View, and Desktop startup load one embedded i18next 26.4.2 runtime and the same `en`/`zh-CN` semantic-key catalogs. Browser language chooses the first locale, `pairroom.lang` persists later choices, and English is the fallback. User content, host paths, and raw native Runtime output bypass translation. Theme selection uses the shared `pairroom.theme` `system|light|dark` value. Management broadcasts changes to embedded Room surfaces; a standalone Room retains its own control while an embedded Room hides it.

## Web updates and native windows

SSE carries durable state events and transient telemetry. Pages should update incrementally or batch high-frequency activity, and must not rebuild the entire chat tree on every token. On reconnect, the snapshot is authoritative; leftover transient state in the browser or desktop webview must not be trusted.

The Management Shell is Room-centric: the sidebar groups by Project, and in-app tabs embed an active Room View through the Management same-origin surface gateway (`/api/v1/rooms/{room}/surface/…`). The iframe uses the Management Session Cookie. The gateway injects the Runtime bearer on the server; the Runtime token never enters the DOM. An in-app tab is not a Runtime lease. Background tabs still obey existing idle / capacity / LRU / explicit-suspend constraints; switching back to a suspended tab requests activation again. An archived Room cannot open as a tab; restore it first.

**Open in browser** waits until the Runtime is ready, then the Service opens a one-time Room Runtime URL with the system browser. It does not use `window.open`. The Wails host still keeps one main webview and blocks non-PairRoom `window.open` targets outside numeric loopback. Multiple windows are not a durable contract.

## Non-goals

- No distributed multi-node queue;
- No promise of automatic migration for arbitrary old Event Logs;
- Do not turn the two Agents into an unbounded group chat;
- Do not hide native CLI permissions, approvals, or failures;
- Do not treat Mock E2E as a substitute for real vendor E2E;
- Do not maintain a second business UI, Service, or storage format for the desktop host.
