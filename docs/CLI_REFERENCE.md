# CLI reference

CLI Reference describes command responsibilities and how to discover flags. It does not copy the full `--help` output of every subcommand. Exact defaults, allowed values, and platform differences always come from the current binary.

New standalone Rooms support `pairroom serve --collaboration default` (the default) or `--collaboration custom --collaboration-instructions "..."`. An existing Room restores its saved instructions; conflicting explicit flags fail. `pairroom protocol` prints the shared v6 mechanics; the removed `--role` flag no longer selects a collaboration mode.

## Top-level commands

| Command | Responsibility |
|---|---|
| `pairroom daemon` | Install and manage pairroom service in the OS service manager |
| `pairroom service` | Start the multi-Project / multi-Room Management Shell |
| `pairroom serve` | Start a current-format standalone Room |
| `pairroom doctor` | Validate Git and vendor CLI installation |
| `pairroom providers` | Read and validate the sanitized CC Switch Profile catalog without changing current state |
| `pairroom verify` | Strictly validate room data integrity |
| `pairroom backup` | Create a verified room-data backup |
| `pairroom restore` | Restore and verify a room-data backup |
| `pairroom diagnostics` | Generate a redacted diagnostics bundle |
| `pairroom relay` | Install approved project hooks, bind user-owned sessions, and publish/collect native relay messages |
| `pairroom protocol` | Print embedded v6 or `--host-mode native` v7 collaboration contracts |
| `pairroom version` | Print the build version |

Start every command with:

```bash
pairroom --help
pairroom <command> --help
```

## Common entry points

From a source checkout, stop leftover daemon ownership and open the current Management Shell:

```bash
make dev
```

That is `go run ./cmd/pairroom service --recover-stale-lock` after `make stop`. Mock Management Service with an installed binary:

```bash
pairroom service --mock
```

Do not open a browser automatically:

```bash
pairroom service --no-browser
```

Print a machine-readable Agent protocol:

```bash
pairroom protocol --json
```

Show version:

```bash
pairroom version
```

Project and Room lifecycle is managed by the `pairroom service` Management Shell and REST API, not by this CLI. Daemon, Backup, Restore, and similar commands have subcommands or dedicated flags. Do not guess flags from older docs; read `--help` at the matching command level.

