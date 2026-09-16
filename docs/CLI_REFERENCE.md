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
- `--brief`
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

Run create, bind and foreground commands through the intended native harness's command tool (Claude Code, Codex CLI/Desktop or Grok Build), not a separate terminal. The current Git workspace and native session metadata supply defaults. PairRoom does not start a replacement Agent process. For ownership, runtime limits, audit fixes and the evidence behind efficiency claims, see [Native relay](NATIVE_RELAY.md).

Create a Room with host mode **Native** in Management, or let the first session create it: `pairroom relay bind --create` registers the Project when missing, creates the native Room through the same validated Management path, binds that session, and prints the canonical `peer_join` command plus `peer_join_local` for a peer on the same machine, Service data root and workspace. Keep `pairroom` on the native harness's PATH. In that Project's worktree, install and then approve the exact hook in each native harness (Codex: `/hooks`; Claude: project hook consent). Installation is explicit and preserves unrelated settings. The `pairroom-relay` skill ships in `skills/pairroom-relay/` and is installable through skill installers (`npx skills add sean2077/pairroom`); `relay install` writes the same canonical file, and a freshness test keeps the embedded projection identical. With the skill loaded, `/pairroom-relay <topic>` runs the create flow.

```bash
pairroom relay install                           # external OK; prompts a multi-select at a terminal, otherwise pass --runtime
pairroom relay install --runtime claude,codex     # comma-separated; cc|claude, codex, grok (each runtime gets its own hooks; grok uses .grok/hooks)
pairroom relay bind --create --name "<topic>"    # creator: project + native Room + bind; prints peer_join and peer_join_local
pairroom relay bind                              # peer: zero-flag inside a recognized session
```

`relay install` does not require a native session: run it from any terminal in the Project's worktree. With `--runtime` it installs those harnesses (comma-separated `cc|claude`, `codex`, `grok`); inside a recognized session it infers the harness; at an interactive terminal without `--runtime` it prompts a multi-select; non-interactively without `--runtime` it fails listing the options instead of hanging. Each selected runtime gets its own project hook (Claude Code `.claude/settings.json`, Codex `.codex/hooks.json`, Grok `.grok/hooks/pairroom.json`) and skill directory.

Run each bind as a tool call inside its intended native session: the official harnesses expose the current session ID to tool-call subprocesses (Claude Code sets `CLAUDE_CODE_SESSION_ID`; Codex sets `CODEX_SESSION_ID`; Grok sets `GROK_SESSION_ID`), and bind associates that session immediately. In a detached or plain terminal where the variable is absent, bind fails closed with guidance to run it inside the session; there is no fallback. Slots are Agent 1 / Agent 2: `--slot 1|2` is the primary form and the durable IDs `claude`/`codex` remain accepted; slot names never denote the selected Runtime. Omitted `--room` resolves the workspace's sole active native Room; omitted `--slot` resolves only when exactly one Room slot runs the caller's harness runtime; anything ambiguous fails with the candidate list instead of guessing. Bind stdout contains no long-lived secret. No installed Stop hook means bind is rejected. See [Native relay setup and usage](NATIVE_RELAY.md) for installation and approval steps. Native configuration selections are display-only, and PairRoom never starts or interrupts either process.

A completed binding already occupies its slot. Re-running bind inside the same session resumes idempotently without rotating the generation; a different session is rejected as occupied. Use `bind --replace` explicitly to revoke the existing generation and rebind from the current session; this cannot stop any native work.

All per-slot commands accept `--repo <project> --room <id> --slot <slot>`. Normally omit Room/slot flags inside the native session: `CLAUDE_CODE_SESSION_ID`, `CODEX_SESSION_ID` or `GROK_SESSION_ID` selects its exact associated binding before PID-based fallback, including multiple Desktop sessions sharing one process. Repeating `bind` in the same session resumes it and restores its saved Service endpoint without manual flags. Conflicting or unmatched session metadata fails instead of selecting another session. Association happens at bind from that environment; discovery metadata never grants collection. `status --brief` may inspect a unique incomplete binding of this runtime, including without `--room`/`--slot`; send/wait/exchange require a completed bind. Older harnesses without session metadata retain lineage-based defaults and explicit flags. Runtime ambiguity and multiple unassociated Rooms still require an explicit candidate. `install` infers the recognized harness; native provider/model/effort settings are not re-entered or harvested. For the first bind to a custom Service root, use `--service-file <root>/relay-endpoint.json` (a file path, never a token); later commands use the saved endpoint.

