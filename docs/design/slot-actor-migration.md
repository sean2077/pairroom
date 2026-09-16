# Participant-slot ActorID migration: legacy `claude`/`codex` to canonical `slot1`/`slot2`

Status: draft v3 — owner decision required; nothing here is implemented.
Updated: 2026-09-16, after adversarial peer review round 2 (P0-3/P0-4 and
P1-6..P1-11 incorporated; scope narrowed to native-only per review).

## Problem

`model.ActorID` persists `"claude"`/`"codex"` as the two participant-slot
identities (84 files, ~878 references). The CLI surface already treats
`--slot 1|2` as canonical and resolves slots zero-flag in recognized
sessions, but the durable vocabulary leaks into store schemas, Event Log
actors, HTTP paths `/api/v1/relay/{room}/{slot}/{action}`, per-slot
credential/state directories, inbox keys, protocol envelopes, checkpoints,
profiles and browser assets. `ActorID.DisplayName()` actively mislabels a
slot with the wrong runtime name (live example: a `claude` slot running the
codex runtime). CONTEXT.md codifies the legacy values in the glossary.

## Scope decision (owner decision A — recommendation: native-only)

Round 2 proved that a global single in-memory vocabulary contradicts the
frozen embedded v6 wire: `durableRoom.Agents/Bindings` and `room.Engine`
participant/message maps are internal model access, not serialization edges;
canonicalizing them rewrites v6-visible actors, and keeping them legacy
abandons "single vocabulary". Two coherent resolutions exist:

- **Native-only (recommended)**: canonical `slot1`/`slot2` applies to new
  native Rooms (protocol v7 is additive). Embedded Rooms keep the legacy
  vocabulary end-to-end — in memory and on every wire — permanently. v6
  byte-identity needs no adapter and stays trivially true. Host mode is
  already an immutable, durably explicit field, so the vocabulary boundary
  rides an existing gate.
- Global with VocabularyCodec: a full translation module with defined seams
  at every Event append/replay, snapshot, embedded runtime construction and
  browser JSON boundary, plus proof obligations for v6 goldens. Strictly
  more work and more risk for no user-visible gain over native-only.

All sections below assume native-only.

## Design

### Vocabulary generation is per-Room, authoritative and durable

- Room vocabulary generation lives in the store: native Rooms created under
  store schema 12 / provisioning 5 are `canonical`; every existing or
  legacy-mode Room (schema ≤ 11, or any embedded Room at any schema) is
  `legacy`. `store.Open` gains an explicit host-mode-aware schema selector
  (P0-4): new embedded Rooms keep writing 11/4; the global
  `version.StoreSchema` bump to 12 only raises the native creation path,
  and readers accept 10/11/12 with a policy table plus old-binary failure
  text and tests.
- A legacy Room's append-only Event Log keeps legacy actor values forever;
  a canonical Room writes canonical values. No events.jsonl ever mixes
  vocabularies. The room engine receives the generation explicitly (config
  plumbed from provisioning), never inferred post-normalization (P1-10).
- Bind responses and native service snapshots gain an optional
  `slot_vocabulary: canonical|legacy` field (v7 additive); binding State
  creation records it; the capability preflight reads it.

### In-memory identity and normalization

- Canonical constants `ActorSlot1 = "slot1"`, `ActorSlot2 = "slot2"`.
  `ActorClaude`/`ActorCodex` remain as the embedded/legacy vocabulary and as
  read aliases. Native code paths normalize via `NormalizeActor()` at every
  ingress (store load, event replay, registry/checkpoint read, HTTP
  path/field parse, local state/credential resolution, profile loads);
  embedded code paths keep legacy constants untouched.
- Dual representation (both `claude` and `slot1` for one slot) is rejected
  fail-closed at every map ingress before normalization — Room
  Bindings/Agents/RuntimeNames, ProvisionRequest, Message delivery maps,
  snapshots, relay bindings/messages. Never first-wins.
