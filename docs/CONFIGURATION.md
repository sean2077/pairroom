# Configuration

PairRoom configuration describes the local listener, Runtime policy, two native Agent slots, and optional Providers. A complete runnable sample is [`examples/pairroom.example.json`](../examples/pairroom.example.json). The final interpretation of command-line flags is always `pairroom <command> --help`.

## Load and override

Startup applies built-in defaults first, then the JSON configuration, then explicit CLI flags for the current command. Settings that can be changed inside a Room apply only to that Room and must not be treated as a write-back of the global configuration file.

The JSON decoder rejects unknown fields. Spelling mistakes are therefore not silently ignored, but you must read [Upgrading](UPGRADING.md) before a breaking release.

## Collaboration policy

`routing_mode` accepts only:

```json
{"routing_mode": "turns"}
```

`manual`, `mentions`, and `roundtable` are removed and are not migrated. `max_agent_hops` limits the number of Agent Turns in one automatic relay chain. `stall_warning_seconds` only controls the “no Runtime event for a long time” reminder; silence alone does not mean the Turn has terminated.

## Agent slots and runtimes

The JSON keys `claude` and `codex` are durable Agent 1 and Agent 2 slots, not vendor identities. Each slot has a `runtime` of `claude`, `codex`, or `grok`. Both slots may select the same runtime.

`provider`, `model`, `effort`, and `instructions` are explicit overrides. Empty values inherit the selected native CLI's user/global configuration. Permission, approval, and sandbox fields work the same way when left empty: PairRoom does not synthesize a vendor default. PairRoom maps explicit settings onto the official CLI, but vendor support still depends on the locally installed version.

Recommendations:

- Keep credentials in the vendor CLI, environment variables, or a controlled Provider profile;
- Do not put API keys in command arguments, logs, Room messages, or the repository;
- Give the Reviewer read-only / plan boundaries; only the Driver uses write permission;
- After changing an executable or Provider, run Mock first, then a real read-only Turn;
- Keep Grok Build prompt and instruction text out of process argv (PairRoom writes them to a prompt file).

## Provider and cc-connect

A Provider profile can describe an endpoint, model alias, environment-variable mapping, and Codex wire API. `cc_connect` only references an existing provider source; it must not copy long-lived credentials into the Room Event Log. Import collisions need an explicit prefix or rename; later-loaded values must not silently overwrite earlier ones.

## Service runtime policy

Service-level fields control the number of concurrently active Rooms, idle reclaim, reconcile, shutdown timeout, listen address, and token. They affect process lifecycle and do not change facts already committed to the Event Log. `--runtime-limit` defaults to 8, with a legal range of 1–128. Management Settings can adjust that cap while running (raising it starts queued items immediately; lowering it does not interrupt a running Turn). Idle timeout is still set by the startup flag.

## Source field inventory

The following JSON names are extracted from struct tags in `internal/config/`. This is a gap-finding list, not a substitute for field semantics and samples.

<!-- generated:config-fields -->
<details>
<summary>Show current JSON fields</summary>

- `agent_model_lists`
- `agent_models`
- `agent_types`
- `alias`
- `api_key`
- `approval_policy`
- `args`
- `auto_start`
- `base_url`
- `cc_connect`
- `claude`
- `codex`
- `command`
- `effort`
- `endpoints`
- `env`
- `env_key`
- `http_headers`
- `imported_from`
- `instructions`
- `listen`
- `max_agent_hops`
- `model`
- `models`
- `name`
- `path`
- `permission_mode`
- `prefix`
- `provider`
- `providers`
- `room_name`
- `routing_mode`
- `runtime`
- `sandbox`
- `stall_warning_seconds`
- `thinking`
- `token`
- `wire_api`
</details>
<!-- /generated:config-fields -->

## Change checklist

When configuration fields change, update all of:

1. `examples/pairroom.example.json`;
2. the field semantics in this document;
3. `docs/UPGRADING.md` (if the change is breaking);
4. configuration parsing tests.
