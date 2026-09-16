# Participant-slot ActorID migration: legacy `claude`/`codex` to canonical `slot1`/`slot2`

Status: draft v5 — owner decision required; nothing here is implemented.
Updated: 2026-09-16, after adversarial peer review round 4 (P0-6 restructured
as a real owner three-way choice F1/F2/F3; D strengthened; call-site-scoped
prohibition; vocabulary-selection gate; old-CLI upgrade-error contract).
Rounds 1–4 findings all incorporated; peer review verdict: v4 minus P0-6 is
an implementation blueprint.

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
  ActorID slot helpers. Enforcement is mechanical and **call-site scoped**:
  the prohibition test enumerates native files/constructors (internal/
  relay*, native_host_*, and named Service validators) — it must not
  blanket-ban packages like internal/service that also host embedded
  paths; shared validators (Room.Validate) gain an explicit vocabulary
  parameter and appear in both call graphs legitimately.
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

Round 4 proved no design-side closure exists: missing-archived recovery
(`recoverMissingArchivedRoomsFromCheckpoint`) sees only checkpoint Rooms;
checkpoint writes intentionally omit host mode; and in that branch the data
dir is missing by definition, so schema-12 metadata cannot be consulted.
Canonical and legacy Rooms are checkpoint-isomorphic — recovery cannot
distinguish them, and blanket rejection would break legacy recovery. F/G
are therefore a real owner three-way choice:

- **F1 (recommended): durable generation index.** A separate, strictly
  defined one-way index (`room_id → canonical`) written atomically at
  canonical Room activation — before it can be archived — living beside
  the Registry but explicitly NOT part of the rebuildable Registry or the
  checkpoint. Recovery rules: entry present ⇒ fail closed with actionable
  "restore from backup" text; entry absent ⇒ legacy recovery unchanged;
  index file missing or corrupt ⇒ all missing-archived recovery fails
  closed (integrity-first). Stale entries for fully deleted Rooms are
  harmless (the index only ever grows; an entry for a nonexistent Room
  matches nothing). Coexistence: the Registry remains fully rebuildable
  without the index for legacy Rooms; the index is the sole generation
  authority for recovery and is covered by backup/verify/restore.
- **F2: canonical Rooms opt out of checkpoint-only recovery.** Archive and
  delete APIs explicitly block the data-loss path for canonical Rooms or
  preserve generation evidence at archive time. Smaller durable surface
  than F1, but constrains the archive/delete lifecycle and still needs a
  place to keep the evidence — which tends to reimplement F1 scoped down.
- **F3: retire legacy missing-data recovery entirely.** Every missing
  archived Room fails closed. Simplest code, but a real behavior
  regression for legacy Rooms; cannot be described as "legacy unchanged".

G follows from F: with F1, checkpoint schema 2 stays strict and carries no
generation evidence (recommended); F2 needs an archive-side evidence
location; F3 needs none.

## Version skew and capability preflight (P1-12/P1-14; owner decisions B, I)

- **Capability contract**: the existing `/api/v1/service` snapshot (already
  read by `bind --create` preflight before any creation POST) gains an
  additive field `slot_vocabularies: ["legacy"]` or
  `["legacy","canonical"]`. Absent field ⇒ old Service ⇒ legacy-only.
  A new CLI refuses canonical create/bind **before any POST** when the
  capability is absent; the creation POST re-validates server-side
  (defense in depth); tampered/incorrect capability fails closed at
  creation with the schema error. Golden-tested both directions.
- **Vocabulary selection**: canonical is never implicit per-request. A
  Service-level setting (default off) enables canonical native creation;
  flipping it is the explicit irreversible confirmation of decision D
  (gated on a verified backup). `bind --create` selects canonical only
  when the setting is on and the capability field confirms support.
- **Old-CLI error contract**: an old CLI touching a canonical Room fails
  at its snapshot lookup with a distinct, actionable **upgrade error**
  (not a generic runtime-mismatch message), asserted by fixture.
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

Only Phase 2 writes canonical bytes, and only after the Service-level
irreversible confirmation (decision D gate: verified backup + explicit
opt-in). Revertibility ends at the first canonical Room (downgrade floor:
binaries without schema-12 readers fail closed on it; release notes mark
the floor). Phases 0/1 revert freely.

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
- Recovery: per the F choice — F1: generation index write-before-archive
  ordering, entry-present fail-closed with backup text, entry-absent
  legacy recovery unchanged, index missing/corrupt fails all
  missing-archived recovery closed; legacy recovery fixtures unchanged.
- Capability/vocabulary selection: setting off ⇒ create stays legacy even
  on a capable Service; setting on + capability ⇒ canonical; old-CLI
  upgrade-error text distinct from runtime mismatch (fixture-asserted).
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
- **D. Downgrade / point of no return** (strengthened per round 4): the
  first canonical Room requires an explicit irreversible Service-level
  confirmation gated on a **verified** backup (`pairroom backup` +
  `pairroom verify`), not merely docs advice; alternative: owner
  consciously accepts an unverified downgrade floor.
- **E. AgentPairProfile**: schema 1 legacy keys remain the durable form,
  normalize-on-read (recommended) vs profile schema 2.
- **F. Canonical archived missing-data recovery** (real three-way choice,
  no design-side closure exists): F1 durable one-way generation index
  (recommended, with integrity-first fail-closed rules) / F2 canonical
  Rooms opt out of checkpoint-only recovery with archive-side evidence /
  F3 retire legacy missing-data recovery entirely.
- **G. Checkpoint generation evidence**: none — schema 2 stays strict;
  generation authority lives outside the checkpoint per the F choice
  (F1: separate index).
- **H. Vocabulary module**: approve `RoomSlots` module + call-site-scoped
  prohibition of global slot helpers in native paths (recommended;
  implementability precondition).
- **I. Capability preflight contract**: additive `slot_vocabularies` field
  on the existing service snapshot, absent ⇒ legacy-only, fail-closed at
  preflight and creation; canonical creation additionally gated by the
  Service-level setting from decision D (recommended).

## Revision log

- v5 (2026-09-16): round-4 fixes — P0-6 restructured from an
  unimplementable fail-closed claim into owner three-way F1/F2/F3 with F1
  (one-way durable generation index, integrity-first recovery rules)
  recommended; D strengthened to explicit irreversible confirmation gated
  on verified backup; prohibition test scoped to native call sites with
  shared-validator vocabulary parameter; canonical creation gated by a
  Service-level setting; old-CLI canonical failure must be a distinct
  upgrade error; verification matrix extended.
- v4 (2026-09-16): explicit `RoomSlots` Vocabulary module (P0-5); canonical missing-data recovery
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
