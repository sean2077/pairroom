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

Top-level `claude` and `codex` keys name the Agent 1/2 default templates; they are not durable ActorIDs and do not select vendors. Durable slots are `slot1`/`slot2`. Each selection independently uses Runtime `claude`, `codex`, `grok`, or `gemini`, including duplicates.

Each default `AgentSelection` contains `runtime`, structured `provider`, optional `model`, `effort`, `instructions`, and applicable native permission/approval/sandbox values. Creation snapshots both; changing Service defaults does not rewrite an existing Room.

For Embedded, `provider: {"source":"native"}` delegates credentials/configuration to the selected CLI. Empty model/effort/instructions add no override. New defaults use Claude `permission_mode: yolo` and Codex `approval_policy: yolo` with `sandbox: danger-full-access`; Grok and Gemini YOLO project native approval bypass with sandbox `off`. Both participants use these defaults regardless of responsibility. Explicit narrower values remain respected; empty policy fields request native inheritance. Restoring configured `yolo` with no sandbox completes full access; clear both fields to inherit native settings.

Effective Permission profiles (`configured`, `read-only`, `yolo`) can change only in an idle **Embedded Room** with no queued work or pending approval. Stored creation-time values remain immutable. Native Room controls do not change effective permissions.

Commands are not Room selections. Service templates `runtimes.claude`, `runtimes.codex`, `runtimes.grok`, and `runtimes.gemini` own executable `command`/`args`, preventing a Room request from choosing an executable. Arguments must not preselect model, effort, approval, permission, sandbox, or bypass values that belong to selection/policy projection.

On Windows, a command that resolves to a `.cmd`/`.bat` launcher (the npm shim form) runs through `cmd.exe`, which re-parses the command line. Embedded startup therefore fails closed when any argument for such a launcher, including model, effort, template args, and CC Switch provider arguments, contains `&`, `|`, `<`, `>`, `^`, `%`, `!`, or a control character; the error names the value. The Claude session name, which is display metadata derived from the Room name, is neutralized instead, with those characters and `"` replaced by their full-width forms. To use such a value, point the template command at the CLI executable rather than its shim.

| Runtime | Accepted policy values |
|---|---|
| Claude Code | `permission_mode`: `default`, `manual`, `acceptEdits`, `plan`, `auto`, `dontAsk`, `bypassPermissions`, `bypass`, `always-approve`, `yolo` (`bypass` and `always-approve` are accepted aliases of `bypassPermissions`) |
| Codex | `approval_policy`: `untrusted`, `unless-trusted`, `unlessTrusted`, `on-failure`, `on-request`, `never`, `yolo`; `sandbox`: `read-only`, `workspace-write`, `danger-full-access` |
| Gemini CLI | `permission_mode`: `default`, `auto_edit`, `plan`, `yolo`; `sandbox`: `on`, `off`; no effort or approval-policy override, native Provider only |
| Grok Build | `permission_mode`: `default`, `ask`, `acceptEdits`, `plan`, `auto`, `dontAsk`, `bypassPermissions`, `always-approve`, `yolo`; `sandbox`: `read-only`, `workspace`, `strict`, `off` |

`yolo` projects Claude bypass plus `--dangerously-skip-permissions`, Codex `never` plus full-access sandbox, Grok `--always-approve` plus sandbox `off`, or Gemini `--approval-mode yolo` plus boolean sandbox `false`. These are PairRoom's supported mappings, not certification of every future CLI release. Keep secrets out of argv/logs/messages/repositories; use supported read-only/plan restrictions for untrusted review rather than relying on Lead/Executor labels. After changing executable/Provider, check deterministic behavior and separately test an authorized real read-only Turn.

Grok prompts/instructions travel over long-lived ACP, never argv. New sessions receive `_meta.rules`; exact resumption receives bootstrap once in its first PairRoom prompt without replacing the native system prompt.

Claude Code receives the PairRoom system prompt through `--append-system-prompt-file` (a private file under the Room data directory) whenever `claude --help` advertises it, including the `--append-system-prompt[-file]` notation current releases use. Only a CLI that advertises just `--append-system-prompt` gets the prompt as an argument; on Windows, startup then fails with a clear error if the command line would exceed the operating-system limit (32,767 characters, or 8,191 through a `.cmd` launcher).

