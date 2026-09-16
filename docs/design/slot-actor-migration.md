# Participant-slot ActorID migration: legacy `claude`/`codex` to canonical `slot1`/`slot2`

Status: draft v4 — owner decision required; nothing here is implemented.
Updated: 2026-09-16, after adversarial peer review round 3 (P0-5/P0-6 and
P1-12..P1-14 incorporated; Vocabulary module, capability preflight,
creation ordering and the serialization table added).

## Problem

`model.ActorID` persists `"claude"`/`"codex"` as the two participant-slot
identities (84 files, ~878 references). The CLI surface already treats
`--slot 1|2` as canonical and resolves slots zero-flag, but the durable
vocabulary leaks into store schemas, Event Log actors, HTTP paths
`/api/v1/relay/{room}/{slot}/{action}`, per-slot credential/state
directories, inbox keys, protocol envelopes, checkpoints, profiles and
browser assets. `ActorID.DisplayName()` actively mislabels slots whose
runtime differs. CONTEXT.md codifies the legacy values in the glossary.

## Scope decision (owner decision A — recommendation: native-only)

A global single in-memory vocabulary contradicts the frozen embedded v6
wire (round-2 finding): `durableRoom.Agents/Bindings` and `room.Engine`
maps are internal model access, not serialization edges.

- **Native-only (recommended)**: canonical `slot1`/`slot2` applies to new
  native Rooms (protocol v7 is additive). Embedded Rooms keep the legacy
  vocabulary end-to-end — in memory and on every wire — permanently; v6
  byte-identity needs no adapter. The vocabulary boundary rides the
  existing immutable `host_mode` gate.
- Global VocabularyCodec: rejected alternative — a full translation module
  at every embedded seam with v6 proof obligations; strictly more work and
  risk for no user-visible gain.

## Vocabulary module (P0-5 resolution; owner decision H)

Normalize-at-ingress alone is not an implementable contract: native relay
engine bind/replay/Peer/Send, the native host API path parser, native
snapshots and Service room/provision validators call the global
`ActorID.ValidParticipant/OtherParticipant/SlotActors` helpers directly.
Under a native-only migration these globals cannot serve both
vocabularies.

- Introduce an explicit vocabulary type and module (working name
  `model.RoomSlots`): `Vocabulary ∈ {legacy, canonical}` with
  `ParseSlot(vocab, input)`, `Other(vocab, slot)`, `SlotSet(vocab)`,
  `Member(vocab, slot)`, `Serialize(vocab, slot)`, `Normalize(input)`.
- The native relay Engine, native host API path parser, native snapshots
  and Service validators receive the Room's Vocabulary explicitly (plumbed
  from provisioning/generation) and are forbidden from calling the global
  ActorID slot helpers. Enforcement is mechanical: a repository test
  fails when native packages reference `ValidParticipant`,
  `OtherParticipant` or `SlotActors`; the globals remain only for embedded
  and legacy-vocabulary code paths.
- Dual representation (both `claude` and `slot1` for one slot) is rejected
  fail-closed at every map ingress before normalization. Never first-wins.
- Exclusions: `config.File` top-level `claude`/`codex` are runtime template
  names, not slot aliases — explicitly outside normalization. Mention
  handles are runtime-derived and untouched; `@agent1` stays a retired
  mention alias with no durable meaning.

## Room vocabulary generation: authority, creation order, serialization

- **Authority (P1-10)**: generation is durable per Room — store metadata
  schema 12 + provisioning 5 + native host mode ⇒ `canonical`; every
  schema ≤ 11 Room and every embedded Room ⇒ `legacy`. The room engine
  receives generation explicitly from provisioning config, never inferred
  after normalization.
- **Creation order (P1-13)**: `store.OpenNew(schema, provisioning)` is
  selected by ProvisionRoom from host mode + vocabulary policy **before**
  metadata is written; events appended afterwards carry that Room's
  vocabulary. `OpenExisting` reads metadata (10/11/12 policy table, with
  old-binary failure text and tests). New embedded Rooms keep writing
  11/4; the global `version.StoreSchema` bump to 12 raises only the
  native canonical creation path.
- **Append rule**: a legacy Room's append-only Event Log keeps legacy
  actor values forever; a canonical Room writes canonical values. No
  events.jsonl ever mixes vocabularies.

### Serialization table (wire generation per surface)

