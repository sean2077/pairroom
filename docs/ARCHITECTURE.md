# Architecture

PairRoom is a local coordination layer over native coding harnesses. It owns admission, native Turn ownership, message delivery, durable Room facts, and human controls. It does not replace the vendor's model/tool loop, session store, login, or permission system. [Why PairRoom](WHY_PAIRROOM.md) explains the product choice; [Concepts](CONCEPTS.md) explains the user-facing behavior.

## Components and ownership

```text
Desktop host or browser
        |
Management Service ---- Project registry / Room runtime manager
        |
Room HTTP/SSE surface ---- Room Engine ---- native adapters
        |                       |           Claude / Codex / Grok
   derived UI state             |
                         append-only Event Log
```

| Component | Source | Owns |
|---|---|---|
| CLI | `cmd/pairroom/` | Entry points, configuration loading, diagnostics and lifecycle commands |
| Management Service | `internal/service/` | Canonical Projects, Room provisioning, registry/index, Binding ownership, runtime capacity, Management API and gateway |
| Room Engine | `internal/room/` | Admission, one native Turn owner, durable FIFO, approvals, state transitions and projections |
| Native adapters | `internal/agent/` | Vendor process/session transport, typed submission/steering results, native events and permission requests |
| Protocol and prompts | `internal/protocol/`, `internal/prompt/` | Versioned collaboration contract, stable instructions, dynamic envelopes |
| Configuration and selection | `internal/config/`, `internal/model/`, `internal/ccswitch/` | Strict configuration, durable Agent selection, read-only supported external Provider resolution |
| Persistence and media | `internal/store/`, `internal/attachment/`, `internal/archive/` | JSONL integrity/replay, verified attachment metadata/bytes, bounded backup/restore |
| Room API and UI | `internal/server/`, `internal/webui/` | HTTP/SSE, authentication, shared assets and client projections |
| Workspace boundary | `internal/workspace/` | Live workspace and preserved legacy Reviewer snapshot behavior |
| Desktop | [desktop module](../desktop/README.md) | Native window/tray/login registration and platform packaging over the same Service |

The root module is Go 1.25 with the pinned CGo-free SQLite closure for CC Switch access. Wails and GUI dependencies stay in the isolated desktop Go module. Desktop is not a second backend or a separate copy of the product UI.

## Durable facts and derived state

Agent pair profiles are separate Service-scoped user configuration. `agent-pair-profiles.json` owns the named pairs and default ID; the rebuildable `service-registry.json` does not. Creation resolves and copies a pair, then follows ordinary Provider validation and immutable Room persistence. Profile updates/deletion never mutate Rooms.

The Event Log is authoritative for a Room. Registry records/indexes enable Service discovery and ownership checks; browser snapshots and Turn summaries are projections, not alternative stores of truth. High-frequency transient telemetry can remain off disk, but auditable state transitions must be persisted before they are published as facts.

Event sequences begin at 1 and remain contiguous. Room activation/lifecycle operations must validate the existing published Room identity before repair or new writes. A missing or empty log is not a fresh version of that Room. An ambiguous append failure closes the writer rather than continuing with uncertain sequence state. Only an incomplete final record is eligible for tail repair; middle corruption is not skipped.

Current writers use Store schema 10 and modern provisioning schema 3. Schema-9 Rooms and provisioning-1/2 records retain their original policy and identity semantics; they are not relabeled or silently broadened. [Storage](STORAGE.md) owns replay details and [Upgrading](UPGRADING.md) owns compatibility/rollback actions.

## Service, Project, Room, and Binding

A Project is a user-selected canonical Git root, not a copied checkout. Room provisioning is built privately and published only after it is complete. A data root has one Service writer, protected by `service.lock`; this is distinct from each Room's native Turn ownership.

The Service maintains global Binding ownership by durable participant slot and native session ID. Archive does not release that ownership. An existing Binding must resume exactly; a deferred new Binding materializes only after real native acceptance provides its identity. Checkpoint/event/uniqueness failures must fail closed instead of permitting two owners or substituting a session.