| Subcommand | Meaning |
|---|---|
| `bind` (zero-flag) | Inside a recognized native session: resolve the workspace's sole active native Room and the slot whose runtime matches the caller's harness; archived Rooms are never candidates and ambiguity fails with candidates |
| `bind --replace` | Explicitly revoke an occupied generation and rebind the current session; cannot stop old native work |
| `bind --create [--name <display-name>] [--runtime claude\|codex\|grok] [--peer-runtime claude\|codex\|grok]` | Without `--room`: register the workspace Project when missing, create a native Room through the same validated Management path the browser uses, bind this session, and print canonical `peer_join` plus same-machine/data-root/workspace `peer_join_local`. The creator runtime, actual slot, session identity and installed hook are validated before creating anything. If the current runtime matches no Service-default slot, the preflight says no Room was created and prints a one-time `--peer-runtime <claude\|codex\|grok>` retry template; existing-Room slot mismatches never offer that retry. A default pair is read from the Service and pinned for that creation; an unrecognized caller must run inside the native session and explicitly identify its runtime. Omitted runtimes copy the Service default pair after a read-only preflight, before any Project/Room creation; explicit runtimes stay empty-field selections that inherit native configuration |
| `send --id <client-id> --text <body>` | Explicit message to peer; requires the bind-time association like every collection call; repeat the same ID after an uncertain response, never deduplicate by body. A queued receipt prints a body-free diagnostic and peer `wait` command; it does not claim the peer is idle or has been woken |
| `send --to @user --attach <image>` | Human escalation with optional repeatable image paths; stdin supplies text when `--text` is absent |
| `exchange --id <client-id> --text <body>` | One explicit peer send, then the next FIFO input; defaults to a 3,600-second wait, supports `--timeout 0` for no PairRoom total deadline, and finite values up to 21,600 seconds; not a correlated request/reply transaction |
| `wait --timeout 0` | Foreground collection for an associated session; default 30 seconds, `0` means no PairRoom total deadline, finite values may be 1–21,600 seconds; renews only successful empty HTTP polls; stdout precedes ack |
| `status --brief` / `reconcile --brief` | Bounded authenticated transport summary, generated without copying message bodies, native session/transcript references or the full audit log; includes inbox counts and at most eight unknown-delivery recovery IDs. `status --brief` adds body-free collection hints only for inboxes queued at that snapshot |
| `status` / `peer` | Public delivery/binding state and original publication reconciliation / optional peer session references |
| `park --enabled=false` | Disable hook parking without removing the binding; foreground wait remains available |
| `nudge` | Print collection guidance only; no promise of native input injection |
| `reconcile` | Query pending publication: clear accepted, supplement definite absence with same sequence, otherwise retain unknown |
| `reconcile --resend` / `--discard` | Explicit decision on an unknown pending publication; resend retains original key, discard retains consumed sequence |
| `unbind --purge-hooks` | Revoke binding, remove slot files and remove only owned hooks when no other local slot uses them |
| `hook --runtime <kind>` | Official hook JSON on stdin; publishes Stop first, then bounded park; not a user-authored identity shortcut |

Park defaults to 30 seconds and at most eight consecutive message-bearing blocks. Outside this window, use foreground wait or a native human nudge. Unknown delivery must be inspected before the Room's explicit Retry. Same-turn send/exchange plus a peer-directed final reply deliberately creates two publications; omit that handle after explicit publication unless the second full reply is intended.

If creation succeeds but binding fails, the error preserves the Room ID and a recovery command with the Service/workspace paths; finish setup and use that command instead of repeating `--create`. The generated canonical `peer_join` preserves those paths, while `peer_join_local` deliberately does not. Generated commands use PowerShell quoting on Windows and POSIX shell quoting elsewhere. When creation itself cannot be confirmed, inspect Management before retrying to avoid duplicate Rooms.

### Foreground discussion loop

