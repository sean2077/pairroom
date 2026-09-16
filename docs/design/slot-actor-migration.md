# Participant-slot ActorID clean cutover: `claude`/`codex` retired, canonical `slot1`/`slot2`

Status: draft v6 — owner direction received (clean cutover, no migration
compatibility; retired formats fail closed). Supersedes the v1–v5
migration-compatibility design and its owner decisions A–I, which are
moot under this direction. Nothing here is implemented.
Updated: 2026-09-16.

## Owner direction

The tool is in development. Old durable formats are retired outright: no
alias layers, no dual-vocabulary serialization, no skew matrix, no
generation indexes, no migration. Implement the canonical vocabulary
cleanly; anything legacy fails closed with actionable text.

This direction consciously overrides four CLAUDE.md durable invariants
(their rewording ships in the same change, owner pre-approved by this
direction): reading store schema 10/11 as embedded; registry checkpoint
schema 2 strict shape; embedded protocol v6 unchanged; existing rooms
keeping original bytes. The "reject retired Rooms before replay or
repair" invariant is reused as the retirement mechanism.

## Design

### Vocabulary

- Canonical durable ActorID values: `slot1` (Agent 1), `slot2` (Agent 2).
  Constants renamed accordingly; `ActorClaude`/`ActorCodex` deleted, not
  aliased. `user`/`system` actors unchanged.
- CLI input: `--slot 1|2|agent1|agent2` canonical; `claude`/`codex` inputs
  remain accepted as pure CLI aliases normalized before any persistence
  (zero durable meaning). Generated commands print numeric slots.
- Mention handles are runtime-derived (`@claude`/`@codex`/`@grok`) and
  untouched; `@agent1` remains a retired mention alias.
- `config.File` top-level `claude`/`codex` runtime template names are
  unrelated to slot identity and untouched.
- `DisplayName()` becomes "Agent 1"/"Agent 2"; runtime names come only
  from runtime metadata (`runtime_names`, ParticipantIdentities).

### Durable formats (single generation, clean break)

| Surface | New format | Legacy encounter |
|---|---|---|
| Store | schema 12 / provisioning 5, canonical actors only | schema ≤ 11 rejected at open/replay/repair: "retired development format — recreate the Room" |
| Registry checkpoint | schema 3, canonical keys, host mode retained | schema 2 rejected → registry rebuilds from surviving rooms; retired rooms stay rejected |
| Event Log | canonical actors | legacy events only exist inside retired rooms (rejected above) |
| Protocol | v7 envelopes canonical; embedded contract goldens updated to canonical values | n/a — one vocabulary everywhere |
| Binding state | schema 2, canonical slot | schema 1 rejected → re-bind (credentials for retired rooms are dead anyway) |
| Credential/state dirs | `slots/slot1|slot2` | legacy dirs ignored, reported by doctor, purgeable |
| AgentPairProfile | schema 2, canonical keys | schema 1 rejected → recreate profiles |
| HTTP `{slot}` path + JSON | canonical values; numeric `1|2` accepted as alias at parse | legacy string values rejected 400 with upgrade text |
| Browser assets | canonical participant keys | n/a |

- No capability preflight, no response projection, no alias acceptance at
  durable boundaries: CLI and Service ship together; a version-mismatched
  binary meets the existing unsupported-schema failure text.
- Backup/restore/verify: backups containing schema ≤ 11 rooms restore as
  **retired** (rejected before replay, reported, never repaired);
  `pairroom verify` and diagnostics classify them explicitly.

### Code shape

- Global slot helpers (`ValidParticipant`, `OtherParticipant`,
  `SlotActors`) keep working on the single canonical vocabulary — no
  Vocabulary module, no prohibition tests, no per-Room generation
  plumbing. The ~878 references are a mechanical rename plus golden
  updates.
- Retirement is one shared guard at store open / checkpoint load / state
  load / profile load / bind parse, each with its own actionable message.

### Docs and contract text (same change)

- CLAUDE.md: ActorID invariant reworded to canonical `slot1`/`slot2`;
  schema invariants become "read store schema 12/provisioning 5, reject
  retired ≤ 11"; checkpoint schema 3 strict shape; embedded protocol
  vocabulary updated (contract goldens are the lock).
- CONTEXT.md "Participant slot": canonical values `slot1`/`slot2`;
  `claude`/`codex` move to `_Avoid_` (quotation/history only).
- Glossary-adjacent sweep: PROTOCOL/STORAGE/ARCHITECTURE/NATIVE_RELAY/
  CLI_REFERENCE/API_REFERENCE; skill text "durable IDs claude/codex"
  becomes "CLI input aliases only".

## Consequences (accepted by owner direction)

- All existing Rooms on this machine — including today's test Room — plus
  their bindings, credentials, profiles and checkpoints are retired at
  upgrade. Users recreate Rooms; pair profiles are re-saved once.
- Downgrade: old binaries reject schema 12 (existing behavior); the
  upgrade release notes mark the floor. No backup gate is required by
  design, though `pairroom backup` before upgrading remains good hygiene.

## Phasing

1. **Phase 1 (single implementation)**: rename + schema 12/5 + checkpoint
   3 + state 2 + profile 2 + retirement guards + goldens + docs/contract
   text. Mechanically large, semantically flat; one branch, reviewable by
   surface.
2. **Phase 2 (optional, later)**: SlotLabel/DisplayName call-site audit
   follow-ups and doctor/purge tooling for legacy directories.

## Verification

- Retirement fixtures: schema 10/3 and 11/4 rooms, schema-2 checkpoints,
  state 1, profile 1, legacy path/JSON values — each rejected with its
  specific actionable text, never repaired, never partially loaded.
- Canonical end-to-end: create/bind/FIFO/events/registry rebuild/archive/
  restore/backup/verify/diagnostics; embedded and native contract goldens;
  browser fixtures; prompt byte-budget tests preserved.
- `make check`, `make smoke`, `make browser-check`; real vendor E2E stays
  a separate release gate.

## Revision log

- v6 (2026-09-16): owner direction — clean cutover, legacy retired,
  compatibility machinery (aliases, projections, generation index,
  capability preflight, Vocabulary module, skew matrix, decisions A–I)
  removed. Supersedes v1–v5.
- v1–v5: migration-compatibility design iterations under four rounds of
  adversarial review (kept in git history for rationale).
