# Upgrading

Treat upgrades as controlled changes, not overwriting active binaries. [Changelog](../CHANGELOG.md) records release history; this page owns current reader and operational boundaries. [Installation](INSTALLATION.md) owns per-channel binary replacement/uninstall.

## Supported Room formats

The 5.0.0 development cutover retired old formats without migration. Current readers require **Store schema 12/provisioning 5**, explicit immutable `host_mode`, registry checkpoint 3, relay state 2, and Agent pair profile storage 2. A retired Service root fails as a whole before recovery/replay/repair/rewrite. Start a new root and recreate registrations, Rooms, and profiles; moving only old Room directories cannot repair an incompatible root checkpoint.

Legacy data/credentials remain untouched for explicit human backup/removal. Never relabel schema numbers, copy selected old events into a new log, or infer old permissions/Bindings from current defaults. Preserve retired roots with their matching binaries for inspection. Previously imported external Room directories are separate backup/isolation responsibilities.

Legacy imports, binding-completion endpoints, inferred selections, role switching, and Reviewer snapshot workspaces are gone. `ordinary_reviewer_policy` is removed. Current effective Embedded permission profiles are independent of Lead/Executor responsibilities; Native retains original harness permissions.

A fresh Embedded Room may explicitly resume a supported existing native session, but pre-binding history is not imported. `pairroom serve` remains current-format standalone development/diagnostics, not Legacy import. Embedded checkpoint-only cleanup may handle a wholly lost Room directory; missing files inside an existing directory, unidentified archive stubs, and ambiguous replacement data still fail closed. Native archive requires valid data.

## Before upgrading

Record PairRoom and selected native CLI versions, read release notes, and stop/drain the PairRoom owner. For Native, separately pause work in the original harnesses: archive/quit cannot stop them. Verify workspace side effects before replacing binaries.