- Validation (Room.Validate, ProvisionRequest.Validate,
  validateProvisionedBindings, `protocol --actor`) accepts both
  vocabularies for existing durable state; canonical-room writes accept
  canonical only.
- Exclusion list: `config.File` top-level `claude`/`codex` are runtime
  template names, not slot aliases — explicitly excluded from normalization
  (P1-8). Mention handles are runtime-derived and untouched; `@agent1`
  stays a retired mention alias with no durable meaning.

### Compatibility surfaces

- **Registry checkpoint**: schema 2 and strict shape unchanged (hard
  invariant). Checkpoints are a rebuildable legacy projection: all nested
  ActorID positions (Bindings keys and each Binding.Agent value, Agents,
  RuntimeNames) project to legacy on write and normalize on read, with
  round-trip tests (P1-9).
- **HTTP**: server accepts both vocabularies in every `{slot}` path segment
  (native bind/unbind/auth/park/wait/send/exchange/status/upload) and JSON
  field indefinitely. Responses project per Room: legacy Rooms → legacy
  keys, canonical Rooms → canonical keys (owner decision C).
- **Old CLI + new Service (P1-7, owner decision B — recommendation)**:
  legacy Rooms keep working with old CLIs because their snapshots/responses
  project legacy. Canonical Rooms require a current binary: an old CLI
  binding a canonical Room fails closed at the snapshot lookup, and the new
  CLI's capability preflight rejects canonical binds against an old Service
  with actionable upgrade text. No response negotiation protocol; the skew
  matrix documents "old CLI + canonical Room = unsupported, fails closed".
- **Local `.pairroom` state (P1-6)**: existing legacy State files stay
  schema 1 and are never rewritten or silently upgraded. State schema 2
  (explicit vocabulary field) is created only by new canonical binds. Slot
  directory discovery scans canonical and legacy locations and fails closed
  on same-slot coexistence; credentials are never moved. Old CLIs reject
  schema 2 state — acceptable under decision B because only canonical binds
  produce it.
- **AgentPairProfile (P1-8, owner decision E — recommendation)**: profile
  file schema 1 keeps legacy map keys as its durable form permanently;
  loads normalize into memory; saves project back to legacy keys. No
  profile schema bump. Dual-vocabulary profile files are rejected.
- **Browser assets**: participant key handling accepts both vocabularies
  (JS fixtures for each); canonical native Rooms render "Agent 1/2" with
  runtime labels from runtime metadata.
- **Downgrade boundary (P1-11, owner decision D — recommendation)**: once
  any canonical Room exists, binaries without schema-12 readers fail closed
  on it (existing unsupported-schema text). That point is irreversible for
  that Room; docs recommend `pairroom backup` before creating the first
  canonical Room, and the release notes mark the version as the downgrade
  floor. No hidden auto-migration.

### Display, glossary and docs