| Surface | Legacy native Room | Canonical native Room | Embedded Room |
|---|---|---|---|
| Store metadata | 10/3 or 11/4 bytes untouched | 12/5 | new writes stay 11/4 |
| Event Log actors (append) | legacy forever | canonical forever | legacy forever (v6 goldens) |
| Registry checkpoint (schema 2) | legacy keys | legacy projection (see recovery) | legacy keys |
| HTTP `{slot}` path segment | both accepted forever | both accepted forever | n/a (management surfaces legacy) |
| HTTP JSON writes (provision/bind) | legacy; canonical rejected | canonical only; legacy rejected | legacy |
| HTTP/snapshot responses | legacy projection | canonical projection + `slot_vocabulary` field | legacy projection |
| Binding state file | schema 1 frozen, never rewritten | schema 2 with vocabulary field | n/a |
| Credential/state directories | never moved; legacy path resolution | canonical path; scan fails closed on same-slot coexistence | n/a |
| AgentPairProfile file | legacy keys permanently | legacy keys permanently | legacy keys permanently |
| config runtime templates | excluded (not slot aliases) | excluded | excluded |
| Mention handles | runtime-derived, unchanged | unchanged | unchanged |
| Browser assets | accept both vocabularies | accept both; render "Agent 1/2" + runtime labels | accept both |

## Checkpoint recovery of canonical Rooms (P0-6; owner decisions F, G)

Checkpoint schema 2 and its strict shape stay unchanged (hard invariant);
checkpoint writes intentionally omit host mode, so a canonical archived
Room whose data dir is missing has no durable generation evidence in the
checkpoint. Options:

- **(b) Fail-closed recovery (recommended)**: registry recovery
  reconstructs legacy-vocabulary index entries only; a Room whose store
  metadata is schema 12 but whose data is missing fails recovery closed
  with actionable "restore from backup" text. No invariant change; matches
  the project's fail-closed philosophy; the scenario (archived + data dir
  lost) is already a backup-restore path.
- (a) Optional generation field in checkpoint schema 2: rejected —
  conflicts with the strict-shape invariant and old readers.
- (c) Encode generation into an existing invariant field: rejected without
  a safety proof; smuggling semantics into frozen fields is fragile.

## Version skew and capability preflight (P1-12/P1-14; owner decisions B, I)

- **Capability contract**: the existing `/api/v1/service` snapshot (already
  read by `bind --create` preflight before any creation POST) gains an
  additive field `slot_vocabularies: ["legacy"]` or
  `["legacy","canonical"]`. Absent field ⇒ old Service ⇒ legacy-only.
  A new CLI refuses canonical create/bind **before any POST** when the
  capability is absent; the creation POST re-validates server-side
  (defense in depth); tampered/incorrect capability fails closed at
  creation with the schema error. Golden-tested both directions.
- **Per-Room response projection**: legacy Rooms project legacy keys, so
  old CLIs keep working on them. Old CLI + canonical Room fails at the
  client-side snapshot lookup — early, actionable, with no bind side
  effect (asserted by test). New CLI + old Service is blocked by the
  capability preflight. Skew matrix documents "old CLI + canonical Room =
  unsupported, fails closed"; no response negotiation protocol.
