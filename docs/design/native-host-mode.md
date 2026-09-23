# Native host mode

Status: implemented, experimental. This records design rationale, not a setup sequence or pending phase plan. Current contracts live in [Architecture](../ARCHITECTURE.md), [Protocol](../PROTOCOL.md#native-host-protocol-v8), and [Storage](../STORAGE.md#native-relay-state). [Native relay](../NATIVE_RELAY.md) owns the operator workflow.

## Decisions

Native Rooms relay between user-owned sessions; PairRoom never owns their model loop or launches/interrupts those processes. Host mode is immutable and separate from collaboration mode. Provider/model/effort/permissions are display-only. Owner Turn is advisory; each slot has a durable FIFO, not Embedded's single-owner scheduler.

Binding is explicit and in-session. It associates immediately from official tool-call identity, without a SessionStart/nonce association path. Missing/invalid identity, incompatible creator selection, or missing hooks fails before creation. Service defaults are read/pinned before preflight, while full creation validation stays server-side.

Approved Stop hooks confirm identity and may record a transcript reference without parsing it. A known bound session mismatch fails visibly; unrelated unbound sessions are inert. Only a routed Stop reply enters the Room as a complete body; an unaddressed boundary records a publication receipt. Publication and receive-side park are independent.

The documented identity surfaces are Claude Code's [environment contract](https://code.claude.com/docs/en/env-vars), Codex's [execution environment](https://github.com/openai/codex/blob/main/codex-rs/core/src/exec_env.rs), and Grok's tool `GROK_SESSION_ID` with camelCase hook fields. [Grok Native](../CLI_REFERENCE.md#grok-build-native) owns its pinned hook boundaries. References are not proof of compatibility with every installed version, resume/fork/child session, or actual response boundary.

The same-user threat model does not prevent intentional environment overrides or arbitrary private-file reads by that OS user. Discovery/lineage observations grant neither association nor native trust. [Workspace discovery](../NATIVE_SESSION_WORKSPACE.md) owns session-first lookup across cwd changes.

## Durable boundaries

Global `(runtime, session_id)` ownership includes duplicate-runtime slots, Embedded/Native collisions, and archived Rooms. Same-session bind recovery preserves generation; explicit unbind/replacement revokes credentials and invalidates old-generation work without stopping native side effects.

Private unconfirmed bind attempts remain outside active discovery. Failures preserve committed credentials/state; recovery reuses the original identity and promotion preserves advanced publication watermarks. Secrets stay in owner-only files, never stdout, argv, model context, or Room events.

Persist `delivering` before envelope release and acknowledge only after stdout. Ambiguous delivery is `unknown`: the original receipt may still settle it while no Retry is pending, otherwise inspection precedes explicit Retry. Atomically save report sequence/pending body and reconcile that identity. Stop and explicit send are independently auditable and can intentionally duplicate content; neither deduplicates by body.

Grok readiness feedback does not claim an envelope, and clipped replies require explicit full-text publication. No transcript mirroring, concurrent resume injection, or model-driven idle self-wake is introduced. Later [automatic wake](auto-wake.md) and [Claude inbox](claude-inbox-wake.md) add only a fixed Service nudge under durable reservation/rate/no-retry boundaries; submission is not model acceptance.

## Release acceptance

Regressions cover bind-to-send/wait/exchange, ownership, replacement/unbind, hook identity, creation preflight, FIFO/ack/restart, late receipt settlement, and skill freshness. Protocol/bootstrap and current strict storage formats remain specified by their owners rather than this record.

Report Mock, synthetic hooks/HTTP/SSE, browser fixtures, and authenticated vendor results separately. Current real multi-round Claude/Codex/Grok acceptance requires exercising and documenting the actual installed combination. Historical observations and source-level interface documentation do not satisfy that gate or establish billed-token savings.