Backup and Restore target one Room, not a multi-Room Service root. Backup and diagnostics output paths must be outside the source Room data directory. See [Operations](OPERATIONS.md#backup) for complete-Service rollback scope.

## Exit and errors

- Argument, configuration, and security-precondition errors return a non-zero exit code;
- CLI acceptance of a request does not mean a native Turn completed successfully;
- A destructive command should first show the target and preconditions; scripts must check the exit code and output;
- Removed routing and hop-limit flags are rejected; use the current command's `--help` instead of old automation examples.

## Source flag inventory

The following names are extracted from `cmd/pairroom/*.go`. Use them to find omissions; they do not mean every flag applies to every command.

<!-- generated:flags -->
- `--actor`
- `--attach`
- `--auto-start`
- `--cc-switch-db`
- `--claude-command`
- `--claude-effort`
- `--claude-instructions`
- `--claude-model`
- `--claude-permission-mode`
- `--claude-runtime`
- `--codex-approval-policy`
- `--codex-command`
- `--codex-effort`
- `--codex-instructions`
- `--codex-model`
- `--codex-runtime`
- `--codex-sandbox`
- `--collaboration`
- `--collaboration-instructions`
- `--config`
- `--continue`
- `--create`
- `--daemon-control-file`
- `--data-dir`
- `--data-root`
- `--database`
- `--discard`
- `--enabled`
- `--f`
- `--follow`
- `--force`
- `--grok-command`
- `--host-mode`
- `--id`
- `--idle-timeout`
- `--input`
- `--json`
- `--listen`
- `--live`
- `--log-file`
- `--mock`
- `--n`
- `--name`
- `--no-browser`
- `--output`
- `--peer-runtime`
- `--purge-hooks`
- `--recover-stale-lock`
- `--replace`
- `--repo`
- `--resend`
- `--room`
- `--runtime`
- `--runtime-limit`
- `--service-file`
- `--session-id`
- `--shutdown-timeout`
- `--slot`
- `--stall-warning-seconds`
- `--text`
- `--timeout`
- `--to`
- `--token`
<!-- /generated:flags -->

## Installation versus runtime availability

`pairroom doctor` is a Git / CLI protocol-metadata check, not an authentication or inference test. It checks the two configured slots, including Grok Build when selected; executable overrides are `--claude-command`, `--codex-command`, and `--grok-command` and follow the Runtime kind rather than the historical slot name.

```bash
pairroom doctor --config /absolute/path/pairroom.json --repo /absolute/path/project --json
# Explicit consent: this can consume Provider quota and create native sessions.
pairroom doctor --config /absolute/path/pairroom.json --live --json
```

`--live` uses each config-file Agent's Runtime, Provider, model, and effort. It starts a fresh native session in a disposable Git workspace, narrows native permissions, requests only a nonce response, and requires both the expected text and its matching input-completed event. It stops on a tool/approval request and attempts native shutdown and temporary-directory cleanup on every exit. Native global configuration, hooks, and MCP still apply: this is not an isolation sandbox or a tool-compatibility certification. Each Agent check has a 75-second budget plus bounded shutdown; Ctrl+C cancels it. The JSON adds separate `checks` for startup/response, and failures make the command exit nonzero. Existing `doctor` JSON still includes local paths; review it before sharing.

The Management **Settings → Diagnostics** section offers the same live check with explicit confirmation and cancellation, plus Service storage, Registry, Project, capacity, and three-CLI environment checks. Its default pair follows the saved Service default Agent pair profile; selecting a Room uses that Room's immutable selections. CLI `doctor` instead uses the supplied configuration file, not Service-scoped profiles. Neither entry point resumes an existing Room session. Mock results are explicitly unverified.

**Download safe report** exports only check codes/statuses, platform, numeric versions, timestamps, and timings. It excludes local paths, Room names/IDs, Provider details, native session IDs, command arguments, credentials, and raw process/model output. This report does not replace `pairroom diagnostics`, the existing redacted Room archive bundle.

## Native relay commands

Create a Room with host mode **Native** in Management, or let the first session create it: `pairroom relay bind --create` registers the Project when missing, creates the native Room through the same validated Management path, binds that session, and prints the peer's `peer_join` command. Keep `pairroom` on the native harness's PATH. In that Project's worktree, install and then approve the exact hook in each native harness (Codex: `/hooks`; Claude: project hook consent). Installation is explicit and preserves unrelated settings. The `pairroom-relay` skill ships in `skills/pairroom-relay/` and is installable through skill installers (`npx skills add sean2077/pairroom`); `relay install` writes the same canonical file, and a freshness test keeps the embedded projection identical. With the skill loaded, `/pairroom-relay <topic>` runs the create flow.

```bash
pairroom relay install --runtime claude          # inside a recognized session, --runtime is inferred
pairroom relay install --runtime codex
pairroom relay bind --create --name "<topic>"    # creator: project + native Room + bind; prints peer_join
pairroom relay bind                              # peer: zero-flag inside a recognized session
```

Run each bind from its intended native session, then include the returned one-time `bind_nonce` verbatim in that session's visible reply. The Stop hook associates the official session ID. Slots are Agent 1 / Agent 2: `--slot 1|2` is the primary form and the durable IDs `claude`/`codex` remain accepted; slot names never denote the selected Runtime. Omitted `--room` resolves the workspace's sole native Room; omitted `--slot` resolves only when exactly one Room slot runs the caller's harness runtime; anything ambiguous fails with the candidate list instead of guessing. Bind stdout contains no long-lived secret. No installed Stop hook means bind is rejected. Native configuration selections are display-only, and PairRoom never starts or interrupts either process.

A pending binding already occupies its slot. Repeating ordinary bind, including `--continue` before association, never returns its nonce again. Finish in the original session using the nonce it already received. If that output or the bind confirmation was lost, use `bind --replace` explicitly to revoke the pending generation and obtain a new nonce; this cannot stop any native work.

All per-slot commands accept `--repo <project> --room <id> --slot <slot>`. Foreground per-slot commands may omit `--room` and `--slot` together: a recognized native-harness caller must match exactly one recorded lineage, even when the workspace has only one binding. If the caller cannot be identified, only a sole workspace binding can be selected automatically. Missing matches and ambiguity require explicit flags; pre-lineage bindings remain usable with explicit flags. Lineage is a default selector, never authentication — every call still presents the slot's owner-only credential, and the hook path still requires the associated official session identity. `install` without `--runtime` infers claude/codex when run inside a recognized native session. For a custom Service root, give bind `--service-file <root>/relay-endpoint.json`; this is a **file path**, never a token. Later commands follow the saved path and re-read the endpoint after Service restart.

| Subcommand | Meaning |
|---|---|
| `bind` (zero-flag) | Inside a recognized native session: resolve the workspace's sole native Room and the slot whose runtime matches the caller's harness; ambiguity fails with candidates |
| `bind --continue --session-id <id>` | Resume the same associated session; a different session is rejected |
| `bind --replace` | Explicitly revoke an occupied generation and require a new nonce association; cannot stop old native work |
| `bind --create [--name <display-name>] [--runtime claude\|codex] [--peer-runtime claude\|codex]` | Without `--room`: register the workspace Project when missing, create a native Room through the same validated Management path the browser uses, bind this session, and print the peer's `peer_join` command. Without `--slot`, the creator's slot is inferred from the recognized harness (its runtime occupies the default-matching slot). Omitted runtimes keep the Service default pair; explicit runtimes stay empty-field selections that inherit the native configuration |
| `send --id <client-id> --text <body>` | Explicit message to peer; requires the completed association like every collection call; repeat the same ID after an uncertain response, never deduplicate by body |
| `send --to @user --attach <image>` | Human escalation with optional repeatable image paths; stdin supplies text when `--text` is absent |
| `wait --timeout 30` | Foreground collection for an associated session; stdout precedes ack; timeout leaves work queued |
| `status` / `peer` | Public delivery/binding state and original publication reconciliation / optional peer session references |
| `park --enabled=false` | Disable hook parking without removing association; foreground wait remains available |
| `nudge` | Print collection guidance only; no promise of native input injection |
| `reconcile` | Query pending publication: clear accepted, supplement definite absence with same sequence, otherwise retain unknown |
| `reconcile --resend` / `--discard` | Explicit decision on an unknown pending publication; resend retains original key, discard retains consumed sequence |
| `unbind --purge-hooks` | Revoke binding, remove slot files and remove only owned hooks when no other local slot uses them |
| `hook --runtime <kind>` | Official hook JSON on stdin; publishes Stop first, then bounded park; not a user-authored identity shortcut |

Park defaults to 30 seconds and at most eight consecutive message-bearing blocks. Outside this window, use foreground wait or a native human nudge. Unknown delivery must be inspected before the Room's explicit Retry. Same-turn send plus a peer-directed final reply deliberately creates two publications; omit that handle after send unless the second full reply is intended.

If creation succeeds but binding fails, the error preserves the Room ID and a recovery command with the Service/workspace paths; finish setup and use that command instead of repeating `--create`. The generated `peer_join` preserves those paths too. Generated commands use PowerShell quoting on Windows and POSIX shell quoting elsewhere. When creation itself cannot be confirmed, inspect Management before retrying to avoid duplicate Rooms.
