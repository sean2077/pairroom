# Upgrading

Treat an upgrade as a controlled change, not as overwriting an active binary. Release history is in [CHANGELOG](../CHANGELOG.md); this page describes the current reader and operational boundary.

## Supported Room formats

Readers and writers require **Store schema 10**. Service-managed Rooms also require **provisioning schema 3**, explicit immutable Agent selections, and matching collaboration instructions. Registry checkpoints use schema 2. Current-schema version-1 default instructions, version-2 flexible defaults, and custom instructions remain readable and unchanged; this cleanup does not upgrade their prose or grant permissions.

Legacy Room compatibility is removed: schema 9, provisioning 1/2, missing-metadata imports, inferred native Bindings, Service-default fallback at activation, role switching, role-bound Reviewer snapshots, and lifecycle-only archive stubs are unsupported. The import and binding-completion HTTP endpoints and UI are removed. `ordinary_reviewer_policy` is no longer a configuration field. The current `configured`, `read-only`, and `yolo` permission profiles remain independent of Lead/Executor responsibilities.

**There is no automatic migration or deletion of old data.** Unsupported stores fail before Event Log replay or tail repair. Preserve them and their matching older binary for inspection. Before starting the current Service, move unsupported Room directories out of its `rooms/` discovery root after stopping all owners and making a backup. Do not edit schema numbers, relabel metadata, copy selected records into a current log, or use new defaults to infer historical permissions. Previously imported external directories are no longer discovered from the checkpoint and are never erased by this cleanup.

Create a new Room to continue work. An explicit Existing Binding may resume a native CLI session, but prior native history remains outside the PairRoom transcript. `pairroom serve` remains a current-format standalone development/diagnostic command; it does not restore Legacy Rooms or import standalone history into the Service.

A current Room whose entire directory is lost can still be archived and explicitly removed using its validated checkpoint identity. Missing files inside a directory, ambiguous replacement data, and unidentified archive stubs fail closed. Prepared deletion quarantine entries are restored when the checkpoint still owns them; committed current-format deletions can finish cleanup after a crash.

## Before upgrading

1. Read the changelog, stop or archive active Rooms, and record the binary and native CLI versions.
2. Back up the complete Service data root and verify any Room archives with the matching binary. Keep the matching binary and configuration alongside the backup.
3. Inspect working repositories for unrecognized native side effects. Identify unsupported Room data before replacing the binary.

## Configuration and native runtimes

Service configuration is one strict JSON object: duplicate fields, trailing documents, unknown fields, and `null` runtime/policy fields are rejected. Empty policy strings request native inheritance. Move per-Room Provider/model/effort/permission choices out of runtime command arguments and into the Agent selection.

Providers use native configuration or read-only CC Switch references. Back up the data root, configure the equivalent CC Switch profile, then replace removed top-level `providers` / `cc_connect` and per-slot Provider-name strings with structured references. Never copy credentials into Room selections. Native runtime versions and per-process credential boundaries must be rechecked after upgrade.

Remove retired `routing_mode`, `max_agent_hops`, `--routing`, `--max-hops`, and role-target automation. Message intents are `steer` or `queue`. Only current runtime-derived exact handles route Agent relay; old control markers are ordinary text. Stable JSON slot IDs remain `claude` and `codex`, independently of the selected Runtime.

## Desktop and daemon

Desktop never installs a daemon implicitly. It reuses an installed daemon or owns an embedded Service when none is installed. Launch at login is an explicit native Settings operation. `make desktop-update` replaces the host and bundled CLI without changing user data or daemon configuration.

Desktop and `pairroom daemon start` recover a crash-stale lock only after verifying the recorded PID is gone. A live owner fails closed. Normal stop/restart drains active native Turns; never force a second Service onto an owned data root.

## Verify the upgrade

```bash
pairroom version
pairroom service --mock
```

Check strict configuration parsing and Project discovery, then create a Mock Room and exercise exact-handle relay, FIFO, permission changes, restart, and backup verification. Real-runtime verification is separate: start with an explicitly read-only single-Agent Turn before testing an addressed peer response. Mock success does not prove vendor authentication or model availability.

HTTP/SSE clients must re-run their contract tests. On a stream `reset`, fetch a fresh snapshot and reconnect from its `latest_seq`; do not replay commands to repair a display gap. See [API reference](API_REFERENCE.md) and [CLI reference](CLI_REFERENCE.md) for the checked inventories.

## Rollback and integrity

Stop/drain the Service, save its current data root, restore the complete verified pre-upgrade backup with its matching binary/configuration, then recheck Bindings and repository side effects. Do not mix old and new data files. Native session titles already changed through vendor metadata APIs require a separate native rename; binary rollback cannot undo them.

Do not renumber complete Event Log records to bypass verification. Only an incomplete final JSONL record in a supported store may be repaired. An ambiguous append I/O error closes the writer; reopen only after checking storage health and the verified log. Restore validates the entire archive before replacing a destination, and backup/diagnostic outputs must remain outside the source Room directory.