Gemini uses official ACP without calling the state-changing `authenticate` RPC. Configure/login through its native CLI before Embedded activation. Empty policy overrides preserve the reported native mode; `plan` does not disable an inherited sandbox. Explicit sandbox choices override `GEMINI_SANDBOX` only in the selected child environment; empty sandbox retains native inheritance. Unsupported effort and CC Switch references are rejected. Embedded exact resume is blocked until a reliable replay-completion boundary is supported; the accepted Binding is preserved. See [Gemini boundaries](NATIVE_RELAY.md#gemini-cli).

## Agent pair profiles

A named pair is reusable across Projects in one Service. In **Settings → Agent pair profiles**, create/edit/rename/delete or set/clear the default, even before registering a Project. Create Room can load a profile or **Service defaults (no profile)**; **Save or update this pair** persists the current controls explicitly. Browser and Desktop share storage/API.

Profiles contain each slot's Runtime, Provider reference, model, effort, additional instructions, and applicable policy values. They contain no resolved native credentials, command/args, Project/path, Room name, session/Binding, or collaboration mode. Do not place secrets in names/instructions. A pair profile is distinct from a CC Switch Provider Profile and an effective Permission profile.

Selection fills controls without saving temporary edits. Room creation copies the final pair; Embedded additionally revalidates/materializes supported Provider references. Editing/deleting a profile never changes existing Rooms or Native processes. Deleting the default clears it rather than selecting another arbitrary profile. Missing/unsupported Providers remain visible; Embedded creation fails rather than falling back, while saved templates remain repairable.

Profiles live in `<service data root>/agent-pair-profiles.json`, separate from startup JSON, browser storage, and the rebuildable Registry. Different roots have independent profiles; standalone `pairroom serve` does not read them. Names are non-blank, unique case-insensitively, at most 160 UTF-8 bytes without control characters; up to 100 profiles are supported. Use [Management API](API_REFERENCE.md#agent-pair-profiles) for automation.

## CC Switch Provider references

The Embedded integration reads CC Switch through CGo-free `modernc.org/sqlite`. Verified schemas are 18 (v3.20.1), 19 (v3.20.4), and 20 ([v4.0.4](https://github.com/farion1231/cc-switch/releases/tag/v4.0.4)), checked against upstream `providers` columns and `settings_config`/`meta` semantics. A newer schema is accepted when every required column remains present; the catalog reports `schema_verified: false` and Management shows an unverified-schema notice. Per-Profile validation still applies. Schemas older than 18 (`cc_switch_schema_mismatch`) and missing required columns (`cc_switch_schema_incompatible`) fail closed.

The database opens read-only/query-only at `~/.cc-switch/cc-switch.db`. Set `cc_switch.database` to an absolute path if CC Switch stores its database elsewhere. PairRoom never creates or updates that database, switches its current Provider, or writes live CLI configuration. CC Switch is optional: native Provider selections work when the database is absent; Management reports that absence only for a slot referencing CC Switch, while other database failures remain visible. The catalog's `current` field mirrors the database `is_current` marker, independent of CC Switch's device-local current Provider, routing, or aggregation state.

A reference such as `{"source":"cc-switch","app_type":"codex","profile_id":"…"}` identifies a row in upstream `providers`; Embedded creation/activation re-reads `(app_type, profile_id)`. CC Switch's separate project snapshots in `profiles`, including their MCP/Skills/prompt selections, are outside this adapter. Supported cases are direct Claude Anthropic-compatible API-key profiles, Codex API-key custom Providers using Responses, and Grok direct custom-model profiles. A direct Provider remains usable when it also belongs to a CC Switch failover queue or aggregation list. PairRoom does not reproduce those routing modes. For routing, protocol conversion, or managed OAuth, select Provider source `native` with the CLI already configured through CC Switch. Missing/deleted references, unavailable credentials, unsupported credential sources or applications, and malformed data fail closed without falling back to another Provider.

[CC Switch 4 keeps shared configuration in the native CLI files](https://github.com/farion1231/cc-switch/blob/v4.0.4/src-tauri/src/live/floor.rs). PairRoom projects selected Provider-owned fields and ignores shared hooks, permissions, MCP, plugins, and instructions left in older Provider snapshots. Claude and Codex continue loading their own shared configuration. Grok uses a standalone overlay containing only the selected model; PairRoom does not merge the native Grok configuration file into that overlay. An explicit slot model or effort wins over the Profile default. Codex defaults come from top-level TOML `model` and `model_reasoning_effort`; absent defaults retain native inheritance. Model suggestions include Codex `settings_config.modelCatalog.models[].model` and Claude `meta.stackModels[].model`, including Claude's `[1M]` marker. Those lists supply model IDs only, without importing their capability metadata, display text, or instructions. No network model discovery is performed.

Codex credentials follow the [v4.0.4 projection contract](https://github.com/farion1231/cc-switch/blob/v4.0.4/src-tauri/src/live/project/codex.rs): an explicitly declared `env_key` must resolve in the Service environment; otherwise PairRoom uses `auth.OPENAI_API_KEY`, then the selected table's `experimental_bearer_token`, then the top-level bearer token. An unavailable or incorrectly typed credential declaration cannot fall back to a stored key. Quoted provider table names, dotted scalar assignments, literal-string scalars, and legacy `openai_base_url` configurations are supported. For `openai` or an omitted Provider selector, the legacy endpoint supplies missing route defaults in both table and dotted form; this merge never replaces a declared field, including empty or invalid values. An empty `wire_api` retains the existing `meta.apiFormat` fallback. The selected key is passed through a private child environment variable and a per-process `env_key` override with `requires_openai_auth=false`, independent of the source scalar value for that flag.

Selected Codex context limits, review/plan settings, and subagent/model options are projected without copying arbitrary TOML. Dotted options such as `agents.default_subagent_model` have the same meaning as their table form. The top-level `model` and `model_reasoning_effort` defaults, Provider selector, routing, and credential fields require strings. Codex Provider display names and string-valued projected options retain their historical scalar-text conversion, including `web_search = true` becoming `-c web_search="true"` and `review_model = 123` becoming `-c review_model="123"`; the Runtime still owns its accepted option values. Boolean and integer options retain compatibility with quoted values such as `disable_response_storage = 'false'` and `model_context_window = '262144'`; PairRoom emits canonical booleans and integers, and context limits must be positive. Incompatible selected declarations, including a table or inline table in place of a scalar, fail validation instead of disappearing and inheriting native defaults.

Grok accepts both table and dotted declarations for its selected custom model. Model selectors and selected model, routing, and credential fields require strings; a numeric-looking alias such as `'123'` remains valid when quoted. `context_window` must parse as a positive integer and retains support for quoted values with surrounding whitespace. A nonempty selected `api_key` takes precedence over `env_key`.

Credential-container validation follows the fields read for the selected Provider. An unrelated table name such as `[model_providers.other.env_key]` or `[mcp_servers.secret]` does not disable it. Known credential values across the entire Profile still participate in public metadata redaction and collision checks, including scalar descendants of credential-shaped TOML tables and JSON containers belonging to unselected Providers. Redaction follows TOML path components, preserving quoted Provider IDs and literal dotted keys. An `env_key` descendant is inspected as an environment-variable reference, not as a literal secret. Unsupported opaque authentication data, including credential-shaped data in nested configuration snapshots, still withholds the row's authored metadata and Provider reference. Malformed active TOML also fails inspection; unrelated free-form metadata is not validated as active Provider configuration.

Claude references require **Claude Code 2.1.222 or newer**, the [host model-selection isolation baseline](https://github.com/anthropics/claude-code/blob/71cdddec623889d38af14b7a489670a03186f659/CHANGELOG.md#21222). PairRoom removes conflicting inherited Provider variables from that child and sets `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1`; native Provider selections retain ordinary CLI behavior. Supported compatibility switches, including `CLAUDE_CODE_AUTO_MODE_SERVER`, are applied through the child environment and a secret-free `--settings` overlay that takes precedence over user, project, and local `settings.env`. Managed settings retain their precedence for compatibility switches outside the host-protected Provider/model keys, and native permission policy still applies. Cloud/managed OAuth authentication, credential helpers, custom authentication headers, extra request bodies, and `PROXY_MANAGED` placeholder credentials require native configuration rather than independent Profile materialization.

Secrets exist only in the selected child environment. Safe non-secret flags may select Provider/model/options; Grok uses a permission-restricted secret-free overlay selected by `GROK_CONFIG_PATH`. Secrets never enter argv, temporary configuration, Room/Event Log, Registry, RuntimeInfo, API, browser, diagnostics, or logs. A bounded control-character-free, credential-redacted `provider_name` may identify the Profile in projections/history; a name containing its own credential fails materialization. Native host mode keeps effective configuration in the original session.

Removed `providers`, `cc_connect`, and string-valued Agent `provider` fields produce an upgrade error; see [Upgrading](UPGRADING.md).

## Service runtime policy

Service policy controls Embedded capacity, idle reclaim, reconciliation, shutdown, listen address, and token without changing committed Room facts. `--runtime-limit` defaults to 8 and accepts 1–128. Management can raise it to admit queued work or lower it without preempting running Turns. Idle timeout remains a startup flag.

`resume_pending` (default `true`; flag `--resume-pending`) makes a starting Service request activation, once, for suspended Rooms whose Event Log shows work only an active Room Runtime can move: an Embedded Room-owned FIFO input that never crossed native submission, or a wake-enabled Native slot with unattempted queued input for its bound session. Detection replays each log read-only and appends nothing; activation then applies the ordinary restore rules, so accepted or uncertain input is never replayed and wake keeps its reservation and rate limits. Embedded resumes still queue behind the capacity limit. Set it to `false` to leave every Room suspended until it is opened or called.

`notify_command` is an optional argv (for example `["notify-send", "PairRoom"]` or a script path), never a shell string. The Service runs it once per [attention notification](API_REFERENCE.md#attention-notifications) with that notification's body-free JSON on stdin and a 10-second limit. Failures are ignored and never affect Room state; notifications raised while an earlier command is still running may be skipped rather than queued without bound. Closing the notifier cancels the in-flight command and discards queued commands instead of draining a separate timeout for each item. The command runs with the Service's own environment and permissions, so point it only at a program you trust.

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
- `gemini`
- `grok`
- `instructions`
- `listen`
- `model`
- `notify_command`
- `permission_mode`
- `profile_id`
- `provider`
- `resume_pending`
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
