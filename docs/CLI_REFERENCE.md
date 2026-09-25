# CLI reference

CLI Reference describes command responsibilities and how to discover flags. It does not copy the full `--help` output of every subcommand. Exact defaults, allowed values, and platform differences always come from the current binary.

New standalone Rooms support `pairroom serve --collaboration default` (the default) or `--collaboration custom --collaboration-instructions "..."`. An existing current-schema Room restores its saved instructions; conflicting explicit flags fail. `pairroom protocol` prints embedded v7 or native v8 mechanics; the removed `--role` flag no longer selects a collaboration mode.

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
| `pairroom protocol` | Print embedded v7 or `--host-mode native` v8 collaboration contracts |
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

The following names are extracted from `cmd/pairroom/*.go` and `internal/relayclient/cli.go`. Use them to find omissions; they do not mean every flag applies to every command.

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
- `--cursor`
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
- `--limit`
- `--listen`
- `--live`
- `--local-only`
- `--log-file`
- `--mock`
- `--n`
- `--name`
- `--no-browser`
- `--output`
- `--output-file`
- `--peer-runtime`
- `--pending`
- `--purge-hooks`
- `--recover-stale-lock`
- `--ref`
- `--replace`
- `--repo`
- `--resend`
- `--review`
- `--review-base`
- `--review-repo`
- `--room`
- `--runtime`
- `--runtime-limit`
- `--service-file`
- `--shutdown-timeout`
- `--since`
- `--slot`
- `--stall-warning-seconds`
- `--text`
- `--text-file`
- `--timeout`
- `--to`
- `--token`
<!-- /generated:flags -->

## Installation versus runtime availability

