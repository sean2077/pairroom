# Participant-slot ActorID migration: legacy `claude`/`codex` to canonical `slot1`/`slot2`

Status: draft v2 — owner decision required; nothing here is implemented.
Updated: 2026-09-16, after adversarial peer review round 1 (two P0 blockers
and five P1 findings incorporated; see Revision log).

## Problem

`model.ActorID` persists `"claude"`/`"codex"` as the two participant-slot
identities (84 files, ~878 references). The CLI surface already treats
`--slot 1|2` as canonical and resolves slots zero-flag in recognized
sessions, but the durable vocabulary leaks:

- `ActorID.DisplayName()` renders slot 1 as "Claude Code" and slot 2 as
  "Codex" even when the slot runs a different runtime (live example: a room
  whose `claude` slot runs the codex runtime). This is an active mislabel.
- Store schema 11/provisioning 4 rooms, registry checkpoints, Event Log
  actors, HTTP paths `/api/v1/relay/{room}/{slot}/{action}`, per-slot
  credential/state directories, inbox keys and protocol v7 envelope senders
  all serialize the legacy strings.
- Embedded protocol v6 wire values, snapshots and browser assets hardcode
  the same strings; v6 must stay byte-identical.
- CONTEXT.md defines "Participant slot" as persisted ActorID `claude`/`codex`,
  codifying the debt in the glossary.

## Goals / Non-goals

Goals: one canonical in-memory vocabulary `slot1`/`slot2`; legacy values
accepted as read aliases for existing durable state indefinitely; display
never implies a runtime; embedded v6 bytes and registry checkpoint schema 2
provably unchanged; glossary, CLAUDE.md invariant text and docs updated in
the same change series.

Non-goals: rewriting any existing room bytes; changing runtime-derived
mention handles (`@claude`/`@codex`/`@grok`; note `@agent1` is a retired
mention alias and must not gain durable meaning); any embedded v6 wire
change; moving or rewriting existing credential/state files.

## Design

### One in-memory vocabulary, per-boundary serialization generations

- Canonical constants `ActorSlot1 = "slot1"`, `ActorSlot2 = "slot2"` are the
  only in-memory participant identity after `NormalizeActor()` runs at every
  ingress boundary (store load, event replay, registry read, checkpoint
  read, HTTP path/field parse, local state/credential resolution, config
  and profile loads). `ActorClaude`/`ActorCodex` become deprecated
  serialization aliases, not identities.
- **Per-Room durable vocabulary generation**: each Room records whether its
  durable streams are `legacy` (schema ≤ 11) or `canonical` (schema 12).
  Legacy rooms append legacy actor values to their append-only Event Log
  forever; canonical rooms append canonical values. No events.jsonl ever
  mixes vocabularies.
- **Embedded v6 adapter (P0-1 resolution)**: embedded serialization
  boundaries (v6 wire envelopes, snapshots, `durableRoom.Agents/Bindings`
  access in embedded runtime construction, contract golden output) keep
  legacy string values through an explicit translation layer at the
  serialization edge. v6 byte-identity is proven by golden tests, not
  assumed. Browser assets (`app.js`, `management.js` participant keys)
  accept both vocabularies; Management API responses for embedded rooms may
  keep legacy keys to avoid a JS cutover in Phase 1.
- **Registry checkpoint (P0-2 resolution)**: checkpoint schema 2 and its
  strict shape stay unchanged — the durable invariant admits no bump here.
  Checkpoints are a rebuildable compatibility projection: canonical
  in-memory state projects back to legacy keys on write, normalizes on
  read. Missing-archived-room recovery via checkpoint Rooms keeps working
  on legacy keys.

### Collision policy (fail-closed)

- Any map ingress (Room.Bindings/Agents/RuntimeNames, ProvisionRequest,
  AgentPairProfile, Message delivery/processing maps, snapshot
  participants, relay bindings/messages) must reject a dual representation
  (both `claude` and `slot1` for the same slot) before normalization —
  never first-wins, never silent overwrite.
- Local `.pairroom` state: binding State gains an explicit vocabulary
  field (State schema 2); slot directory discovery scans canonical and
  legacy locations and fails closed on coexistence for the same slot,
  protecting committed credentials. No file is ever moved or rewritten.
- Validation functions (Room.Validate, ProvisionRequest.Validate, registry
  validateProvisionedBindings) and `protocol --actor` validation move to
  canonical with legacy acceptance for existing durable state only; new
  canonical room/config writes reject legacy values.

### Wire, skew and process boundaries

- HTTP: server accepts both vocabularies in `{slot}` path segments
  (native_host_api bind/unbind/auth/park included) and JSON fields
  indefinitely; server responses for native rooms emit canonical values.
- CLI/service version skew: binding state records the actor vocabulary used
  at bind time and keeps using it on the wire (legacy binds stay legacy).
  New canonical binds run a service capability preflight; against an old
  service they fail closed with an actionable upgrade message. No implicit
  negotiation.
- Store schema: readers accept 10/11/12 with an explicit policy table
  (version.go), provisioning map gains 5, old binaries rejecting schema 12
  produce the existing unsupported-schema failure text plus a test.