This optional Native path borrows the active wait idea from [Orca's messaging loop](https://github.com/stablyai/orca/blob/403b62a8d8fa6e896a93acc4c15405be0f0b7dc7/skill-guides/orchestration/references/messaging-and-gates.md), not its Run/Task/Dispatch hierarchy. It reuses PairRoom's explicit send, associated bindings, inbox and acknowledgement. Install the updated CLI and relay skill; `pairroom relay exchange --help` checks command availability. An older CLI is not made compatible by installing the new skill alone.

After BOTH sessions are bound, tell one to receive and the other to start. Run these through the intended native agents' tools, not a third unrelated terminal:

```bash
# Participant B: wait without a PairRoom total deadline when the native harness permits it.
pairroom relay wait --timeout 0

# Participant A: choose a fresh client ID. Exchange defaults to a one-hour total wait.
pairroom relay exchange --id review-opening-01 --text "Review this proposal against the repository: ..."

# Participant B: after receiving and reviewing, use another fresh ID.
pairroom relay exchange --id review-findings-01 --text "I found this counterexample: ..." --timeout 0
```

These are separate commands in separate sessions, not a script to run sequentially in one shell. Follow-up rounds reuse the binding and native session, but each new message needs a fresh client ID. Exchange IDs allow 1–128 letters, digits, `-` or `_` (not `.`/`..`). Exchange also accepts stdin text and repeatable `--attach` through the existing send path. It targets only the peer; use `send --to @user` for human escalation.

Exchange is **send once, then receive next**, not an atomic conversation transaction or a promise to match a reply. An already-queued message or user steering can arrive first; handle the actual envelope rather than skipping it. No accumulated history or outgoing body is appended to the returned envelope. A separate process-owned collector lock rejects a second `wait` or `exchange` before it claims or publishes; a Stop hook still publishes but does not take input from the foreground collector. The lock is separate from the short-lived state lock, so status/send/unbind remain available. Do not enable another coordinator for the same pair. For a final peer-facing result, use `relay send` and finish instead of calling exchange and leaving both sides waiting for ceremonial acknowledgements.

Long foreground waits keep the existing HTTP window at most 30 seconds and renew only an explicit successful `{"claim":null}`. The loop runs in the CLI, not in the model. Ordinary `wait` retains its 30-second default. Exchange defaults to 3,600 seconds. Both accept finite waits up to 21,600 seconds (6 hours), while `--timeout 0` removes PairRoom's total deadline and waits until delivery, caller/native cancellation, transport/auth failure, or process/service shutdown. A finite last poll can round up by less than a second and completes its bounded transport/output/ack work; these budgets are not hard task-completion or cost limits. No slot-state lock is held during the wait, and the hook's 30-second park/45-second timeout and block cap are unchanged.

| Outcome | Next action |
|---|---|
| Exchange returned an envelope | Process that exact input; send a new message only when needed, with a fresh ID |
| Finite publication wait expired with no input | Use the printed receive-only `relay wait` command; do not repeat send/exchange |
| Send outcome uncertain | Inspect `relay status --brief` first (full status only when needed); recover only the same publication with the original client ID and unchanged content, never a new ID |
| Collection, stdout or acknowledgement error | Inspect state/history and side effects first; the CLI stops and does not retry a possibly issued claim |
| Peer is idle, disconnected or outside a park | Start its foreground wait in that native session or use a human nudge; exchange cannot wake it |

Hook installation/approval and official session association remain required. `--timeout 0` does not guarantee a vendor tool can stay pending forever; native harness/tool cancellation remains authoritative. Model acceptance, uninterrupted long-running native tool calls and lower billed token usage require real vendor testing; synthetic transport tests do not establish them. No new Room mode, protocol version, schema, stage compiler, background model worker or process ownership is introduced. Existing send/wait and automatic Stop relay remain usable independently.


### Grok Build Native

Run setup and binding through the existing Grok session's own terminal tool.
`pairroom relay install` infers Grok where the native session/lineage is visible;
`--runtime grok` is the explicit setup fallback. It writes only PairRoom's
entries in the project's `.grok/hooks/pairroom.json` and installs the relay skill
under `$GROK_HOME/skills` (default `~/.grok/skills`). Existing hooks/configuration
are preserved. Review `/hooks`; project trust is a human decision via
`/hooks-trust`, not a permission PairRoom can grant.

```bash
# In Grok, after reviewing the installed hook:
pairroom relay bind --create --name "Joint review" --peer-runtime codex
# In the intended peer session, use the printed join command (or unambiguous bind):
pairroom relay bind
```

Choose `--peer-runtime claude|codex|grok` when creating a specific pair. The
calling Grok runtime is inferred; no vendor-named third slot is created. Without
pair overrides the Service's saved default pair still applies and must contain
the intended runtimes. Two Grok sessions use two distinct slots; the printed
join command disambiguates the peer. Bind reads `GROK_SESSION_ID` from the
harness environment and immediately enables relay: there is no nonce and no
need to finish a first Stop before send/wait/exchange. Later `bind` resumes the
same binding. Missing identity fails before creation; a lost response uses the
existing private bind-attempt journal, not a new ID or automatic replacement.

Grok clips Stop output after 32,768 Unicode scalars and hook feedback at 10,000.
Use explicit `relay send`/`exchange` for long replies, preferably stdin rather
than a very long shell argument, then omit a final peer handle. A clipped Stop
is never forwarded as a complete reply. Its recovery hint does not authorize
resending an already confirmed explicit publication.

A Grok Stop with queued input returns only a small instruction to run foreground
`relay wait`: the input remains queued, with no claim/ack, until that command
actually collects the full FIFO envelope. Active `exchange` rounds need no
intermediate Stop. Readiness is not delivery or model acceptance. Native tool
output truncation, cancellation and Grok's own continuation cap remain separate
boundaries; do not mistake a truncated tool result for a complete review.

This implements the [pinned Grok file-hook contract](https://github.com/xai-org/grok-build/blob/37949780c144e37df692e3d669051a21fec24f20/crates/codegen/xai-grok-pager/docs/user-guide/10-hooks.md),
not a transcript adapter or idle-session injector. SDK hook callback payloads
are not the installed file-hook channel. Authenticated Grok multi-round/tool
E2E and billed cost improvements are not established by fixture tests.
