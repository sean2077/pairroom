---
status: historical
updated: 2026-09-23
---

# Participant-slot ActorID clean cutover

This is a completed design record, **not a pending implementation plan or authorization to purge data**. The owner decisions were resolved on 2026-09-16 for the 5.0.0 development cutover. The former “nothing implemented yet / ready for Phase 1” status is obsolete.

## Accepted decision

Use canonical durable `slot1`/`slot2` identities, independently of the selected Runtime. Retire old durable formats outright instead of introducing a migration/alias layer. Keep `claude`/`codex` only as relay CLI input aliases; runtime-derived mention handles remain separate. Reject retired Service roots before recovery or mutation, preserving their data for explicit human handling.

The implemented contracts use Embedded protocol v7 / Native v8, Store schema 12/provisioning 5, registry checkpoint 3, relay state 2, and Agent pair profile storage 2. These numbers describe the cutover; their current authority is [Storage](../STORAGE.md), [Protocol](../PROTOCOL.md), and [Upgrading](../UPGRADING.md), not this record.

## Current entry points

Follow [Upgrading](../UPGRADING.md) for format retirement and rollback, [Configuration](../CONFIGURATION.md) for selection semantics, and [Native workspace recovery](../NATIVE_SESSION_WORKSPACE.md) for current bindings. Routine compatible updates do not require repeating the old cutover or replacing valid bindings.

The [complete approved plan at the audited revision](https://github.com/sean2077/pairroom/blob/0d6e63111beb025f2d00c5dd60afa32b16d661a9/docs/design/slot-actor-migration.md) preserves the original alternatives, owner decisions, review history, and phased verification proposal. In particular, its proposed purge tooling and optional follow-ups are not claims that such commands exist. Use the matching CLI reference and actual `--help`, not that plan, for operations.
