# Upgrading

PairRoom's CLI, Event Log, HTTP API, and native adapters evolve with the official CLIs. Treat an upgrade as a controlled change, not as overwriting a binary in place.

## Before upgrading

1. Read [CHANGELOG](../CHANGELOG.md);
2. Stop or archive active Rooms;
3. Create and verify a PairRoom data backup;
4. Record the current binary, Claude Code / Codex / Grok Build, and configuration versions;
5. Make sure the working repository has no unrecognized native side effects.

## Pending retry exclusion (Unreleased)

A retry request returns HTTP 409 while a direct retry of the same source Message and participant is already waiting or working. External clients should display the existing pending attempt instead of repeatedly submitting. Completed/failed/cancelled attempts still allow an explicit new retry. This change adds no schema, event kind, or native prompt; Store schema 10 / provisioning schema 3 remain unchanged. Normal stop/drain and reverting the code are sufficient to roll back this change without rewriting Room data.

## HTTP client adjustments (v3.0.0)

The HTTP reliability changes below preserve complete-response Agent relay. The collaboration update additionally changes native instructions and new-Room schemas as described in the next section.

For external HTTP/SSE clients, validate `message_limit` as an integer from 0 to 1000; invalid values now return HTTP 400 rather than being silently reinterpreted. Omitted or zero limits still request the full snapshot. On an SSE `reset` event, fetch a fresh snapshot and reconnect from its `latest_seq`; the server closes that stream because the cursor is ahead or older than the bounded replay tail. A non-empty `Last-Event-ID` takes precedence over `since`. See [API reference](API_REFERENCE.md) for the wire contract.

Native approval clients should render Grok's advertised options and send `decision: "option:<optionId>"`. One-time grants no longer fall back to remembered authorization, and cancellation is not a remembered rejection. Claude question responses must answer every exact native question text; incomplete or unknown answers fail without consuming the request. See [API reference](API_REFERENCE.md#native-approval-responses). Those approval fixes alone did not change schemas.

## Room names (v3.0.0)

The naming update adds no new Store/provisioning schema or event kind. Existing Room names and native IDs remain unchanged on upgrade; the normal next activation applies the Room-derived native display title where the CLI supports it. This includes explicitly bound existing sessions and can replace their old manual title. Unsupported/failed synchronization remains visible alongside the original session ID; upgrade the native CLI or check by that ID instead of assuming the display title changed.

New Room names are optional; omitted names are generated once and persist through restart. Right-click the Room in the sidebar, tabstrip, or Project list (or press Shift+F10) to rename it. Existing explicit Rename buttons remain available. Rename waits at the existing safe boundary, suspends without interrupting active work, and then commits. Reactivate normally to apply new native titles; dormant/archived Rooms are not started merely to rename them. Invalid and unchanged names no longer suspend a runtime. External clients must treat `runtime_names` as desired display metadata, not native IDs or synchronization receipts; see [API reference](API_REFERENCE.md#room-names-and-native-session-correspondence).

Reverting only this naming change does not restore a native title already changed through a vendor's naming API. Rename it in that native CLI if necessary. The earlier schema-10 downgrade restrictions below still apply.

## Collaboration modes and native permissions (v3.0.0)

New Rooms write **Store schema 10 / provisioning schema 3**. They choose only default Lead/Executor or custom natural-language instructions, fixed at creation. Both participants use the live workspace and default Service policy is YOLO; use the explicit creation controls or native configuration to narrow access. The new permission endpoint can change effective tool policy at an idle boundary without changing the mode or session identity.

Existing Store-schema-9 Rooms and provisioning-1/2 records remain readable with their original policy and workspace boundaries. Opening them does not relabel metadata, add a new mode, or silently grant YOLO. Legacy public role controls are removed; create a new Room to adopt the new collaboration model. No in-place data migration is required or performed. Earlier pre-schema-9 stores remain unsupported.

External clients must replace role controls with the independent permission endpoint for modern Rooms and must not send `target_role` or role aliases. The model-facing protocol is now v6: fixed identity/mode rules move into native instructions and dynamic envelopes contain only sender/body/media. Correlation IDs remain in transport and persistence, and message bodies/attachments are not summarized.

An old binary cannot safely read new schema-10 / provisioning-3 data. Keep and verify a complete pre-upgrade backup. For downgrade, stop/drain normally and restore that backup with its matching binary; never edit schema numbers or copy partial Event Logs.

## Earlier breaking boundaries still enforced

### Provider and Room provisioning migration

The earlier Provider update moved the root module to Go 1.25, replaced PairRoom-owned Provider configuration with read-only CC Switch v3.20.1/schema 18 references, and introduced provisioning schema 2. These Provider constraints remain; newly created Rooms now use schema 3.

Before installing the new binary:

1. Stop the Service after active Turns drain, then run `pairroom backup` for the complete data root and verify the archive;
2. Preserve that backup unchanged as the downgrade point;
3. Remove top-level `providers` and `cc_connect` configuration. Move per-slot `command`/`args` into `runtimes.claude`, `runtimes.codex`, or `runtimes.grok`;
4. Replace a string-valued slot `provider` with `{"source":"native"}` or `{"source":"cc-switch","app_type":"…","profile_id":"…"}`. Use `pairroom providers --json` to inspect the sanitized CC Switch catalog and disabled reasons;
5. If an existing schema-v1 Room depends on a former PairRoom Provider default, point the corresponding Service default slot at the equivalent CC Switch Profile before activating that Room.

Configuration containing removed Provider fields fails startup with migration guidance; it is never silently ignored. PairRoom does not copy old secrets into CC Switch and does not change the CC Switch current Profile.

Existing schema-v1 Rooms are read without modification and shown as `Legacy defaults`. Existing schema-v2 Rooms retain their immutable two-slot Agent selections. An older PairRoom binary fails closed on schema-v2 provisioning facts. To downgrade, stop the newer Service and restore the complete pre-upgrade data-root backup; do not copy individual Event Logs or edit schema numbers.

### Routing migration

The earlier routing redesign established Store schema `9`. The current reader accepts `9` and `10`, rejects all other schemas before Event Log replay, and does not migrate pre-9 Rooms. Keep their matching binary and backup for inspection; do not rewrite JSONL or metadata to fake a migration.

Remove `routing_mode` and `max_agent_hops` from JSON configuration and remove `--routing` / `--max-hops` from automation. The strict decoder and CLI reject those removed interfaces. HTTP clients must send only `steer` or `queue` Message intents; `steer` is the default. Old `append`, `next_turn`, and `supersede` values are invalid.

Workflow state, compilation, events, approval gates, and UI have been removed. Express the task to one Agent, use creation-time collaboration instructions, and retain native approvals. Agent relay now recognizes only runtime-derived exact handles: unique runtimes use `@claude`, `@codex`, or `@grok`; duplicate runtimes use stable `0/1` suffixes. Old aliases no longer route, and an unaddressed user send that relies on one is rejected; old control markers are ordinary text.

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
- real mode first completes a read-only single-Agent Turn, then an explicitly addressed peer review Turn.

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