- Phase 0 does not rewrite `DisplayName()` (normal runtime UI already uses
  ParticipantIdentities; global rewrite touches fallback/error paths).
  Phase 0 ships docs plus a `SlotLabel` call-site audit; the fix ("Agent
  1"/"Agent 2") lands in Phase 3 behind that audit's tests.
- CONTEXT.md "Participant slot": canonical durable values `slot1`/`slot2`
  for native canonical Rooms; legacy `claude`/`codex` remain for embedded
  and pre-migration Rooms and move to `_Avoid_` for new-vocabulary contexts
  (quotation/history/migration use only).
- CLAUDE.md invariant rewording (owner approval, Phase 3): durable ActorID
  values are `slot1`/`slot2` for canonical native Rooms; legacy values
  persist for embedded and existing Rooms; registry checkpoint schema 2 and
  embedded v6 unchanged.
- Skill text: "durable IDs claude/codex remain accepted" becomes "accepted
  as legacy aliases"; CLI_REFERENCE/API_REFERENCE/PROTOCOL/NATIVE_RELAY
  sweep.

## Phasing

1. **Phase 0**: docs + SlotLabel call-site audit only; independently
   shippable; no behavior change.
2. **Phase 1 (read side)**: native-path `NormalizeActor()` at all ingress,
   dual-representation rejection, HTTP path aliases, per-Room response
   projection plumbing, profile normalize-on-read with legacy-key saves,
   runtime-template-name exclusion. No canonical writes; old and new
   binaries interoperate on legacy Rooms.
3. **Phase 2 (write side)**: host-mode-aware schema selector (12/5 native,
   11/4 embedded), canonical new native Rooms, State schema 2, capability
   preflight, checkpoint nested legacy projection tests, UI dual-vocabulary
   fixtures.
4. **Phase 3 (cleanup)**: DisplayName fix behind audit tests, alias
   constant deprecation in canonical-only paths, glossary/CLAUDE.md final
   sweep, golden updates.

Only Phase 2 writes canonical bytes. Revertibility ends at the first
canonical Room (decision D boundary); Phases 0/1 are revertible freely.

## Verification matrix

- Legacy room fixtures (10/3, 11/4): replay, repair, rebind, archive,
  restore, diagnostics — exact bytes; active legacy Room appends keep
  legacy vocabulary after upgrade.
- Embedded: v6 byte-identical goldens; embedded new-room creation still
  writes 11/4 (schema selector test); JS participant-key fixtures both
  vocabularies; contract output unchanged.
- Canonical native room: end-to-end create/bind/FIFO/events/registry
  rebuild; browser shows "Agent 1/2" with runtime labels.
- Collision matrix: dual representation rejected at every ingress
  (room, provision, profiles, messages, snapshots, relay state, checkpoint
  nested ActorID round-trip).
- Local directories: canonical/legacy coexistence fails closed; credential
  preservation; hook discovery and purge scan both vocabularies; State
  schema 1/2 actual binary interoperability fixtures.
- Skew fixtures as real processes: old CLI + new Service on legacy Room
  (works), old CLI + canonical Room (fails closed, actionable), new CLI +
  old Service canonical bind (preflight rejection), old binary + schema 12
  store (unsupported text).
- Profile fixtures: old service reads profiles saved by new binary and
  vice versa (legacy keys both ways).
- Canonical generation missing/tampered: fail-closed.
- HTTP: every slot-bearing path in both vocabularies; protocol prompt
  byte-budget; archive/restore event actor exact bytes.
- `make check`, `make smoke`, `make browser-check`; real vendor E2E remains
  a separate release gate.

## Owner decisions (with recommendations)

- **A. Scope**: native-only (recommended) vs global VocabularyCodec.
- **B. Old CLI + new Service**: legacy Rooms supported; canonical Rooms
  require current binary, fail closed (recommended) vs full response
  negotiation (not recommended: new protocol surface for a transient skew).
- **C. Management/public snapshots**: per-Room projection — legacy Rooms
  legacy keys, canonical Rooms canonical keys (recommended) vs frozen
  legacy everywhere.
- **D. Downgrade**: first canonical Room = downgrade floor; backup
  recommended in docs (as designed) vs stronger gates.
- **E. AgentPairProfile**: schema 1 legacy keys remain the durable form,
  normalize-on-read (recommended) vs profile schema 2.

## Revision log

- v3 (2026-09-16): scope narrowed to native-only (P0-3); host-mode-aware
  schema selector and per-Room durable generation with explicit engine
  plumbing (P0-4, P1-10); legacy State schema 1 frozen, State schema 2 only
  for canonical binds (P1-6); old-CLI skew closed via per-Room projection +
  capability preflight + documented unsupported cell (P1-7); profile
  legacy-key durable form and runtime-template-name exclusion (P1-8);
  per-object alias rules and nested checkpoint projection (P1-9);
  irreversible boundary and downgrade floor (P1-11); owner decisions A–E
  with recommendations; verification matrix expanded per review.
- v2 (2026-09-16): round-1 fixes (embedded adapter attempt, checkpoint
  schema 2 retained, collision policy, skew strategy b, Phase 0 reduction).
- v1 (2026-09-16): initial draft.