Historical slot IDs remain `claude` and `codex`; RuntimeKind is independent. Configuration, routing, permission translation, and resume behavior must use the selected Runtime rather than assume the old slot name identifies the vendor.

A modern Room does not automatically create an isolated task worktree. Its two participants use the live workspace. The one-native-Turn rule does not lock a repository against other Rooms, external editors, or native subprocesses. Independent writing tasks need separate workspaces and explicit integration outside that rule.

## Room and native session names

Room names are display metadata. Rename uses the existing safe suspension boundary; it does not change IDs, Bindings, or collaboration. Native title synchronization occurs at activation where supported and reports configured/acknowledged/unsupported/failed state. Desired display metadata is not proof of vendor acknowledgement. Claude supplies its advertised `--name` launch option and reports configuration, not a separate title-read acknowledgement; Codex/Grok use native metadata acknowledgement. Naming never requires a model Turn or direct vendor-store edits. See [API reference](API_REFERENCE.md#room-names-and-native-session-correspondence).

## Admission and native Turn ownership

The Room owns the only coordination FIFO. Adapters must not create an additional hidden queue. At most one participant owns a native Turn in a Room; another participant cannot start until a reliable native terminal boundary or confirmed exit releases that owner.

A Message is not a Turn. Multiple accepted same-target inputs may belong to one native Turn. Default `steer` intent attempts typed native steering only when appropriate; unsupported or rejected steering queues that input once. An unknown native result is not equivalent to “not submitted” and must fail visibly rather than being queued again. Explicit `queue` and cross-Agent input wait for the boundary.

Cancel precisely removes waiting Room work. After native acceptance, interruption may affect the entire native Turn. Newer human instructions invalidate stale not-yet-started Agent relays without pretending they can undo accepted side effects. Retry creates a new auditable Message; concurrent pending retries of the same source/participant are rejected.

A generic runtime diagnostic, quiet stdout, or a transport receipt is not terminal proof. Late events and callbacks must remain tied to the original native request/session/Turn identity and cannot reclaim or complete a newer owner. Stall warnings are observations, not an automatic permission to release ownership.

## Agent relay and instructions

New Rooms persist default Lead/Executor instructions or user-supplied custom instructions at creation. The stable native instruction layer owns participant identity, exact handles, collaboration responsibilities, and versioned protocol rules. Additional participant instructions remain separate explicit configuration.

The dynamic input envelope carries sender, complete body, and attachment metadata. Correlation IDs remain in transport/persistence where applicable rather than being repeated as model-facing context. Relay forwards the complete visible peer reply and its attachments; it does not summarize the reply or append accumulated Room history.

Only an exact current peer handle in visible output requests relay after the native Turn boundary. No such handle ends relay. There is no counter-based relay ceiling, and default/custom instructions are not executable workflow phases or enforced human plan-approval gates. [Protocol](PROTOCOL.md) owns matching exclusions, aliases, `@user` precedence, and envelope budgets; do not duplicate that parser contract here.

## Native adapters

| Runtime | Transport and boundary |
|---|---|
| Claude Code | Long-lived stream-json/control transport with native initialize, tools/questions, permissions and exact session handling. Current steering reports unavailable rather than pretending input was accepted. |
| Codex | Long-lived app-server transport; native Turn start/steer/completed state, permission requests, and thread identity. Generic `error` diagnostics alone do not release the owner. |
| Grok Build | Long-lived ACP stdio via `grok --no-auto-update agent stdio`; session/Turn operations and the supported interjection extension. Prompt/instruction content travels over ACP, not a prompt-file or argv transport. |

For a new Grok session, rules travel through `_meta.rules`; for an exactly loaded session, current bootstrap is supplied once in its first PairRoom prompt rather than replacing the native system prompt. PairRoom advertises `terminal=false`, retaining native Grok tool execution. Unsupported high-privilege reverse requests fail closed.

Adapters retain native authority and do not guarantee every interactive feature is available through headless transport. Empty explicit overrides must retain native inheritance. Supported CC Switch Profile references are resolved at creation and activation without changing the external current Profile. Credentials belong only in the child environment, not argv, Room history, browser payloads, or a second PairRoom secret store. Resolution failures cannot silently fall back to another Provider.

See [Configuration](CONFIGURATION.md) for supported mappings and [Support](../SUPPORT.md#compatibility-policy) for verification after native CLI changes.

## Permissions and approvals

Lead/Executor are instruction responsibilities, not grants. Both modern participants use the live workspace and new Rooms default to YOLO. The independent permission profile maps to each selected Runtime's actual native policy.

A modern permission change requires an idle Room with no waiting work or pending approval. Record intent before effects, stop the old adapter before committing the effective policy, then start the replacement while retaining collaboration and native identity. A failed stop cannot grant a policy; a failed replacement cannot fall back to broader access.

Native approval requests retain their exact identity, advertised scope/options, and validation rules. Unknown high-privilege requests fail closed. Invalid answers do not consume the pending request. Stop/restart/interrupt and terminal lifecycle changes expire requests that cannot be safely reused; a stale browser response cannot authorize a different native request. [API reference](API_REFERENCE.md#native-approval-responses) and [Security](../SECURITY.md) own wire/security details.

Legacy role-bound Reviewer snapshots remain a compatibility boundary, not a new-Room mode or an OS security sandbox. Do not recreate public role switches or silently migrate their permissions.

## Restart, capacity, and shutdown

| Boundary | Required behavior |
|---|---|
| Queued before native submission | Rebuild Room FIFO order on restart |
| Native submission outcome unknown | Fail with explicit Retry guidance; never automatically execute again |
| Accepted but unfinished native input | Cancel without replay; inspect side effects before Retry |
| Pending connection-local approval | Expire, do not replay an old decision |
| Runtime cleanup uncertain | Retain its capacity claim rather than pretending it is suspended |
| Graceful Service shutdown | Stop Management mutations, drain admitted work/native Turns, then close stores and release ownership |

Runtime capacity limits active Rooms, not durable Room count. Idle reclamation frees processes without deleting history. It must not interrupt an active Turn merely to reclaim capacity. A browser disconnect or a hidden window is not proof that native work stopped.

Desktop ownership follows the same Service rules: explicit validated URL, installed daemon, or embedded Service when no daemon exists. Crash-stale lock recovery first proves the recorded PID is gone; live owners fail closed. Desktop never installs a daemon on launch. [Operations](OPERATIONS.md#desktop-lifecycle) owns user-visible close/quit/login behavior and operational commands.

## HTTP, browser, and privacy boundaries

All built-in listeners are numeric-loopback-only and validated before state is opened. Bearer tokens do not authorize LAN/hostname listeners. Browser bootstrap exchanges credentials for scoped sessions; mutations require the appropriate same-origin/CSRF checks. A Service Room gateway keeps its embedded surface on the Management origin without giving one Room another's credentials or identity.

SSE is a projection stream, not an execution queue. Clients reconnect using the actual event cursor and obtain a fresh snapshot after a bounded-tail `reset`; they must not replay commands to repair a display gap. Incremental rendering preserves drafts, focus, disclosure, and scroll state. Stale responses cannot overwrite a newer mutation's state or target another Room.

Room data, screenshots, and tool output may contain private source. Known Provider secrets are redacted at relevant boundaries, but arbitrary user/agent content still needs human review before export. Cloud inference follows the native CLI's Provider path. [Security](../SECURITY.md) owns the full threat/data model.

## Verification and change discipline

The architectural checks are state-transition tests, not persuasive prompt wording. Changes to ownership/recovery need successful, rejected, ambiguous, cancelled, restarted, and late-callback cases. Contract inventories must match source registrations and fields. UI fixtures, real-browser Mock Service tests, and authenticated native E2E prove different layers.

Use [Contributing](../CONTRIBUTING.md) for commands and PR evidence. This architecture describes current contracts; a historical plan, old validation JSON, or a successful Mock run cannot establish a new runtime capability.
