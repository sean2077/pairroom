# Participant-slot ActorID clean cutover: `claude`/`codex` retired, canonical `slot1`/`slot2`

Status: draft v7 — owner direction (clean cutover, no migration compatibility) plus peer sanity-check completeness requirements incorporated. Two owner decisions remain (Release version, Service-root policy); everything else is design-final. Nothing here is implemented.
Updated: 2026-09-16.

## Owner direction

The tool is in development. Old durable formats are retired outright: no alias layers, no dual-vocabulary serialization, no skew matrix, no generation indexes, no migration. Implement the canonical vocabulary cleanly; anything legacy fails closed with actionable text. Test outcomes say "retired / fail-closed", never "unsupported but recoverable".

This direction consciously overrides four CLAUDE.md durable invariants (their rewording ships in the same change, owner pre-approved by this direction): reading store schema 10/11 as embedded; registry checkpoint schema 2 strict shape; embedded protocol v6 unchanged; existing rooms keeping original bytes. The "reject retired Rooms before replay or repair" invariant is reused as the retirement mechanism.

## Design

### Vocabulary

Canonical durable ActorID values: `slot1` (Agent 1) and `slot2` (Agent 2). Constants renamed accordingly; `ActorClaude`/`ActorCodex` deleted, not aliased. `user`/`system` actors unchanged.

CLI input: `--slot 1|2|agent1|agent2` canonical; `claude`/`codex` inputs remain accepted as pure CLI aliases normalized before any persistence (zero durable meaning). Generated commands print numeric slots.

Mention handles are runtime-derived (`@claude`/`@codex`/`@grok`) and untouched; `@agent1` remains a retired mention alias. `config.File` top-level `claude`/`codex` runtime template names are unrelated to slot identity and untouched. `DisplayName()` becomes "Agent 1"/"Agent 2"; runtime names come only from runtime metadata (`runtime_names`, ParticipantIdentities).

### Protocol version map (sanity check 1)

Changing canonical ActorID bytes changes both shipped contracts, so both versions bump explicitly: embedded `pairroom-protocol/v6` → **v7**, native `pairroom-protocol/v7` → **v8**. `pairroom protocol` output, bootstrap projections, contract goldens, PROTOCOL.md and every version-stamped doc/test move together in the same change. No shipped version string keeps legacy actor bytes.

### Durable formats (single generation, clean break)

| Surface | New format | Legacy encounter |
|---|---|---|
| Store | schema 12 / provisioning 5, canonical actors only | schema ≤ 11 rejected at open/replay/repair: "retired development format — recreate the Room" |
| Registry checkpoint | schema 3, strict exact canonical keys, host mode retained | schema 2 rejected per Service-root policy below; never silently overwritten |
| Event Log | canonical actors | legacy events only exist inside retired rooms (rejected above) |
| Protocol | embedded v7 / native v8, canonical values, goldens updated | n/a — one vocabulary everywhere |
| Binding state | schema 2, canonical slot | schema 1 rejected → re-bind |
| Credential/state dirs | `slots/slot1|slot2` | legacy dirs never read, copied, or auto-deleted — only ignored, reported by doctor, and explicitly human-purgeable; tests assert no fallback path reaches them |
| AgentPairProfile | schema 2, canonical keys | schema 1 rejected → recreate profiles |
| HTTP `{slot}` path + JSON | canonical values; numeric `1|2` accepted as alias at parse | legacy string values rejected 400 with upgrade text |
| Browser assets | canonical participant keys | n/a |

No capability preflight, no response projection, no alias acceptance at durable boundaries: CLI and Service ship together; a version-mismatched binary meets the unsupported-version failure text.

### Root-format retirement preflight (sanity check 3)

Retirement must precede every recovery mutation. Today `OpenRegistry` runs `recoverRoomDeletionQuarantine` before checkpoint load and room scan, and a prepared deletion can restore data when the checkpoint is untrusted — once schema 2 is retired, that path could mutate legacy data before any guard fires. The cutover adds a root-format preflight that stops recovery, replay, repair, quarantine restoration and any rewrite **before the first mutation**, covered by checkpoint and quarantine fixtures (prepared-deletion + retired-checkpoint combinations must exit fail-closed with zero writes).

### Service-root policy (sanity check 4; owner decision S)

The checkpoint is the only persistence for explicitly registered empty Projects, so "reject schema 2, rebuild surviving Rooms" would silently lose them. Options:

- **S1 (recommended): whole-root retirement.** An old Service data root fails closed entirely at startup with actionable text (retired root; start a new root or purge via doctor tooling). Nothing is silently lost or overwritten; empty-Project registrations are recreated by the user. Matches "old formats are invalid" literally.
- S2: accept and report the loss — rebuild from surviving schema-12 rooms, explicitly log dropped empty-Project entries. Weaker guarantee, more startup logic.

Either way schema-2 checkpoints are never overwritten in place with schema 3.

### Strict canonical validation (sanity check 5)

