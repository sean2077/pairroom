# Configuration

PairRoom configuration describes the local listener, Runtime policy, Runtime command templates, two default Agent slots, and optional read-only CC Switch database location. A complete runnable sample is [`examples/pairroom.example.json`](../examples/pairroom.example.json). The final interpretation of command-line flags is always `pairroom <command> --help`.

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

Each slot supplies a default `AgentSelection`: `runtime`, a structured `provider`, optional `model`, `effort`, `instructions`, Runtime-specific permission/approval/sandbox values, and `ordinary_reviewer_policy`. A new Room snapshots both selections; changing Service configuration later does not rewrite it. Existing schema-v1 Rooms have no selection snapshot, are shown as `Legacy defaults`, and continue to resolve the current Service defaults at activation.

`provider: {"source":"native"}` delegates Provider and credentials to the selected CLI's user/global configuration. Empty model, effort, instructions, permission, approval, and sandbox fields inherit native configuration. `ordinary_reviewer_policy` defaults to `enforced`; `explicit` is the dangerous opt-in that applies the selected Runtime policy to ordinary Reviewer Turns. Compiled plan, review, and audit Workflow stages remain read-only regardless of this option.

Commands are not part of a Room selection. `runtimes.claude`, `runtimes.codex`, and `runtimes.grok` each own one Service-level `command`/`args` template, preventing a Room request from selecting an executable.

Runtime policy fields are validated per Runtime: Claude Code accepts `permission_mode`; Codex accepts `approval_policy` (`untrusted`, `unless-trusted`, `unlessTrusted`, `on-failure`, `on-request`, or `never`) and `sandbox` (`read-only`, `workspace-write`, or `danger-full-access`); Grok Build accepts `permission_mode` and `sandbox` (`read-only`, `workspace`, `strict`, or `off`). Empty values inherit native configuration. Service runtime `args` templates must not preselect model, effort, permission, approval, sandbox, or bypass flags, because those values belong to the immutable Room selection and Workflow safety projection.

Recommendations:

- Keep credentials in the vendor CLI, environment variables, or a controlled Provider profile;
- Do not put API keys in command arguments, logs, Room messages, or the repository;
- Give the Reviewer read-only / plan boundaries; only the Driver uses write permission;
- After changing an executable or Provider, run Mock first, then a real read-only Turn;
- Keep Grok Build prompt and instruction text out of process argv (PairRoom writes them to a prompt file).

## CC Switch Provider references

PairRoom supports CC Switch v3.20.1/schema 18 through the CGo-free `modernc.org/sqlite` driver. It opens `~/.cc-switch/cc-switch.db` in SQLite read-only/query-only mode; `cc_switch.database` may override it only with an absolute path. PairRoom does not create or update this database, change `is_current`, manage Providers, or write live CLI configuration.

A CC Switch selection has the stable form `{"source":"cc-switch","app_type":"codex","profile_id":"…"}`. PairRoom re-reads the composite `(app_type, profile_id)` at creation validation and every Runtime activation. Supported profiles are directly materializable Claude Anthropic-compatible API-key profiles, Codex API-key custom Providers using the Responses wire API, and Grok Build direct custom-model profiles. Managed OAuth, proxy/protocol conversion, failover, unsupported applications, missing credentials, and malformed profiles remain visible in the Agent catalog but are disabled with a reason. A missing/deleted Profile, locked/unreadable database, or schema mismatch fails closed without fallback to cached or current Provider state.

Profile secrets exist only in the target child-process environment. Safe non-secret CLI overrides may select a Provider/model. For Grok Build, PairRoom creates a permission-restricted, secret-free Runtime overlay and points only the target process at it with `GROK_CONFIG_PATH`; the API key remains in that process environment. Secrets never enter argv, temporary configuration, Room data, Event Logs, the Registry checkpoint, RuntimeInfo, HTTP responses, browser state, diagnostics, or logs. Model suggestions come only from the selected Profile and Service defaults; PairRoom performs no network model discovery.

The former PairRoom `providers`, `cc_connect`, and string-valued Agent `provider` fields are removed. Configuration loading returns a migration error with a link to [Upgrading](UPGRADING.md).

## Service runtime policy

Service-level fields control the number of concurrently active Rooms, idle reclaim, reconcile, shutdown timeout, listen address, and token. They affect process lifecycle and do not change facts already committed to the Event Log. `--runtime-limit` defaults to 8, with a legal range of 1–128. Management Settings can adjust that cap while running (raising it starts queued items immediately; lowering it does not interrupt a running Turn). Idle timeout is still set by the startup flag.

## Source field inventory

The following JSON names are extracted from struct tags in `internal/config/`. This is a gap-finding list, not a substitute for field semantics and samples.

<!-- generated:config-fields -->
<details>
<summary>Show current JSON fields</summary>

- `approval_policy`
- `app_type`
- `args`
- `auto_start`
- `cc_switch`
- `claude`
- `codex`
- `command`
- `database`
- `effort`
- `grok`
- `instructions`
- `listen`
- `max_agent_hops`
- `model`
- `ordinary_reviewer_policy`
- `permission_mode`
- `profile_id`
- `provider`
- `room_name`
- `routing_mode`
- `runtime`
- `runtimes`
- `sandbox`
- `source`
- `stall_warning_seconds`
- `token`
</details>
<!-- /generated:config-fields -->

## Change checklist

When configuration fields change, update all of:

1. `examples/pairroom.example.json`;
2. the field semantics in this document;
3. `docs/UPGRADING.md` (if the change is breaking);
4. configuration parsing tests.