- Protocol v7: sender field canonical for canonical-generation rooms,
  legacy for legacy rooms; readers normalize. Prompt byte-budget tests
  preserved.

### Display, glossary and docs

- Phase 0 does NOT rewrite `DisplayName()` globally (peer review: normal
  runtime UI already uses ParticipantIdentities runtime display; a global
  rewrite touches fallbacks/error paths without call-site tests). Phase 0
  ships docs plus a `SlotLabel` call-site audit; the actual display fix
  lands in Phase 3 behind that audit's tests — "Agent 1"/"Agent 2", with
  runtime names only from runtime metadata.
- CONTEXT.md "Participant slot" entry updated in the same change; legacy
  names move to its `_Avoid_` list (quotation/history/migration only).
- CLAUDE.md invariant rewording (owner approval): durable ActorID values
  are `slot1`/`slot2` identifying Agent 1/Agent 2; legacy `claude`/`codex`
  remain accepted read aliases for existing durable state; registry
  checkpoint schema 2 unchanged.
- Skill text: "durable IDs claude/codex remain accepted" becomes "accepted
  as legacy read aliases"; CLI_REFERENCE/API_REFERENCE/PROTOCOL/
  NATIVE_RELAY sweep.

## Phasing

1. **Phase 0**: docs + SlotLabel call-site audit only. Independently
   shippable; no behavior change.
2. **Phase 1 (read side)**: `NormalizeActor()` at all ingress boundaries,
   collision fail-closed, server alias acceptance, embedded legacy
   serialization adapter with v6 golden proofs, State schema 2 vocabulary
   field. No durable write vocabulary change; fully backward compatible.
3. **Phase 2 (write side)**: store schema 12/provisioning 5, per-Room
   vocabulary generation, canonical new native rooms, capability preflight
   for new binds, registry legacy projection retained.
4. **Phase 3 (cleanup)**: DisplayName fix behind audit tests, alias
   constant deprecation in canonical-only paths, glossary/CLAUDE.md final
   sweep, golden updates.

Each phase is separately mergeable; only Phase 2 writes canonical bytes and
it is revertible until canonical rooms exist in the wild.

## Verification matrix

- Legacy room fixtures (10/3 and 11/4): replay, repair, rebind, archive,
  restore, diagnostics — bytes preserved; active legacy Room appends keep
  legacy vocabulary after upgrade.
- Embedded: v6 byte-identical goldens; JS participant-key fixtures for both
  vocabularies; contract test output unchanged.
- New canonical room: end-to-end create/bind/FIFO/events/registry rebuild;
  browser shows "Agent 1/2" with runtime labels.
- All ActorID map collision matrix: dual representation rejected at every
  ingress (room, provision, profiles, messages, snapshots, relay state).
- Local directories: canonical/legacy coexistence fails closed; credential
  preservation; hook discovery and purge scan both vocabularies.
- AgentPairProfile config: legacy loads normalize; new saves canonical;
  dual-vocabulary file rejected.
- Checkpoint: schema 2 legacy projection round-trip; missing-archived
  recovery intact.
- Skew: old binary vs canonical state and new CLI vs old service as actual
  process fixtures with actionable failures.
- HTTP: every slot-bearing path (native bind/unbind/auth/park/wait/send/
  exchange/status/upload) in both vocabularies.
- Protocol prompt byte-budget; archive/restore event actor exact bytes.
- `make check`, `make smoke`, `make browser-check`; real vendor E2E remains
  a separate release gate (no synthetic claims).

## Resolved in v2 (peer review round 1)

1. `slot1/slot2` chosen over `agent1/agent2` (`agent1` is an existing CLI
   alias and `@agent1` a retired mention alias — no durable meaning reuse).
2. Registry checkpoint stays schema 2 with legacy projection (invariant
   admits no bump).
3. Skew strategy = bind-time vocabulary in state + capability preflight +
   per-Room generation (option b alone insufficient).
4. No standalone DisplayName rewrite; docs + audit first, fix behind tests.
5. Legacy aliases: read-permissive indefinitely for existing durable
   state; new canonical writes reject legacy; dual representation always
   rejected.

## Remaining owner decisions

1. Approve the overall approach (one in-memory vocabulary + per-boundary
   serialization generations) and phase gating.
2. Approve the CLAUDE.md invariant rewording when Phase 3 lands.
3. Confirm Phase 0 ships ahead of the rest.

## Revision log

- v2 (2026-09-16): incorporated adversarial review — P0-1 embedded v6
  vocabulary adapter (was contradictory "all new writes canonical" vs "v6
  untouched"); P0-2 checkpoint schema 2 retained as legacy projection (was
  unauthorized schema 3 bump); P1 collision fail-closed policy, State
  schema 2 vocabulary field, per-Room append vocabulary, capability
  preflight, Phase 0 scope reduction, expanded boundary inventory
  (agent-pair-profiles, state/hook paths, native_host_api PathValue,
  Management provision maps, protocol --actor, browser assets, version.go
  read policy) and verification matrix.
- v1 (2026-09-16): initial draft.