Legacy actors must not be assumed to occur only in retired rooms: a fabricated schema-12 Room carrying a legacy event actor fails during replay. Checkpoint schema 3 enforces strict exact canonical keys — unknown or duplicate alias keys rejected — with validation before indexing. Fixtures cover both.

### Backup / restore preflight (sanity check 6)

Backup inspection happens before touching the destination: a legacy backup never overwrites canonical data and then becomes an unusable retired root. Restore of a legacy backup fails closed at preflight with retirement text, or lands as an explicitly isolated, non-activating import (implementation picks one; both tested). `pairroom verify` and diagnostics classify retired formats explicitly.

### Code shape

Global slot helpers (`ValidParticipant`, `OtherParticipant`, `SlotActors`) keep working on the single canonical vocabulary — no Vocabulary module, no prohibition tests, no per-Room generation plumbing. The ~878 references are a mechanical rename plus golden updates. Retirement is one shared guard style at store open / checkpoint load / state load / profile load / bind parse / restore preflight / root preflight, each with its own actionable message and retired/fail-closed test naming.

### Docs and contract text (same change)

CLAUDE.md: ActorID invariant reworded to canonical `slot1`/`slot2`; schema invariants become "read store schema 12/provisioning 5, reject retired ≤ 11"; checkpoint schema 3 strict shape; protocol map embedded v7 / native v8. CONTEXT.md "Participant slot": canonical values `slot1`/`slot2`; `claude`/`codex` move to `_Avoid_` (quotation/history only). Sweep PROTOCOL/STORAGE/ARCHITECTURE/NATIVE_RELAY/CLI_REFERENCE/API_REFERENCE. Skill text "durable IDs claude/codex" becomes "CLI input aliases only" — both product copies (`skills/pairroom-relay/SKILL.md` and the `internal/relayclient/skill/` projection) updated byte-identically. Revised prose follows the project convention of no fixed-column hard wraps.

### Release version (sanity check 2; owner decision R)

This is a persisted-format, API and protocol breaking release from 4.1.0.

- **R1 (recommended): 5.0.0** — honest SemVer major; VERSION, `internal/version.Current`, changelog heading, tag and release tests agree in the same change.
- R2: an explicit unreleased/prerelease policy chosen by the owner (e.g. 5.0.0-rc.N flow), stated before Phase 1 starts.

## Consequences (accepted by owner direction)

All existing Rooms on this machine — including today's test Room — plus their bindings, credentials, profiles and checkpoints are retired at upgrade; users recreate Rooms and re-save pair profiles once. Downgrade: old binaries reject schema 12 (existing behavior); the release notes mark the floor. `pairroom backup` before upgrading remains good hygiene but gates nothing.

## Phasing

1. **Phase 1 (single implementation)**: rename + schema 12/5 + checkpoint 3 + state 2 + profile 2 + protocol v7/v8 bump + retirement guards (root preflight first) + strict validation + backup preflight + goldens + docs/contract text + release version. Mechanically large, semantically flat; one branch, reviewable by surface.
2. **Phase 2 (optional, later)**: SlotLabel/DisplayName call-site audit follow-ups; doctor/purge tooling for legacy directories.

## Verification

- Retirement fixtures: schema 10/3 and 11/4 rooms, schema-2 checkpoints (including prepared-deletion quarantine combinations), state 1, profile 1, legacy path/JSON values, legacy backups — each rejected with its specific actionable text, zero mutations, never partially loaded.
- Strict-format fixtures: fabricated schema-12 room with legacy actor fails replay; schema-3 unknown/duplicate keys rejected before indexing.
- Credential isolation: no code path reads, copies, or deletes legacy credential/state directories; doctor reports; human purge explicit.
- Canonical end-to-end: create/bind/FIFO/events/registry rebuild/archive/restore/backup/verify/diagnostics; embedded v7 and native v8 contract goldens; browser fixtures; prompt byte-budget tests preserved.
- Release: VERSION/version.Current/changelog/tag agreement tests.
- `make check`, `make smoke`, `make browser-check`; real vendor E2E stays a separate release gate.

## Owner decisions

- **R. Release version**: R1 = 5.0.0 (recommended) or R2 = explicit prerelease policy.
- **S. Service-root policy**: S1 = whole-root retirement, fail closed (recommended) or S2 = accept and report empty-Project loss.

## Revision log

- v7 (2026-09-16): peer sanity-check incorporated — explicit protocol map (embedded v7 / native v8); release-version decision R; root-format retirement preflight before any recovery mutation; Service-root policy decision S; strict canonical validation against fabricated current-format rooms; backup/restore destination preflight; credential-directory isolation guarantees; retired/fail-closed test naming; prose convention note.
- v6 (2026-09-16): owner direction — clean cutover, legacy retired, compatibility machinery removed. Supersedes v1–v5.
- v1–v5: migration-compatibility design iterations under four rounds of adversarial review (kept in git history for rationale).