`pairroom doctor` is a Git / CLI protocol-metadata check, not an authentication or inference test. It checks the two configured slots, including Grok Build when selected; executable overrides are `--claude-command`, `--codex-command`, and `--grok-command` and follow the Runtime kind rather than the durable slot name. If it finds legacy workspace relay directories named `claude` or `codex`, it reports only their count, never opens credentials or state, and tells the user to re-bind canonical slots.

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
pairroom relay install --runtime claude,codex     # comma-separated; cc|claude, codex, grok (Grok reuses Claude Code hooks by default)
pairroom relay bind --create --name "<topic>"    # creator: project + native Room + bind; prints peer_join and peer_join_local
pairroom relay bind                              # peer: zero-flag inside a recognized session
```

`relay install` does not require a native session: run it from any terminal in the Project's worktree. With `--runtime` it installs those harnesses (comma-separated `cc|claude`, `codex`, `grok`); inside a recognized session it infers the harness; at an interactive terminal without `--runtime` it prompts a multi-select; non-interactively without `--runtime` it fails listing the options instead of hanging. Codex writes `.codex/hooks.json`. Claude Code writes `.claude/settings.json`. Grok Build reuses Claude Code project hooks by default, so a combined Claude Code+Grok install, or a Grok install when that Claude Code PairRoom Stop hook already exists, skips `.grok/hooks/pairroom.json` to avoid a second Stop command; a Grok-only install still writes the Grok file. If a leftover PairRoom Grok Stop hook is still present while the Claude Code hook covers Grok, install removes that extra Grok command. If `GROK_CLAUDE_HOOKS_ENABLED=false` is set for the install (the same switch as Grok's `[compat.claude] hooks = false`), Grok install writes the Grok file even when Claude Code hooks are present. Each selected runtime still gets its skill directory. Skill roots honor `CLAUDE_CONFIG_DIR`, `CODEX_HOME` and `GROK_HOME` respectively; hook files remain project-local.

Run each bind as a tool call inside its intended native session: the official harnesses expose the current session ID to tool-call subprocesses (Claude Code sets `CLAUDE_CODE_SESSION_ID`; Codex sets `CODEX_SESSION_ID`; Grok sets `GROK_SESSION_ID`), and bind associates that session immediately. In a detached or plain terminal where the variable is absent, bind fails closed with guidance to run it inside the session; there is no fallback. Grok shell mode (`!`) is a user-owned shell, not a tool-call subprocess: it does not receive `GROK_SESSION_ID`, and a completed `!` command does not run the Stop hook, so session-associated relay commands must be agent tool calls (`relay install` remains usable from any terminal). Durable slots are `slot1` / `slot2` (Agent 1 / Agent 2); `--slot 1|2` and `agent1|agent2` are canonical CLI forms, while `claude`/`codex` are input aliases normalized before persistence. Slot names never denote the selected Runtime. Omitted `--room` resolves the workspace's sole active native Room; omitted `--slot` resolves only when exactly one Room slot runs the caller's harness runtime; anything ambiguous fails with the candidate list instead of guessing. Bind stdout contains no long-lived secret. No installed Stop hook means bind is rejected. See [Native relay setup and usage](NATIVE_RELAY.md) for installation and approval steps. Native configuration selections are display-only, and PairRoom never starts or interrupts either process.

A completed binding already occupies its slot. Re-running bind inside the same session resumes idempotently without rotating the generation; a different session is rejected as occupied. Use `bind --replace` explicitly to revoke the existing generation and rebind from the current session; this cannot stop any native work.

All per-slot commands accept `--repo <project> --room <id> --slot <slot>`. Normally omit Room/slot flags inside the native session: `CLAUDE_CODE_SESSION_ID`, `CODEX_SESSION_ID` or `GROK_SESSION_ID` selects its exact associated binding before PID-based fallback, including multiple Desktop sessions sharing one process. Repeating `bind` in the same session resumes it and restores its saved Service endpoint without manual flags. Conflicting or unmatched session metadata fails instead of selecting another session. Association happens at bind from that environment; discovery metadata never grants collection. Only confirmed current-generation bindings participate in discovery; incomplete attempts and retired state never become active defaults. With an exact session match, either supplied Room/slot selector may filter it and the other is inferred; supplied `--repo` and `--service-file` must likewise agree with that binding instead of retargeting the session, and changing directories does not change its workspace. See [Native session workspace discovery](NATIVE_SESSION_WORKSPACE.md) for resolution order, locators and upgrade recovery. A plain terminal can inspect an existing binding but cannot create a session association. Runtime ambiguity and multiple unassociated Rooms still require an explicit candidate. `install` infers the recognized harness; native provider/model/effort settings are not re-entered or harvested. For the first bind to a custom Service root, use `--service-file <root>/relay-endpoint.json` (a file path, never a token); later commands use the saved endpoint.

| Subcommand | Meaning |
|---|---|
| `bind` (zero-flag) | Inside a recognized native session: resolve the workspace's sole active native Room and the slot whose runtime matches the caller's harness; archived Rooms are never candidates and ambiguity fails with candidates |
| `bind --replace` | Explicitly revoke an occupied generation and rebind the current session; cannot stop old native work |
| `bind --create [--name <display-name>] [--runtime claude\|codex\|grok] [--peer-runtime claude\|codex\|grok]` | Without `--room`: register the workspace Project when missing, create a native Room through the same validated Management path the browser uses, bind this session, and print canonical `peer_join` plus same-machine/data-root/workspace `peer_join_local`. The creator runtime, actual slot, session identity and installed hook are validated before creating anything. If the current runtime matches no Service-default slot, the preflight says no Room was created and prints a one-time `--peer-runtime <claude\|codex\|grok>` retry template; existing-Room slot mismatches never offer that retry. A default pair is read from the Service and pinned for that creation; an unrecognized caller must run inside the native session and explicitly identify its runtime. Omitted runtimes copy the Service default pair after a read-only preflight, before any Project/Room creation; explicit runtimes stay empty-field selections that inherit native configuration |
| `send --id <client-id> --text <body>` | Explicit message to peer; `--text-file PATH` reads UTF-8 from a file (`-` is stdin) and repeatable `--ref PATH` appends a local path/size/SHA-256 pointer without uploading contents; requires the bind-time association like every collection call; repeat the same ID after an uncertain response, never deduplicate by body. A queued peer receipt prints a short body-free diagnostic and the peer `wait` command on stderr, without an extra lookup or vendor session identity; `exchange` omits it because the sender collects next. In a wake-enabled Room the Service nudges an eligible Claude inbox or idle Codex-bound target automatically; `status --brief` shows the human-executable Codex fallback |
| `send --to @user --attach <image>` | Human escalation with optional repeatable image paths; stdin supplies text when `--text`, `--text-file` and `--ref` are absent |
| `exchange --id <client-id> --text <body>` | One explicit peer send, then the next FIFO input; accepts `--text-file`, `--ref` and `--output-file`; defaults to a 3,600-second wait, supports `--timeout 0` for no PairRoom total deadline, and finite values up to 21,600 seconds; not a correlated request/reply transaction |
| `wait --timeout 0` | Foreground collection for an associated session; default 3,600 seconds, `0` means no PairRoom total deadline, finite values may be 1–21,600 seconds; renews only successful empty HTTP polls; stdout precedes ack; `--output-file NEW_PATH` persists the envelope and prints a locator |
| `doctor` | Read-only current Native Room diagnostics plus local hook installation, CLI/protocol version match and last hook observation. Reports an already active Native Room; a suspended Room fails instead of being activated. Contacts no model; approval and model acceptance remain unknown. |
| `history --id ID` | Read exactly one published message as evidence, never collect or replay it. |
| `history [--pending] [--cursor CURSOR] [--limit 1–100] [--since RFC3339]` | Bounded pages: newest history or oldest unresolved work, independent of recent chat. Use returned `next_cursor`; `--id` cannot be combined with `--cursor`, `--pending`, `--since` or an explicit `--limit`. |
| `send/exchange --review [--review-repo PATH] [--review-base REF]` | Add an optional immutable Git review version to an ordinary explicit message. Trusted evidence checkout defaults to the bound Room workspace; base defaults to HEAD. Capture is bounded and needs an existing commit. No new file contents are sent. |
| `review --id ID [--review-repo PATH]` | Compare a published review version with the selected local checkout. A different checkout reports `different_workspace`; `unverified` and `different_workspace` are not a pass, and unchanged observation is not approval. |
| `status --brief` / `reconcile --brief` | Bounded authenticated transport summary, generated without copying message bodies, native session/transcript references or the full audit log; includes inbox counts and at most eight unknown-delivery recovery IDs. `status --brief` adds body-free collection hints only for inboxes queued at that snapshot, including manual Codex `wake_command` and `wake_notice` only when that target's authenticated vendor session is known |
| `status` / `peer` | Bounded body-free delivery summary by default (`--brief=false` for history) / optional peer session and Runtime metadata |
| `park --enabled=false` | Disable hook parking without removing the binding; foreground wait remains available |
| `nudge` | Print collection guidance only; no promise of native input injection |
| `reconcile` | Reconcile pending publication and return a bounded summary by default; clear accepted, supplement definite absence with same sequence, otherwise retain unknown |
| `reconcile --resend` / `--discard` | Explicit decision on an unknown pending publication; resend retains original key, discard retains consumed sequence |
| `unbind --purge-hooks` | Revoke binding, remove slot files and remove only owned hooks when no other local slot uses them |
| `unbind --local-only` | Offline exit: remove the local slot files without contacting the Service; the server-side binding and generation stay active (the slot remains occupied) until an explicit unbind or `bind --replace` |
| `hook --runtime <kind>` | Official hook JSON on stdin; publishes Stop first, then bounded park; not a user-authored identity shortcut |

Park defaults to 30 seconds and applies only while a peer reply is expected (see [Protocol](PROTOCOL.md#native-host-protocol-v8)); otherwise the hook returns at once and already queued input is still collected. Claude/Codex allow eight message-bearing blocks; Grok allows seven readiness/recovery hints, reserving its final publication gate. Outside this window, use foreground wait or a native human nudge. Unknown delivery must be inspected before the Room's explicit Retry. Same-turn send/exchange plus a peer-directed final reply deliberately creates two publications; omit that handle after explicit publication unless the second full reply is intended.

`send` now prints only `published`, `client_id`, `state` and `to`, not the outgoing body. A reused ID with different text fails rather than reporting the old publication as a new send. Its queued-delivery hint needs no extra request; `status --brief` wake advice uses a separate short peer-metadata deadline.

Claude external wake is captured automatically at confirmed bind/Stop; it adds no CLI flags or manual socket command. An unavailable capture adds a body-free `wake_notice` to bind output without invalidating the binding. See [Claude inbox setup](design/claude-inbox-wake.md).

`wake_command` and its adjacent `wake_notice` are a copyable Codex CLI template and boundary reminder for a human fallback. In a wake-enabled Room (default on, per-Room) the Service itself executes the equivalent vendor queue wake automatically under the durable reservation, rate-limit and audit contract in [PROTOCOL.md](PROTOCOL.md#automatic-idle-peer-wake); agents never run the printed template. The command contains the target's vendor session identity in the local process arguments and a fixed body-free nudge; do not place peer message content, secrets, or additional shell fragments into that command.

If creation succeeds but binding fails, the error preserves the Room ID and a recovery command with the Service/workspace paths; finish setup and use that command instead of repeating `--create`. The generated canonical `peer_join` preserves those paths, while `peer_join_local` omits the workspace path and only omits the Service path when it is the default. Never shorten away a custom `--service-file`. Generated commands use PowerShell quoting on Windows and POSIX shell quoting elsewhere. When creation itself cannot be confirmed, inspect Management before retrying to avoid duplicate Rooms.

### File-based messages and evidence

Keep short decisions and questions inline. For an existing long report, pass
`--text-file PATH` to `send` or `exchange`: the CLI reads the complete UTF-8 body
without putting it in argv or asking the model to copy it from tool output.
`--text-file -` explicitly reads stdin. Choose one body source, not both
`--text` and `--text-file`; `--text ""` is an explicit empty body. Without either
flag, stdin remains the default unless `--ref` is present. The complete body,
including reference metadata, must fit 256 KiB; invalid UTF-8 and NUL bytes fail
before publication rather than being silently rewritten by JSON encoding.

```bash
# Full report: no preliminary cat/read-and-retype step is needed.
pairroom relay send --id review-full-01 --text-file "/absolute/path/review.md"