- **Alias layering**: HTTP path segments accept both vocabularies forever
  (server normalizes to the Room's generation); JSON body map keys follow
  the Room's write vocabulary and reject the other. Path-alias and
  body-write rules have separate test suites.
- **Local state**: legacy State schema 1 files are never rewritten or
  silently upgraded; State schema 2 is produced only by canonical binds.
  Old CLIs rejecting schema 2 falls inside decision B's unsupported cell.
  Directory discovery scans both vocabularies and fails closed on
  same-slot coexistence; credentials are never moved.

## Display, glossary and docs

- Phase 0 does not rewrite `DisplayName()` (normal runtime UI already uses
  ParticipantIdentities; the fix lands in Phase 3 behind a SlotLabel
  call-site audit's tests — "Agent 1"/"Agent 2", runtime names only from
  runtime metadata).
- CONTEXT.md "Participant slot": canonical durable values `slot1`/`slot2`
  for canonical native Rooms; legacy values persist for embedded and
  pre-migration Rooms and move to `_Avoid_` for new-vocabulary contexts.
- CLAUDE.md invariant rewording (owner approval, Phase 3): durable ActorID
  values are `slot1`/`slot2` for canonical native Rooms; legacy
  `claude`/`codex` persist for embedded and existing Rooms as read
  aliases; registry checkpoint schema 2 and embedded v6 unchanged.
- Skill: "durable IDs claude/codex remain accepted" becomes "accepted as
  legacy aliases"; CLI_REFERENCE/API_REFERENCE/PROTOCOL/NATIVE_RELAY sweep.

## Phasing

1. **Phase 0**: docs + SlotLabel call-site audit only; independently
   shippable; no behavior change.
2. **Phase 1 (read side + module)**: `RoomSlots` Vocabulary module;
   native call sites migrated off global helpers with the prohibition
   test; ingress normalization and dual-representation rejection; HTTP
   path aliases; profile normalize-on-read with legacy-key saves; runtime
   template exclusion. No canonical writes; full legacy interop.
3. **Phase 2 (write side)**: `store.OpenNew` ordering with host-mode-aware
   schema selection (12/5 native, 11/4 embedded); canonical new native
   Rooms; capability field + preflight; State schema 2; per-Room response
   projection; checkpoint nested legacy projection + canonical
   missing-data fail-closed recovery; UI dual-vocabulary fixtures.
4. **Phase 3 (cleanup)**: DisplayName fix behind audit tests; alias
   constant deprecation in canonical-only paths; glossary/CLAUDE.md final
   sweep; golden updates.

Only Phase 2 writes canonical bytes. Revertibility ends at the first
canonical Room (downgrade floor: binaries without schema-12 readers fail
closed on it; docs recommend `pairroom backup` beforehand; release notes
mark the floor). Phases 0/1 revert freely.

## Verification matrix

- Legacy room fixtures (10/3, 11/4): replay, repair, rebind, archive,
  restore, diagnostics — exact bytes; active legacy Room appends keep
  legacy vocabulary after upgrade.
- Embedded: v6 byte-identical goldens; new embedded creation still writes
  11/4 (schema selector test); JS fixtures both vocabularies; contract
  output unchanged.
- Canonical native room: end-to-end create/bind/FIFO/events/registry
  rebuild; browser shows "Agent 1/2" with runtime labels.
- Vocabulary module: prohibition test (native packages never reference
  global slot helpers); ParseSlot/Other/SlotSet per generation; collision
  matrix — dual representation rejected at every ingress (room, provision,
  profiles, messages, snapshots, relay state, checkpoint nested round-trip
  of every ActorID position).
- Skew as real process fixtures: old CLI + new Service legacy Room (works);
  old CLI + canonical Room (fails at snapshot lookup, asserted no bind
  side effect); new CLI + old Service canonical create (preflight blocks
  before POST; server-side re-validation); old binary + schema 12 store
  (unsupported text).
- Capability field: absent/present/tampered golden fixtures both
  directions.
- Creation ordering: OpenNew schema selection before metadata; generation
  missing/tampered fails closed.
- Recovery: canonical archived missing-data Room fails closed with backup
  text; legacy recovery unchanged.
- Local directories: coexistence fail-closed; credential preservation;
  hook discovery and purge scan both vocabularies; State schema 1/2 real
  binary interoperability fixtures.
- Profiles: old service reads new-saved files and vice versa (legacy keys
  both ways).
- HTTP: every slot-bearing path in both vocabularies; path-alias vs
  body-write suites separate; protocol prompt byte-budget; archive/restore
  event actor exact bytes.
- `make check`, `make smoke`, `make browser-check`; real vendor E2E remains
  a separate release gate.

## Owner decisions (with recommendations)

- **A. Scope**: native-only (recommended) vs global VocabularyCodec.
- **B. Old CLI + new Service**: legacy Rooms supported; canonical Rooms
  require a current binary and fail closed early (recommended) vs full
  response negotiation (rejected: new protocol surface for transient skew).
- **C. Snapshots/responses**: per-Room projection (recommended) vs frozen
  legacy everywhere.
- **D. Downgrade**: first canonical Room = downgrade floor; backup advised
  in docs (as designed) vs stronger gates.
- **E. AgentPairProfile**: schema 1 legacy keys remain the durable form,
  normalize-on-read (recommended) vs profile schema 2.
- **F. Canonical archived missing-data recovery**: fail-closed + backup
  text (recommended) vs checkpoint field vs encoded evidence.
- **G. Checkpoint generation evidence**: none — schema 2 stays strict
  (recommended, follows from F).
- **H. Vocabulary module**: approve `RoomSlots` module + prohibition of
  global slot helpers in native paths (recommended; implementability
  precondition).
- **I. Capability preflight contract**: additive `slot_vocabularies` field
  on the existing service snapshot, absent ⇒ legacy-only, fail-closed at
  preflight and creation (recommended).

## Revision log

- v4 (2026-09-16): round-3 fixes — explicit `RoomSlots` Vocabulary module
  with call-site prohibition (P0-5); canonical missing-data recovery
  fail-closed, checkpoint untouched (P0-6); capability preflight contract
  on the existing snapshot read, blocking before any POST (P1-12);
  `store.OpenNew` creation ordering with host-mode-aware schema selection
  (P1-13); early side-effect-free old-CLI failure and path-alias vs
  body-write layering (P1-14); full serialization table; owner decisions
  F–I added.
- v3 (2026-09-16): scope narrowed to native-only; per-Room generation;
  State/profile/skew fixes from round 2.
- v2 (2026-09-16): round-1 fixes (checkpoint schema 2 retained, collision
  policy, Phase 0 reduction).
- v1 (2026-09-16): initial draft.
