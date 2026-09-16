# Participant-slot ActorID migration: legacy `claude`/`codex` to canonical `slot1`/`slot2`

Status: draft — owner decision required; nothing here is implemented.
Updated: 2026-09-16. Requested by the owner after the native-mode audit found
the durable ActorID vocabulary still keyed to vendor names while runtime and
slot identity are fully separated.

## Problem

`model.ActorID` persists `"claude"`/`"codex"` as the two participant-slot
identities (84 files, ~878 references). The CLI surface already treats
`--slot 1|2` as canonical and resolves slots zero-flag in recognized
sessions, but the durable vocabulary leaks:

- `ActorID.DisplayName()` renders slot 1 as "Claude Code" and slot 2 as
  "Codex" even when the slot runs a different runtime (live example: a room
  whose `claude` slot runs the codex runtime). This is an active mislabel.
- Store schema 11/provisioning 4 rooms, registry checkpoint schema 2, Event
  Log actors, HTTP paths `/api/v1/relay/{room}/{slot}/{action}`, per-slot
  credential/state directories, inbox keys and protocol v7 envelope senders
  all serialize the legacy strings.
- CONTEXT.md defines "Participant slot" as persisted ActorID `claude`/`codex`,
  codifying the debt in the glossary.

## Goals / Non-goals

Goals: canonical durable vocabulary `slot1`/`slot2` for all new writes;
legacy values accepted as read aliases indefinitely; display never implies a
runtime; glossary, CLAUDE.md invariant text and docs updated in the same
change series.

Non-goals: rewriting any existing room bytes (append-only Event Log and
"existing rooms keep their original bytes" are hard invariants); changing
runtime-derived mention handles (`@claude`/`@codex`/`@grok` are runtime
identity, untouched); changing Grok readiness semantics; any wire change to
embedded protocol v6.

## Design

### Vocabulary and normalization

- Canonical constants: `ActorSlot1 = "slot1"`, `ActorSlot2 = "slot2"`.
  `ActorClaude`/`ActorCodex` become deprecated read aliases; one
  `NormalizeActor()` applied at every ingress boundary (store load, event
  replay, registry read, HTTP path/field parse, local state/credential
  directory resolution). In memory and in all new writes there is exactly
  one canonical value per slot.
- Owner decision point: `slot1/slot2` (owner-stated preference, consistent
  with the "Participant slot" glossary term) vs `agent1/agent2` (consistent
  with "Agent 1/Agent 2" display text). This draft uses `slot1/slot2`.

### Storage and schema

- New rooms: store schema 12 / provisioning 5 writing canonical actors.
  Readers accept 10..12; schema 12 gates the canonical-only write path.
  Existing 10/3 and 11/4 rooms keep their bytes; replay/repair normalize on
  read. Retired-room rejection order is unchanged.
- Registry checkpoint: rebuildable index. Decision point: bump to checkpoint
  schema 3 with canonical keys (recommended; "strict shape unchanged" then
  applies to schema 3), or keep schema 2 legacy keys as index-internal
  vocabulary. Mixed-vocabulary registries must never exist; rebuild derives
  keys from normalized room state.
- Local `.pairroom` binding state and credential directories: never move or
  rewrite existing files (preserve committed credentials invariant).
  Resolution tries canonical then legacy directory for the normalized slot;
  new binds create canonical directories.

### Wire and process boundaries

- HTTP: server accepts both vocabularies in `{slot}` path segments and JSON
  fields indefinitely (alias at parse). Server responses emit canonical
  values only.
- CLI/service version skew: a new CLI emitting canonical values against an
  old service fails closed. Mitigation options (decision point): (a)
  document upgrade-together and add a service version preflight to relay
  commands; (b) CLI emits legacy values when the binding state holds a
  legacy actor, canonical only for new binds. Recommendation: (b) — the
  binding state already records the actor used at bind time; no new
  negotiation protocol needed.
- Protocol v7 envelopes: sender field writes canonical for new rooms;
  readers normalize. v6 embedded stays byte-identical.
- Diagnostics/archive: `SupportsStoreSchema` gates as today; archive reports
  include the vocabulary generation for triage.

### Display and docs

- `DisplayName()`: "Agent 1"/"Agent 2" (runtime names come from runtime
  metadata and existing `runtime_names` title projections, never from the
  slot actor).
- CONTEXT.md "Participant slot" entry updated in the same change; legacy
  names move to its `_Avoid_` list (quotation/history/migration use only).
- CLAUDE.md invariant rewording (owner approval): durable ActorID values are
  `slot1`/`slot2`, identifying Agent 1/Agent 2; legacy `claude`/`codex`
  remain accepted read aliases.
- Skill: "durable IDs claude/codex remain accepted" becomes "accepted as
  legacy aliases"; CLI_REFERENCE/API_REFERENCE/PROTOCOL/NATIVE_RELAY sweep.

## Phasing

1. **Phase 0 (independent quick win)**: DisplayName fix + doc mislabel
   sweep. No schema change; ships immediately if approved.
2. **Phase 1 (read side)**: `NormalizeActor()` at all ingress boundaries,
   server alias acceptance, tests for legacy fixtures. No write change;
   fully backward/forward compatible.
3. **Phase 2 (write side)**: schema 12/5, canonical writes for new rooms,
   registry checkpoint decision implemented, binding-state skew strategy
   implemented.
4. **Phase 3 (cleanup)**: deprecate alias constants in code paths that no
   longer need them, glossary/CLAUDE.md/docs final sweep, golden updates.

Each phase is separately mergeable and independently revertible before
Phase 2 writes canonical bytes.

## Verification matrix

- Legacy room fixtures (10/3 and 11/4): replay, repair, rebind, archive,
  restore, diagnostics — bytes preserved, behavior identical.
- New room: canonical actors end-to-end (create, bind, FIFO, events,
  registry rebuild, browser fixtures show "Agent 1/2" with runtime labels).
- Active legacy binding across upgrade: credentials intact, wait/send/
  exchange/hook paths unaffected.
- API alias matrix: both vocabularies in path and body; canonical-only
  responses.
- Version skew: new CLI + old service fails closed with actionable error;
  old CLI + new service works via alias.
- `make check`, `make smoke`, `make browser-check`; real vendor E2E remains
  a separate release gate (no synthetic claims).

## Open questions

1. `slot1/slot2` vs `agent1/agent2` (owner).
2. Registry checkpoint schema 3 vs frozen legacy keys (recommend bump).
3. Skew mitigation (a) vs (b) (recommend b).
4. Should Phase 0 ship ahead of the rest (recommend yes)?
5. Any acceptance appetite for eventually rejecting legacy aliases in new
   writes only (this draft keeps reads permissive forever)?
