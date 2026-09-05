# Upgrading

PairRoom's CLI, Event Log, HTTP API, and native adapters evolve with the official CLIs. Treat an upgrade as a controlled change, not as overwriting a binary in place.

## Before upgrading

1. Read [CHANGELOG](../CHANGELOG.md);
2. Stop or archive active Rooms;
3. Create and verify a PairRoom data backup;
4. Record the current binary, Claude Code / Codex / Grok Build, and configuration versions;
5. Make sure the working repository has no unrecognized native side effects.

## HTTP client adjustments (Unreleased)

This polish update does not change Store schema 9, provisioning schema 2, native prompts, or complete-response Agent relay. Existing supported Rooms require no data migration for these changes.

For external HTTP/SSE clients, validate `message_limit` as an integer from 0 to 1000; invalid values now return HTTP 400 rather than being silently reinterpreted. Omitted or zero limits still request the full snapshot. On an SSE `reset` event, fetch a fresh snapshot and reconnect from its `latest_seq`; the server closes that stream because the cursor is ahead or older than the bounded replay tail. A non-empty `Last-Event-ID` takes precedence over `since`. See [API reference](API_REFERENCE.md) for the wire contract.

## Current breaking boundary

### Provider and Room provisioning migration

This release moves the root module to Go 1.25, replaces PairRoom-owned Provider configuration with read-only CC Switch v3.20.1/schema 18 references, and writes new Rooms with provisioning schema 2.

Before installing the new binary:

1. Stop the Service after active Turns drain, then run `pairroom backup` for the complete data root and verify the archive;
2. Preserve that backup unchanged as the downgrade point;
3. Remove top-level `providers` and `cc_connect` configuration. Move per-slot `command`/`args` into `runtimes.claude`, `runtimes.codex`, or `runtimes.grok`;
4. Replace a string-valued slot `provider` with `{"source":"native"}` or `{"source":"cc-switch","app_type":"…","profile_id":"…"}`. Use `pairroom providers --json` to inspect the sanitized CC Switch catalog and disabled reasons;
5. If an existing schema-v1 Room depends on a former PairRoom Provider default, point the corresponding Service default slot at the equivalent CC Switch Profile before activating that Room.

Configuration containing removed Provider fields fails startup with migration guidance; it is never silently ignored. PairRoom does not copy old secrets into CC Switch and does not change the CC Switch current Profile.

Existing schema-v1 Rooms are read without modification and shown as `Legacy defaults`. New schema-v2 Rooms retain their immutable two-slot Agent selections. An older PairRoom binary fails closed on schema-v2 provisioning facts. To downgrade, stop the newer Service and restore the complete pre-upgrade data-root backup; do not copy individual Event Logs or edit schema numbers.

### Routing migration

The current Store schema is `9`. PairRoom rejects every older or newer Room before Event Log replay and provides no migration. Back up old Room data, keep the matching old binary if you need to inspect it, and create a new Room for the current release. Do not rewrite JSONL or metadata to fake a migration.

Remove `routing_mode` and `max_agent_hops` from JSON configuration and remove `--routing` / `--max-hops` from automation. The strict decoder and CLI reject those removed interfaces. HTTP clients must send only `steer` or `queue` Message intents; `steer` is the default. Old `append`, `next_turn`, and `supersede` values are invalid.

Workflow state, compilation, events, approval gates, and UI have been removed. Express the current task to one Agent, select Driver / Reviewer / Peer roles directly, and use native approvals. Agent relay now recognizes only runtime-derived exact handles: unique runtimes use `@claude`, `@codex`, or `@grok`; duplicate runtimes use stable `0/1` suffixes. Old aliases no longer route, and an unaddressed user send that relies on one is rejected; old control markers are ordinary text.

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
- a new Mock Room can complete exact-handle relay and a multi-Turn FIFO;
- backup verification succeeds;
- real mode first completes a read-only single-Agent Turn, then an explicitly addressed Reviewer Turn.

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