# Large evidence: send the decision/question plus a local reference, not the evidence body.
pairroom relay exchange --id review-evidence-01 --text "Review section 3 against the current patch" --ref "/absolute/path/review.md"

# Receiver: use a NEW path in an existing private directory for a large expected reply.
pairroom relay wait --output-file "/absolute/path/incoming-review.txt"
```

`--ref PATH` is repeatable (at most 16, each at most 64 MiB). It appends a compact
JSON manifest of canonical absolute paths, byte lengths and SHA-256 hashes to the
ordinary message. Duplicate canonical paths are omitted. Files are hashed
without expanding their contents into the message; binary files are also allowed.
Use `--text-file - --ref PATH` to combine a piped body with references. Paths are
relative to the tool's current working directory, not `--repo`. The same flags
work with quoted Windows paths; no shell-specific substitution is necessary.

**References are not uploads, snapshots or archived attachments.** Both native
sessions need access to the same local file under their existing permissions.
Finish writing before sending, keep the file until publication is reconciled and
the peer has read it, and use a new revision/path when updating evidence. The
receiver verifies the hash and reads only the sections needed for the task;
a missing/changed file is an explicit failure, not permission to infer its content.
Keep decision-critical context in the short body. Send unchanged conclusions only
once and use subsequent messages for deltas, blockers or targeted questions.
For portable, self-contained text use `--text-file`; `--attach` remains the
validated image-upload path, not a general-file upload facility.

`wait/exchange --output-file NEW_PATH` saves the entire incoming envelope,
including sender/context, without a body preview. Stdout instead contains
`envelope_file`, `bytes`, `sha256` and a reminder to read the file before acting.
The CLI exclusively creates a new file, flushes and closes it, writes the complete
stdout receipt, then acknowledges the original claim. Existing files and symlinks
are never overwritten; an empty poll creates nothing. The parent directory must
already exist and be writable; that preflight runs before publication or claim.
Files are mode 0600 on POSIX; Windows inherits the destination directory's ACL,
so use a private directory.
The receipt confirms delivery to local storage/tool output, not model comprehension.
Read the envelope before acting and clean up only after it is no longer needed.
Stop-hook output is unchanged; file output is an explicit foreground choice.

A receipt-output failure retains the complete local file but withholds ack;
file-write failures also withhold ack. Inspect delivery state before any explicit
Retry and never automatically replay. After a confirmed exchange times out, its
printed receive-only command preserves `--output-file`; do not repeat exchange.
After success or a withheld acknowledgement, choose a fresh output path for the
next collection.

Disk/HTTP copies do not themselves consume model tokens. v5.2.1 already prints
body-free send/exchange publication receipts; these options avoid workflow
re-copying and optional receiver-side inlining, not an existing send-body echo.
Creating evidence and reading required content still cost tokens. File output is
not a better default for small replies, where another read would add a tool turn.
Tests bound observable output bytes, not vendor billing or model acceptance.

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

These are separate commands in separate sessions, not a script to run sequentially in one shell. Follow-up rounds reuse the binding and native session, but each new message needs a fresh client ID. Exchange IDs allow 1–128 letters, digits, `-` or `_` (not `.`/`..`). Exchange also accepts `--text-file`, repeatable `--ref` and `--attach` through the existing send path, plus `--output-file` for its incoming envelope. It targets only the peer; use `send --to @user` for human escalation.

Exchange is **send once, then receive next**, not an atomic conversation transaction or a promise to match a reply. An already-queued message or user steering can arrive first; handle the actual envelope rather than skipping it. No accumulated history or outgoing body is appended to the returned envelope. A separate process-owned collector lock rejects a second `wait` or `exchange` before it claims or publishes; a Stop hook still publishes but does not take input from the foreground collector. The lock is separate from the short-lived state lock, so status/send/unbind remain available. Do not enable another coordinator for the same pair. For a final peer-facing result, use `relay send` and finish instead of calling exchange and leaving both sides waiting for ceremonial acknowledgements.

Long foreground waits keep the existing HTTP window at most 30 seconds and renew only an explicit successful `{"claim":null}`. The loop runs in the CLI, not in the model. Both `wait` and `exchange` default to 3,600 seconds. Use a single long or unbounded background wait only when the harness wakes on tracked completion without model polling; otherwise prefer a foreground wait within its tool deadline, then end the turn instead of repeatedly polling. Empty expiries can themselves cost model turns. Both accept finite waits up to 21,600 seconds (6 hours), while `--timeout 0` removes PairRoom's total deadline and waits until delivery, caller/native cancellation, transport/auth failure, or process/service shutdown. A finite last poll can round up by less than a second and completes its bounded transport/output/ack work; these budgets are not hard task-completion or cost limits. No slot-state lock is held during the wait, and the hook's 30-second park/45-second timeout remain separate.

| Outcome | Next action |
|---|---|
| Exchange returned an envelope | Process that exact input; send a new message only when needed, with a fresh ID |
| Finite publication wait expired with no input | Use the printed receive-only `relay wait` command; do not repeat send/exchange |
| Send outcome uncertain | Inspect `relay status --brief` first (full status only when needed); recover only the same publication with the original client ID and unchanged content, never a new ID |
| Collection, stdout or acknowledgement error | Inspect state/history and side effects first; the CLI stops and does not retry a possibly issued claim |
| Peer is idle, disconnected or outside a park | Start its foreground wait in that native session, rely on a wake-enabled Room's automatic Service wake for an eligible Claude/Codex-bound target, or use a human nudge; exchange itself cannot wake it |

Hook installation/approval and official session association remain required. `--timeout 0` does not guarantee a vendor tool can stay pending forever; native harness/tool cancellation remains authoritative. Model acceptance, uninterrupted long-running native tool calls and lower billed token usage require real vendor testing; synthetic transport tests do not establish them. No new Room mode, protocol version, schema, stage compiler, background model worker or process ownership is introduced. Existing send/wait and automatic Stop relay remain usable independently.

### Grok Build Native

Run setup and binding through the existing Grok session's own terminal tool.
`pairroom relay install` infers Grok where the native session/lineage is visible;
`--runtime grok` is the explicit setup fallback. Grok Build's Claude Code
compatibility layer runs `.claude/settings.json` hooks by default, so PairRoom
writes `.grok/hooks/pairroom.json` only when no Claude Code PairRoom Stop hook is
present (or when that compatibility is disabled). Combined Claude Code+Grok
setup uses the Claude Code hook alone, and install strips a leftover PairRoom
Grok Stop hook in that case. The relay skill still installs under
`$GROK_HOME/skills` (default `~/.grok/skills`). Existing hooks/configuration
are preserved. Review `/hooks` and press `r` to reload after installation; project trust is a human decision via
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
Grok shell mode (`!`) does not inject that variable — paste a join command as a
normal prompt so the agent runs it, rather than executing `! pairroom relay bind`.

Grok clips Stop output after 32,768 Unicode scalars and hook feedback at 10,000.
Use explicit `relay send`/`exchange --text-file PATH` (or stdin) for long replies
rather than a very long shell argument, then omit a final peer handle. A clipped Stop
is never forwarded as a complete reply. Its recovery hint does not authorize
resending an already confirmed explicit publication.

PairRoom requests at most seven Grok continuations: the vendor skips the final
Stop hook after eight, so reserving one gate avoids losing the last reply from
PairRoom's own continuation chain. Other hooks share that vendor budget. For
long discussions, use foreground exchange rather than additional Stop rounds.

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