Preserve the entire stopped Service root, including profiles/navigation preferences, and any external Room directories. Verify Room archives with the matching binary, but do not mistake them for complete Service, repository, vendor-session, or Native workspace-credential backups. Keep the matching configuration and binary alongside the backup. See [Operations](OPERATIONS.md#backup).

## Configuration and native runtimes

Startup uses strict JSON: unknown/duplicate fields, trailing documents, and null runtime/policy values fail. Empty policy strings request native inheritance. For Embedded, move Provider/model/effort/permissions out of executable argument templates into Agent selections.

Supported Provider configuration is native inheritance or read-only CC Switch references. Replace removed `providers`, `cc_connect`, and string Provider names with structured references as described in [Configuration](CONFIGURATION.md); never copy credentials into selections. Recheck compatibility and per-process isolation after runtime upgrades.

Remove obsolete `routing_mode`, `max_agent_hops`, `--routing`, `--max-hops`, and role-target automation. Embedded message intents are `steer`/`queue`; Native has its own relay protocol. Exact runtime-derived handles route automatic relay, while explicit Native send uses its command target. Stable JSON actors are `slot1`/`slot2`; runtime-named relay CLI aliases are not durable identities.

## Desktop and daemon

Desktop never implicitly installs a daemon. It reuses an installed daemon or owns an embedded Service; Launch at login is separate OS registration. `make desktop-update` is for source builds and preserves data/login state without reconfiguring a daemon.

Desktop and `pairroom daemon start` recover stale locks only after verifying the recorded PID exited. A live owner fails closed. Use normal drain/stop; never force a second Service onto an owned root. Native harnesses remain separately owned.

## Verify the upgrade

Verify the binary, then use an unused isolated demo root:

```bash
pairroom version
pairroom service --mock --data-root "$HOME/.pairroom-upgrade-check"
```

Use the corresponding PowerShell path syntax on Windows. Check strict configuration and Project discovery, then a fresh Mock Embedded Room's relay/FIFO, permissions, restart, and backup. Do not run Mock over real Rooms or assume it verifies vendor authentication.

For real Embedded acceptance, begin with an explicitly read-only task before peer relay. For Native, inspect the existing binding with the matching CLI, verify approved hook installation, and perform a controlled addressed exchange in the original sessions. Distinguish queued/handed-off evidence from model acceptance; [Native setup](NATIVE_RELAY.md) owns the procedure. Paid/live checks require explicit consent.

HTTP/SSE clients must rerun their contract tests. A stream reset requires a fresh snapshot/current cursor, not replayed commands. [API](API_REFERENCE.md) and [CLI](CLI_REFERENCE.md) own checked inventories.

## Rollback and integrity

Stop/drain all relevant owners, preserve the current root, and restore a complete verified pre-upgrade root with its matching binary/configuration. Inspect native sessions and workspace side effects separately. Do not mix old/new data files; binary rollback cannot undo native edits or metadata title changes.

Only an incomplete final JSONL record in a supported store is repairable. Never renumber complete records or skip middle corruption. Ambiguous append failure closes the writer; check storage health before reopening. Restore validates the full archive before publishing a destination. Backup/diagnostic outputs must stay outside the source Room directory, including symlink aliases.

## Native rollout and rollback

Native remains experimental. Authenticated multi-round acceptance for the actual Claude/Codex/Grok versions and settings is separate from synthetic hooks, browser fixtures, and dated working-session reports. Consult the [design record](design/native-host-mode.md) for rationale, not a promise that current vendor acceptance has run.

**Routine compatible upgrades do not require unbinding or replacing valid sessions.** Update CLI/Service together and refresh/review installed hooks/skill when their definitions change. Use idempotent bind to restore discovery without rotating generation.

An incompatible downgrade or deliberate Native removal is different. Before replacing the working binary, stop native work, back up all relevant state, and use that matching CLI to unbind the affected slots, with `--purge-hooks` only when intentional. Remove only selected PairRoom workspace data/unused managed skills after confirming no other bindings need them. Never purge unrelated native hooks/configuration. Archive alone neither releases Binding ownership nor creates compatibility isolation. Before running an older CLI on a bound slot, run `pairroom relay reconcile` with the current CLI until `relay status` shows no `held_publications`: earlier CLIs keep only the oldest pending Stop reply and would drop replies held behind it.

Pre-5.0.0 binaries reject schema-12 Rooms. There is no in-place format downgrade: preserve an isolated matching-version backup/root rather than relabelling schema or copying credentials between generations.

## Native binding setup

Follow [Native setup](NATIVE_RELAY.md). A confirmed binding retains its identity across compatible updates. An incomplete historical binding may require intentional replacement, but a **lost response to a current bind** is recovered by rerunning the same Room/slot bind without a new `--create` or `--replace`. Creation success followed by bind failure must use the reported Room recovery command.

Session-first discovery is compatible with current relay state. A binding without a locator can be rediscovered from its workspace/registered Service Projects by an ordinary foreground call. If both cwd and project hints moved, run this once in the original session:

```bash
pairroom relay bind --repo "<original-bound-workspace>"
```

This resumes the binding, not a new generation. Conflicting explicit workspace/Room/slot/Service selectors are rejected; a bound session cannot create another Room just by changing directories. Missing/redirected workspaces and corrupt locators fail closed until inspected. Do not copy `.pairroom` into a task worktree. See [workspace upgrade/recovery](NATIVE_SESSION_WORKSPACE.md#upgrade-and-recovery).

### Adding Grok Build to Native Rooms

Update CLI, Service, and distributed skill together. Install the intended Grok project's hooks, then approve its exact definition and folder trust before bind. Grok normally reuses a Claude Code project Stop hook; otherwise it uses `.grok/hooks/pairroom.json`. With Claude hook compatibility disabled, it needs its own hook. Installation preserves unrelated hooks/configuration. [Grok Native](CLI_REFERENCE.md#grok-build-native) owns clipped-output and foreground-receive limits.

Existing Rooms retain stored selections. To change their Runtime pair, create a new Room rather than rewriting immutable metadata. Older readers that reject Grok Native selections cannot open those Rooms even when Store schema matches. Prefer a complete matching-version backup for rollback; otherwise stop work/owners, back up, unbind affected Grok Rooms with the matching CLI, and isolate **every Grok-containing Room directory** from the old Service's discovery root. Archive alone is insufficient. Never rename runtimes or rewrite events to force acceptance.
