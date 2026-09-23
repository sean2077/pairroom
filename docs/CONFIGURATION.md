# Configuration

PairRoom configuration describes listeners, runtime policy, command templates, two default Agent selections, and an optional read-only CC Switch database. A runnable sample is [`examples/pairroom.example.json`](../examples/pairroom.example.json). The matching binary's `pairroom <command> --help` is authoritative for flags.

## Host-mode scope

Per-process Provider/model/effort/permission projection in this page applies to **Embedded** adapters. **Native** stores selections for identity/display but does not resolve Providers or apply overrides to already-running harnesses. Configure their effective models, credentials, and permissions in the original sessions; see [Native setup](NATIVE_RELAY.md).

Shared Service settings, saved pair templates, and creation-time collaboration are not permission to reconfigure a Native process. A desktop-owned embedded Service can serve either Room host mode.

## Load and override

Startup applies built-in defaults, then JSON configuration, then explicit flags for that command. In-Room changes affect only that Room, not the global file. The strict decoder rejects unknown/duplicate fields and malformed/null policy values. Read [Upgrading](UPGRADING.md) before breaking changes.

## Collaboration policy

Routing has no configurable hop limit or workflow mode. Embedded enforces one native Turn owner and one Room FIFO. Native uses per-slot FIFO with advisory ownership and user-owned execution. Automatic Agent relay requires the exact current peer handle; Native explicit send instead uses its command target. See [Concepts](CONCEPTS.md) and [Protocol](PROTOCOL.md).

Collaboration **instructions** have two creation-time choices: `default` Lead/Executor or `custom` natural-language rules (non-blank valid UTF-8, no NUL, at most 16 KiB after trimming). The Management form/API persists that choice. `pairroom serve --collaboration custom --collaboration-instructions "..."` supplies it for a new standalone Room. Reopening restores the stored instruction version; conflicting explicit choices fail. Saved mode/instructions cannot be edited in place, but newer task instructions can redirect current work.

`stall_warning_seconds` is an Embedded no-recent-runtime-event reminder, not evidence of a terminated Turn or a Native presence detector.

## Agent slots and runtimes

Top-level `claude` and `codex` keys name the Agent 1/2 default templates; they are not durable ActorIDs and do not select vendors. Durable slots are `slot1`/`slot2`. Each selection independently uses Runtime `claude`, `codex`, or `grok`, including duplicates.

Each default `AgentSelection` contains `runtime`, structured `provider`, optional `model`, `effort`, `instructions`, and applicable native permission/approval/sandbox values. Creation snapshots both; changing Service defaults does not rewrite an existing Room.

For Embedded, `provider: {"source":"native"}` delegates credentials/configuration to the selected CLI. Empty model/effort/instructions add no override. New defaults use Claude `permission_mode: yolo` and Codex `approval_policy: yolo` with `sandbox: danger-full-access`; Grok YOLO projects bypass with sandbox `off`. Both participants use these defaults regardless of responsibility. Explicit narrower values remain respected; empty policy fields request native inheritance. Restoring configured `yolo` with no sandbox completes full access; clear both fields to inherit native settings.

Effective Permission profiles (`configured`, `read-only`, `yolo`) can change only in an idle **Embedded Room** with no queued work or pending approval. Stored creation-time values remain immutable. Native Room controls do not change effective permissions.

Commands are not Room selections. Service templates `runtimes.claude`, `runtimes.codex`, and `runtimes.grok` own executable `command`/`args`, preventing a Room request from choosing an executable. Arguments must not preselect model, effort, approval, permission, sandbox, or bypass values that belong to selection/policy projection.

| Runtime | Accepted policy values |
|---|---|
| Claude Code | `permission_mode`: `default`, `manual`, `acceptEdits`, `plan`, `auto`, `dontAsk`, `bypassPermissions`, `yolo` |
| Codex | `approval_policy`: `untrusted`, `unless-trusted`, `unlessTrusted`, `on-failure`, `on-request`, `never`, `yolo`; `sandbox`: `read-only`, `workspace-write`, `danger-full-access` |
| Grok Build | `permission_mode`: `default`, `ask`, `acceptEdits`, `plan`, `auto`, `dontAsk`, `bypassPermissions`, `always-approve`, `yolo`; `sandbox`: `read-only`, `workspace`, `strict`, `off` |

