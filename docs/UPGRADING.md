# Upgrading

PairRoom's CLI, Event Log, HTTP API, and native adapters evolve with the official CLIs. Treat an upgrade as a controlled change, not as overwriting a binary in place.

## Before upgrading

1. Read [CHANGELOG](../CHANGELOG.md);
2. Stop or archive active Rooms;
3. Create and verify a PairRoom data backup;
4. Record the current binary, Claude Code / Codex / Grok Build, and configuration versions;
5. Make sure the working repository has no unrecognized native side effects.

## Current breaking boundary

The routing policy supports only:

```json
{"routing_mode": "turns"}
```

`manual`, `mentions`, and `roundtable` are incompatible and are not silently normalized. When upgrading an old install:

- change the configuration file to `turns` explicitly;
- remove old `--routing` values from CLI automation;
- back up persisted Rooms that contain old routing events, then rebuild them;
- do not rewrite JSONL to fake a migration.

JSON keys `claude` and `codex` remain durable Agent 1 / Agent 2 slots. Add `runtime` (`claude` | `codex` | `grok`) per slot when selecting a non-default harness. Empty `provider`, `model`, `effort`, and `instructions` now inherit the selected native CLI's user/global configuration.

## Perform the upgrade

After replacing the binary, run:

```bash
pairroom version
pairroom service --mock
```

Then verify:

- configuration parses strictly;
- the Project registry can be read;
- a new Mock Room can complete a multi-Turn FIFO;
- backup verification succeeds;
- real mode first completes a read-only single-Agent Turn, then reviewer / handoff.

## Rollback

Rolling back the binary is not the same as rolling back the Event Log. If the new version has already written events the old version does not understand:

1. Stop the Service;
2. Save the current data root;
3. Restore the complete, verified pre-upgrade backup;
4. Restore the matching binary and configuration;
5. Re-verify Bindings and repository side effects.

Do not mix old and new data files.

## Documentation and clients

External tools that call the HTTP API, parse the Event Log, or depend on CLI copy must re-run contract tests at upgrade time. The route inventory in `docs/API_REFERENCE.md` and the flag inventory in `docs/CLI_REFERENCE.md` are checked against current source by `make docs-check`.
