# Configuration

PairRoom configuration describes the local listener, Runtime policy, Runtime command templates, two default Agent slots, and optional read-only CC Switch database location. A complete runnable sample is [`examples/pairroom.example.json`](../examples/pairroom.example.json). The final interpretation of command-line flags is always `pairroom <command> --help`.

## Load and override

Startup applies built-in defaults first, then the JSON configuration, then explicit CLI flags for the current command. Settings that can be changed inside a Room apply only to that Room and must not be treated as a write-back of the global configuration file.

The JSON decoder rejects unknown fields. Spelling mistakes are therefore not silently ignored, but you must read [Upgrading](UPGRADING.md) before a breaking release.

## Collaboration policy

Collaboration routing has no configurable mode or hop limit. PairRoom always enforces one native Turn owner and one Room FIFO. An Agent response relays only when it contains the other participant's exact current runtime-derived handle; without that handle the relay ends. See [Core concepts](CONCEPTS.md) for duplicate-runtime suffixes and steer fallback semantics.

Collaboration **instructions** have only two creation-time modes: `default` Lead/Executor or `custom` natural-language rules (non-blank, valid UTF-8, no NUL, at most 16 KiB after trimming). This is separate from routing. The Management create form and Room creation API persist the choice; `pairroom serve --collaboration custom --collaboration-instructions "..."` supplies it for a new standalone Room. Reopening restores the stored choice; an explicitly conflicting choice fails. Mode and instructions cannot be edited later.

`stall_warning_seconds` only controls the “no Runtime event for a long time” reminder; silence alone does not mean the Turn has terminated.

## Agent slots and runtimes

The JSON keys `claude` and `codex` are durable Agent 1 and Agent 2 slots, not vendor identities. Each slot has a `runtime` of `claude`, `codex`, or `grok`. Both slots may select the same runtime.

Each slot supplies a default `AgentSelection`: `runtime`, a structured `provider`, optional `model`, `effort`, `instructions`, Runtime-specific permission/approval/sandbox values. A new Room snapshots both selections; changing Service configuration later does not rewrite it. Existing schema-v1 Rooms have no selection snapshot, are shown as `Legacy defaults`, and continue to resolve the current Service defaults at activation.

`provider: {"source":"native"}` delegates Provider and credentials to the selected CLI's user/global configuration. Empty model, effort, and per-Agent instructions add no override. New Service defaults use Claude `permission_mode: yolo` and Codex `approval_policy: yolo` with `sandbox: danger-full-access`; Grok YOLO projects bypass and sandbox `off`. Both default-mode participants use these permissions, regardless of responsibility. Explicit narrower settings remain respected; explicitly empty permission/approval/sandbox fields inherit native configuration. When restoring a configured policy, an explicit `yolo` with no sandbox completes the full-access sandbox override; clear both fields to request native inheritance.

`ordinary_reviewer_policy` is a deprecated legacy-read field. It remains meaningful for old Rooms with role-bound workspaces, but is omitted from new selections and the creation form. It does not create a third collaboration mode. Modern permission controls select `configured`, `read-only`, or `yolo` at an idle boundary; the configured creation-time values themselves stay immutable.

Commands are not part of a Room selection. `runtimes.claude`, `runtimes.codex`, and `runtimes.grok` each own one Service-level `command`/`args` template, preventing a Room request from selecting an executable.

Runtime policy fields are validated per Runtime: Claude Code accepts `permission_mode` (`default`, `manual`, `acceptEdits`, `plan`, `auto`, `dontAsk`, `bypassPermissions`, or `yolo`); Codex accepts `approval_policy` (`untrusted`, `unless-trusted`, `unlessTrusted`, `on-failure`, `on-request`, `never`, or `yolo`) and `sandbox` (`read-only`, `workspace-write`, or `danger-full-access`); Grok Build accepts `permission_mode` (`default`, `ask`, `acceptEdits`, `plan`, `auto`, `dontAsk`, `bypassPermissions`, `always-approve`, or `yolo`) and `sandbox` (`read-only`, `workspace`, `strict`, or `off`). `yolo` is the Room-level bypass alias: Claude Code projects `bypassPermissions` plus `--dangerously-skip-permissions`, Codex projects `never` and full-access sandbox for default YOLO, and Grok Build projects `--always-approve` with sandbox `off`. Empty values inherit native configuration. Service runtime `args` templates must not preselect model, effort, permission, approval, sandbox, or bypass flags, because those values belong to the immutable Room selection and independent native permission projection.

Recommendations:

- Keep credentials in the vendor CLI, environment variables, or a controlled Provider profile;
- Do not put API keys in command arguments, logs, Room messages, or the repository;
- Use explicit read-only / plan permissions for untrusted review work; Lead / Executor responsibilities alone do not restrict tools;
- After changing an executable or Provider, run Mock first, then a real read-only Turn;
- Keep Grok Build prompt and instruction text out of process argv. PairRoom uses the long-lived ACP stdio protocol, projects new-session collaboration rules through `_meta.rules`, and injects a bootstrap once when exactly loading an existing session.

## Agent pair profiles

An **Agent pair profile** saves a named pair of Agent selections across Projects in the same Management Service. In **Settings → Agent pair profiles**, create, edit/rename, delete, or set/clear the default. Profile editing also works before registering a Project. In **Create Room**, select a profile or use **Service defaults (no profile)**; **Save or update this pair** saves the current controls as a new profile or explicitly updates the selected one. The shared browser and Desktop Management UI use the same storage and API.

A profile includes each slot's Runtime/harness, Provider reference, model, effort, additional instructions, and applicable permission/approval/sandbox overrides. Both slots may use the same Runtime. Empty overrides retain native inheritance; no resolved global model, credential, command, or Runtime arguments are captured. Do not put secrets in names or instructions. Profiles do not save Projects, paths, Room names, session IDs/Bindings, or collaboration modes. A pair profile is distinct from a CC Switch Provider Profile and from a Room participant's Permission profile.

New Room forms fill the saved default automatically. Selecting a profile only fills the controls: changes are temporary unless explicitly saved. Room creation copies the final pair into the immutable Room selections and revalidates Provider references. Editing, renaming, or deleting a profile never changes existing Rooms. Deleting the default clears it rather than arbitrarily choosing another profile; with no default, new Rooms use Service defaults. A missing Provider stays visible and blocks creation rather than silently falling back; saved profiles remain editable while a Provider is unavailable.

Profiles are Service user configuration in `<service data root>/agent-pair-profiles.json`, not fields in the startup JSON file and not browser local storage. The file survives restart and Registry-index rebuild. Different Service data roots have independent profiles; the legacy standalone `pairroom serve` does not read them. Up to 100 names are accepted, unique case-insensitively, non-blank, at most 160 UTF-8 bytes and without control characters. Use the [Management API](API_REFERENCE.md#agent-pair-profiles) for programmatic management.

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

- `app_type`
- `approval_policy`
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
- `model`
- `ordinary_reviewer_policy`
- `permission_mode`
- `profile_id`
- `provider`
- `room_name`
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