`yolo` projects Claude bypass plus `--dangerously-skip-permissions`, Codex `never` plus full-access sandbox, or Grok `--always-approve` plus sandbox `off`. These are PairRoom's supported mappings, not certification of every future CLI release. Keep secrets out of argv/logs/messages/repositories; use supported read-only/plan restrictions for untrusted review rather than relying on Lead/Executor labels. After changing executable/Provider, check deterministic behavior and separately test an authorized real read-only Turn.

Grok prompts/instructions travel over long-lived ACP, never argv. New sessions receive `_meta.rules`; exact resumption receives bootstrap once in its first PairRoom prompt without replacing the native system prompt.

## Agent pair profiles

A named pair is reusable across Projects in one Service. In **Settings → Agent pair profiles**, create/edit/rename/delete or set/clear the default, even before registering a Project. Create Room can load a profile or **Service defaults (no profile)**; **Save or update this pair** persists the current controls explicitly. Browser and Desktop share storage/API.

Profiles contain each slot's Runtime, Provider reference, model, effort, additional instructions, and applicable policy values. They contain no resolved native credentials, command/args, Project/path, Room name, session/Binding, or collaboration mode. Do not place secrets in names/instructions. A pair profile is distinct from a CC Switch Provider Profile and an effective Permission profile.

Selection fills controls without saving temporary edits. Room creation copies the final pair; Embedded additionally revalidates/materializes supported Provider references. Editing/deleting a profile never changes existing Rooms or Native processes. Deleting the default clears it rather than selecting another arbitrary profile. Missing/unsupported Providers remain visible; Embedded creation fails rather than falling back, while saved templates remain repairable.

Profiles live in `<service data root>/agent-pair-profiles.json`, separate from startup JSON, browser storage, and the rebuildable Registry. Different roots have independent profiles; standalone `pairroom serve` does not read them. Names are non-blank, unique case-insensitively, at most 160 UTF-8 bytes without control characters; up to 100 profiles are supported. Use [Management API](API_REFERENCE.md#agent-pair-profiles) for automation.

## CC Switch Provider references

The implemented Embedded integration supports CC Switch v3.20.1/schema 18 through CGo-free `modernc.org/sqlite`. It opens `~/.cc-switch/cc-switch.db` read-only/query-only; `cc_switch.database` overrides it with an absolute path. PairRoom never creates/updates that database, switches `is_current`, manages Providers, or writes live CLI configuration.

A reference has the form `{"source":"cc-switch","app_type":"codex","profile_id":"…"}`. Embedded creation/activation re-reads `(app_type, profile_id)`. Supported cases are directly materializable Claude Anthropic-compatible API-key profiles, Codex API-key custom Providers using Responses, and Grok direct custom-model profiles. Managed OAuth, proxy/protocol conversion, failover, unsupported applications, missing credentials, and malformed data remain disabled with reasons. A deleted/missing Profile, database/schema failure, or unreadable configuration fails closed, never falling back to cached/current credentials.

Secrets exist only in the selected child environment. Safe non-secret flags may select Provider/model. Grok uses a permission-restricted secret-free runtime overlay selected by `GROK_CONFIG_PATH`; the key stays in the environment. Secrets never enter argv, temporary configuration, Room/Event Log, Registry, RuntimeInfo, API, browser, diagnostics, or logs.

A bounded control-character-free, credential-redacted `provider_name` may appear in projections/history to identify the Profile without exposing its internal reference. A name containing its own credential fails materialization. Suggestions come from that Profile and Service defaults; no network model discovery is performed. Native host mode does not apply these materialization steps to an existing session.

Removed `providers`, `cc_connect`, and string-valued Agent `provider` fields produce an upgrade error; see [Upgrading](UPGRADING.md).

## Service runtime policy

Service policy controls Embedded capacity, idle reclaim, reconciliation, shutdown, listen address, and token without changing committed Room facts. `--runtime-limit` defaults to 8 and accepts 1–128. Management can raise it to admit queued work or lower it without preempting running Turns. Idle timeout remains a startup flag.

Native relay Rooms do not consume this adapter-capacity budget or become capacity-eviction victims. Their wake toggle is a per-Room Management operation at an idle boundary, not a Provider override or a relay-credential capability. Exact wake limits belong in [Protocol](PROTOCOL.md#automatic-idle-peer-wake).

## Source field inventory

These JSON names come from configuration/model struct tags. The list identifies gaps, not field semantics.

<!-- generated:config-fields -->
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
<!-- /generated:config-fields -->

## Change checklist

Field changes update the sample, semantics, strict parser tests, and [Upgrading](UPGRADING.md) when breaking. Keep the inventory aligned with source. Prose-only host-mode corrections do not change JSON fields or supported formats.
