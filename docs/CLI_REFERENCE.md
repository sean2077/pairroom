# CLI reference

CLI Reference describes command responsibilities and how to discover flags. It does not copy the full `--help` output of every subcommand. Exact defaults, allowed values, and platform differences always come from the current binary.

The standalone `serve` command is Embedded-only; Native is the recommended default through `pairroom service` and `pairroom relay bind --create`. New standalone Rooms support `pairroom serve --collaboration default` (the default) or `--collaboration custom --collaboration-instructions "..."`. An existing current-schema Room restores its saved instructions; conflicting explicit flags fail. `pairroom protocol` prints native v8 mechanics by default, or embedded v7 with `--host-mode embedded`; the removed `--role` flag no longer selects a collaboration mode.

## Top-level commands

| Command | Responsibility |
|---|---|
| `pairroom daemon` | Install and manage pairroom service in the OS service manager |
| `pairroom service` | Start the multi-Project / multi-Room Management Shell |
| `pairroom serve` | Start a current-format standalone Embedded Room |
| `pairroom doctor` | Validate Git and vendor CLI installation |
| `pairroom providers` | Read and validate the sanitized CC Switch Profile catalog without changing current state |
| `pairroom verify` | Strictly validate room data integrity |
| `pairroom backup` | Create a verified room-data backup |
| `pairroom restore` | Restore and verify a room-data backup |
| `pairroom diagnostics` | Generate a redacted diagnostics bundle |
| `pairroom relay` | Install approved project hooks, bind user-owned sessions, and publish/collect native relay messages |
| `pairroom protocol` | Print Native v8 (default) or `--host-mode embedded` v7 collaboration contracts |
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
- `--file`
- `--follow`
- `--force`
- `--gemini-command`
- `--grok-command`
- `--host-mode`
- `--id`
- `--idle-timeout`
- `--inline-max`
- `--input`
- `--join`
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
- `--resume-pending`
- `--review`
- `--review-base`
- `--review-repo`
- `--room`
- `--runtime`
- `--runtime-limit`
- `--service-file`
- `--share`
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

`pairroom daemon install` parses `--binary`, `--work-dir`, `--log-max-size`, `--log-max-backups`, and `--` itself, so they are not in this inventory; see `pairroom daemon --help`.

## Installation versus runtime availability

`pairroom doctor` is a Git / CLI protocol-metadata check, not an authentication or inference test. It checks the two configured slots, including Grok Build and Gemini CLI when selected; executable overrides are `--claude-command`, `--codex-command`, `--grok-command`, and `--gemini-command` and follow the Runtime kind rather than the durable slot name. If it finds legacy workspace relay directories named `claude` or `codex`, it reports only their count, never opens credentials or state, and tells the user to re-bind canonical slots.

Each JSON `probe` has `verification: "cli_metadata_only"`. Its protocol and capability list describe the adapter contract and help/version-derived options, not verified native methods, authentication, or successful exact session recovery. Similar option names do not qualify: `--acp-debug` does not satisfy Gemini’s ACP requirement, and `--resume-session` does not satisfy Claude’s `--resume` requirement. Missing Claude resume support blocks restoration while preserving the existing session; it never authorizes a fresh replacement. Separate `--live` results below provide startup/response evidence only.

```bash
pairroom doctor --config /absolute/path/pairroom.json --repo /absolute/path/project --json
# Explicit consent: this can consume Provider quota and create native sessions.
pairroom doctor --config /absolute/path/pairroom.json --live --json
```

`--live` uses each config-file Agent's Runtime, Provider, model, and effort. It starts a fresh native session in a disposable Git workspace, narrows native permissions, requests only a nonce response, and requires both the expected text and its matching input-completed event. It stops on a tool/approval request and attempts native shutdown and temporary-directory cleanup on every exit. Native global configuration, hooks, and MCP still apply: this is not an isolation sandbox or a tool-compatibility certification. Each Agent check has a 75-second budget plus bounded shutdown; Ctrl+C cancels it. The JSON adds separate `checks` for startup/response, and failures make the command exit nonzero. Existing `doctor` JSON still includes local paths; review it before sharing.

The Management **Settings → Diagnostics** section offers the same live check with explicit confirmation and cancellation, plus Service storage, Registry, Project, capacity, and four-CLI environment checks. Its default pair follows the saved Service default Agent pair profile; selecting a Room uses that Room's immutable selections. CLI `doctor` instead uses the supplied configuration file, not Service-scoped profiles. Neither entry point resumes an existing Room session. Mock results are explicitly unverified.

**Download safe report** exports only check codes/statuses, platform, numeric versions, timestamps, and timings. It excludes local paths, Room names/IDs, Provider details, native session IDs, command arguments, credentials, and raw process/model output. This report does not replace `pairroom diagnostics`, the existing redacted Room archive bundle.

## Native relay commands

Run create, bind and foreground commands through the intended native harness's command tool (Claude Code, Codex CLI/Desktop, Grok Build or Gemini CLI), not a separate terminal. The current Git workspace and native session metadata supply defaults. PairRoom does not start a replacement Agent process. For ownership, runtime limits, audit fixes and the evidence behind efficiency claims, see [Native relay](NATIVE_RELAY.md).

For sessions using the same local Service, create a Room with host mode **Native** in Management, or let the first session create it: `pairroom relay bind --create` registers the Project when missing, creates the Native Room through the same validated Management path, binds that session, and prints `peer_join` plus `peer_join_local` for a peer using the same machine, Service data root and workspace. For a remote colleague, the host instead uses `bind --create --share lan`, and the guest uses the returned `relay join` invitation without a local Service. [Native setup](NATIVE_RELAY.md#one-time-project-setup) owns hook installation/approval and [LAN collaboration](LAN_NATIVE.md#create-request-accept) owns exact-receipt admission.

```bash
pairroom relay install                           # external OK; prompts a multi-select at a terminal, otherwise pass --runtime
pairroom relay install --runtime claude,codex     # choose any of claude (cc), codex, grok, gemini
pairroom relay bind --create --name "<topic>"    # creator: project + native Room + bind; prints peer_join and peer_join_local
pairroom relay bind                              # peer: zero-flag inside a recognized session
```

`relay install` runs from any terminal in the intended worktree. `--runtime` accepts a comma-separated selection of `claude` (`cc`), `codex`, `grok`, and `gemini`. Without it, a recognized native session supplies its Runtime; an interactive terminal prompts a multi-select; non-interactive use without a recognized session fails with guidance. Installation preserves unrelated settings and writes each selected Runtime's relay skill. It does not approve hooks or grant workspace trust. The optional `npx skills add sean2077/pairroom` route installs the skill only. With the skill loaded, `/pairroom-relay <topic>` runs the creation flow.

Run each bind as a tool call inside its intended native session. Claude Code supplies `CLAUDE_CODE_SESSION_ID`, Codex supplies `CODEX_SESSION_ID`, and Grok supplies `GROK_SESSION_ID`. Gemini uses official metadata retained by its approved BeforeTool hook; `GEMINI_SESSION_ID` is not its identity source. Missing or conflicting identity fails before association. Grok `!` shell mode and detached terminals cannot establish a binding. See [Gemini Native](#gemini-cli-native) and [Grok Native](#grok-build-native) for their tool-call boundaries.

Durable slots are `slot1` / `slot2` (Agent 1 / Agent 2); `--slot 1|2` and `agent1|agent2` are accepted CLI forms, while legacy `claude`/`codex` inputs are normalized before persistence. Slots never denote the selected Runtime. Bind associates immediately and returns no long-lived secret. Required hooks must be installed; native approval stays outside PairRoom. An unapproved response hook can leave a binding unable to publish, and Gemini's BeforeTool approval is additionally needed to capture bind-time identity. Native Room configuration remains display-only.

A completed binding already occupies its slot. Re-running bind inside the same session resumes idempotently without rotating the generation, and reports `inbox_queued` with a `relay wait` command when input is already waiting for it (one bounded read; omitted when none or unavailable); a different session is rejected as occupied. For a local Room, use `bind --replace` explicitly to revoke the existing generation and rebind from the current session; this cannot stop any native work. Direct LAN session changes use `join '<fresh-invitation>' --replace` after the host confirms the old membership ended, retaining old receipts without replay and requiring fresh exact-receipt approval. When the replaced generation had inbox work, the bind result adds `replaced`: the old `generation` and four lists, each with up to eight `ids` and an exact `count` — `cancelled` (queued input this bind cancelled), `unknown` (claimed, unacknowledged input this bind made unknown), `already_unknown`, and the newest `handed_off`. Handed-off input was written to the previous collector's stdout and may have run; it was not invalidated, and its count covers the newest 5,000 retained messages, exact only when `scan_complete` is true. Inspect any of them with `relay history --id`; nothing is requeued, and the report carries no message text or receipt.

Per-slot commands accept `--repo <project> --room <id> --slot <slot>`. Normally omit them: exact native Runtime/session identity selects the confirmed current-generation binding, including sessions sharing one Desktop process. Repeating `bind` resumes the saved target. A supplied selector must agree with that identity; changing cwd does not retarget it. `--service-file <root>/relay-endpoint.json` selects a custom **local hosting Service** for first bind, after which the saved endpoint is reused. Direct LAN bindings retain their own host and reject that flag. Incomplete attempts and retired state are not active defaults. A plain terminal can inspect an explicitly selected binding but cannot establish an association. [Session workspace discovery](NATIVE_SESSION_WORKSPACE.md) owns the resolution order and recovery rules.

| Subcommand | Meaning |
|---|---|
| `bind` (zero-flag) | Resume this native session's existing local or direct LAN association first. For a new local binding, resolve the workspace's sole active Native Room and matching Runtime slot; archived Rooms are not candidates and ambiguity returns candidates |
| `bind --replace` | Local Rooms only; direct LAN session replacement uses an explicit fresh `join --replace` after retirement and requires new owner approval. Explicitly revoke an occupied generation and rebind the current session; cannot stop old native work. Refused while the slot has unpublished Stop replies: publish them with `reconcile` or drop each with `reconcile --discard` first |
| `bind --create [--name NAME] [--runtime KIND] [--peer-runtime KIND]` | Local creation, without `--room`; `KIND` is `claude` (`cc`), `codex`, `grok`, or `gemini`. Validate caller, selected slot and installed hooks before registering a missing Project or creating the Room. The creator occupies Agent 1 unless `--slot 2` is explicit. Omitted runtime overrides copy the Service's saved pair, pinned for that creation. If that pair does not contain the caller, no Room is created and a one-time `--peer-runtime KIND` recovery command is printed. Explicit runtime choices inherit the original harness's configuration; no Provider/model/effort state is harvested. Returns `peer_join` and `peer_join_local` |
| `bind --create --share lan [--name NAME]` | Create a Native Room explicitly for a LAN peer after the local host has enabled LAN sharing. The creator's actual Runtime is the only selection; the peer stays `awaiting_peer` until exact-key admission. Omit `--peer-runtime`. Returns a public `invite` and `peer_join`, never private credentials. |
| `join '<public-invitation>' [--repo LOCAL_PATH]` | Request LAN membership directly from this native session using CLI/hooks; no local Service, daemon or `--service-file`. The invitation supplies the pinned host target. Returns a public pending receipt; repeat the same command after host acceptance or an uncertain response. A private per-user client record retains the Room key and original request. `bind` resumes an accepted join without changing its host. |
| `join '<fresh-invitation>' --replace [--repo ORIGINAL_PATH]` | Explicitly replace a retired association for the same host/Room and original workspace after the host confirms the previous membership is revoked, left or expired. Starts a fresh request and requires new exact-receipt approval; preserves old client receipts and workspace publication/attempt state under the previous bind ID without replay. Does not replace a still-active or uncertain membership. Plain join still renews an expired never-admitted request in its original session/key. |
| `invite` / `accept '<exact-join-receipt>'` / `revoke` | Bound local owner operations for a shared Native Room: issue a short-lived request-only invitation, admit the exact request/key receipt supplied through the trusted colleague channel, or revoke the member. An ordinary LAN member cannot call owner operations. |
| `send/exchange --file PATH` | Explicitly upload repeatable UTF-8 text evidence files, up to 5 MiB each, as verified downloadable objects. Received scripts remain inert private files. `--ref` remains a path/hash pointer and does not upload bytes. |
| `send --id <client-id> --text <body>` | Explicit message to peer; `--text-file PATH` reads UTF-8 from a file (`-` is stdin) and repeatable `--ref PATH` appends a local path/size/SHA-256 pointer without uploading contents; requires the bind-time association like every collection call; repeat the same ID after an uncertain response, never deduplicate by body. If that ID already names different content, send fails with "already used for a different message"; that answer is definite, not uncertain, and nothing new was published. A queued peer receipt prints a short body-free diagnostic and the peer `wait` command on stderr, without an extra lookup or vendor session identity; `exchange` omits it because the sender collects next. In a wake-enabled Room the Service nudges an eligible Claude inbox or idle Codex-bound target automatically; `status --brief` shows the human-executable Codex fallback |
| `send --to @user --attach <image>` | Human escalation with optional repeatable image paths; stdin supplies text when `--text`, `--text-file` and `--ref` are absent. A same-ID retry re-uploads the images and still matches the original when each image is byte-identical with the same file name and order |
| `exchange --id <client-id> --text <body>` | One explicit peer send, then the next FIFO input; accepts `--text-file`, `--ref` and `--output-file`; defaults to a 3,600-second wait, supports `--timeout 0` for no PairRoom total deadline, and finite values up to 21,600 seconds; not a correlated request/reply transaction |
| `wait --timeout 0` | Foreground collection for an associated session; default 3,600 seconds, `0` means no PairRoom total deadline, finite values may be 1–21,600 seconds; renews only successful empty HTTP polls; stdout precedes ack; `--output-file NEW_PATH` persists the envelope and prints a locator, and `--inline-max N` prints envelopes up to N bytes directly instead |
| `preflight [--runtime KIND] [--repo PATH] [--service-file PATH]` | Check CLI PATH, workspace, caller and required hooks; `KIND` accepts all four supported Runtimes. For local hosting, also check Service reachability, release/build and matching active Native Rooms. An existing direct binding checks its saved host contact/admission instead, without comparing versions. Prints JSON with ordered `next_steps`; exits nonzero unless `ready`. Creates no binding, Room or hook configuration and contacts no model; a bound LAN check may activate the host Room. |
| `preflight --join [--runtime KIND] [--repo PATH]` | Check eligibility before joining: no local Service or `--service-file`. A session already bound locally or remotely is ineligible for another association. Hook approval stays unknown; run the actual join from the intended native session. |
| `doctor` | Inspect an active Native Room plus local hook installation, product skill freshness (`local.skill`), release/protocol matches and last hook observation. `local.hook_hint` persists until the response hook runs. Leaves a suspended Room inactive; explicitly run `status --brief` to activate it before repeating doctor. Direct LAN diagnostics describe direct collection and host contact, not an optional local observer's wake capability. No publication, claim or model call; approval and model acceptance stay unknown. |
| `history --id ID` | Read exactly one published message as evidence, never collect or replay it. |
| `history [--pending] [--cursor CURSOR] [--limit 1–100] [--since RFC3339]` | Bounded pages: newest history or oldest unresolved work, independent of recent chat. Use returned `next_cursor`; `--id` cannot be combined with `--cursor`, `--pending`, `--since` or an explicit `--limit`. |
| `send/exchange --review [--review-repo PATH] [--review-base REF]` | Add an optional immutable Git review version to an ordinary explicit message. Trusted evidence checkout defaults to the bound Room workspace; base defaults to HEAD. Capture is bounded and needs an existing commit. No new file contents are sent. |
| `review --id ID [--review-repo PATH]` | Compare a published review version with the selected local checkout. A different checkout reports `different_workspace`; `unverified` and `different_workspace` are not a pass, and unchanged observation is not approval. |
| `status --brief` / `reconcile --brief` | Bounded authenticated transport summary, generated without copying message bodies, native session/transcript references or the full audit log; includes inbox counts and at most eight unknown-delivery recovery IDs. `status --brief` adds body-free collection hints only for inboxes queued at that snapshot, including manual Codex `wake_command` and `wake_notice` only when that target's authenticated vendor session is known |
| `status` | Bounded body-free delivery summary by default; `--brief=false` returns a full local snapshot or bounded direct LAN history window. The brief `presence` lines summarize transport observations, not model activity. First reconciles pending/held Stop publications under their original identities. |
| `peer` | Read the peer's binding and Runtime metadata. Local transport may include its native session identity; direct LAN transport removes native session/transcript metadata. Does not publish or collect. |
| `park --enabled=false` | Disable hook parking without removing the binding; foreground wait remains available |
| `nudge` | Print collection guidance only; no promise of native input injection |
| `reconcile` | Reconcile pending publication, then any Stop replies held behind it in sequence order, and return a bounded summary by default; clear accepted, supplement definite absence with same sequence, otherwise retain unknown and send nothing behind it |
| `reconcile --resend` / `--discard` | Explicit decision on the unknown oldest pending publication only; resend retains original key, discard retains consumed sequence; held replies then publish in order |
| `unbind [--purge-hooks]` | End the local binding or direct LAN membership, then remove its active workspace files; LAN history and recovery archives are retained. `--purge-hooks` also removes only PairRoom-owned hooks when no other binding in this workspace uses that Runtime. Native processes keep running |
| `unbind --local-only [--room LOCAL_ROOM_ID]` | Offline exit: detach an exact accepted binding or pending LAN attempt without contacting its host; use the ID printed by join if needed. Local routing/observation stops and its native-session reservation is released. A pending or accepted remote admission may remain until the host revokes it; no automatic reconnect or new join follows. Local Rooms retain their existing explicit replacement path. This cannot recall delivered content or stop native work. |
| `hook --runtime <kind>` | Official hook JSON on stdin; publishes Stop first, then bounded park; not a user-authored identity shortcut |

LAN setup, human author provenance, verified local evidence paths, and recovery are described in [Native collaboration on a LAN](LAN_NATIVE.md). Only the Room's host needs a Service. Guest CLI/hooks connect directly through the immutable target retained by each binding, without a Service endpoint file. Distinct native sessions may host local Rooms and join several remote hosts concurrently; there is no global server switch. A running local Service is an optional dashboard/observer over the same per-user client store. Without a live collector, hook or observer, input remains queued at the host.

Preflight hook entries report the product skill as `current`, `stale`, `missing`
or `external` (content PairRoom did not write). Stale/missing skills and local
Service release/build mismatches add advisory `next_steps` without necessarily
making `ready` false. Read those steps even on exit 0. `history`, `peer` and
`review` do not publish or collect messages, but their authenticated reads can
activate a suspended Room and resume configured wake processing. Use `doctor`
when activation must also be avoided.

For direct LAN `status --brief=false`, the window contains at most 300 complete messages and 80 audit entries; encoded JSON limits may return fewer messages to keep the response below the 8 MiB LAN protocol cap. The exact `total_messages` and `total_audit` counts are preserved, and message bodies, quotes and evidence metadata are not clipped. Read older evidence through paginated `history` or `history --id ID`. Local `--brief=false` continues to return the full snapshot.

For local Service transport, every relay subcommand except `preflight` prints one stderr line suggesting `pairroom relay preflight` when a Service response named a release other than the CLI's (build metadata such as `+8.a5cb253` is ignored). It reads the release from responses the command already receives, so it never changes stdout or adds a Stop-hook request. When a failure looks like version skew (a rejected route or request shape, an authentication rejection, or no matching binding), the line says the mismatch may explain it; if such a failure happened before any Service contact, a foreground command, never the hook, makes one short read-only Service request to learn the release. An unknown or matching release prints nothing. Direct LAN transport does not supply this header or run that local fallback probe; compare the host release/protocol in `relay doctor`. Direct preflight checks contact/admission without a version comparison.

Park lasts at most 30 seconds (fixed; `--timeout` does not change it, and it shortens to fit the 45-second hook timeout) and applies only while a peer reply is expected (see [Protocol](PROTOCOL.md#native-host-protocol-v8)); otherwise the hook returns at once and already queued input is still collected. Claude/Codex/Gemini allow eight message-bearing blocks; Grok allows seven readiness/recovery hints, reserving its final publication gate. Outside this window, use foreground wait or a native human nudge. Unknown delivery must be inspected before the Room's explicit Retry. Same-turn send/exchange plus a peer-directed final reply deliberately creates two publications; omit that handle after explicit publication unless the second full reply is intended.

`send` now prints only `published`, `client_id`, `state` and `to`, not the outgoing body. A reused ID with different text fails rather than reporting the old publication as a new send. Its queued-delivery hint needs no extra request; `status --brief` wake advice uses a separate short peer-metadata deadline.

Claude external wake is captured automatically at confirmed bind/Stop; it adds no CLI flags or manual socket command. An unavailable capture adds a body-free `wake_notice` to bind output without invalidating the binding. See [Claude inbox setup](design/claude-inbox-wake.md).

`wake_command` and its adjacent `wake_notice` are a copyable Codex CLI template and boundary reminder for a human fallback. In a wake-enabled Room (default on, per-Room) the Service itself executes the equivalent vendor queue wake automatically under the durable reservation, rate-limit and audit contract in [PROTOCOL.md](PROTOCOL.md#automatic-idle-peer-wake); agents never run the printed template. The command contains the target's vendor session identity in the local process arguments and a fixed body-free nudge; do not place peer message content, secrets, or additional shell fragments into that command.

If creation succeeds but binding fails, the error preserves the Room ID and a recovery command with the Service/workspace paths; finish setup and use that command instead of repeating `--create`. The generated canonical `peer_join` preserves those paths, while `peer_join_local` omits the workspace path and only omits the Service path when it is the default. Never shorten away a custom `--service-file`. Generated commands use PowerShell quoting on Windows and POSIX shell quoting elsewhere. When creation itself cannot be confirmed, inspect Management before retrying to avoid duplicate Rooms.

### File-based messages and evidence

Choose the form according to what the receiver needs:

| Option on `send` / `exchange` | What crosses the relay | Receiver access |
|---|---|---|
| `--text-file PATH` | The file becomes the message body, within the 256 KiB total body limit | Read the message; there is no separate attachment |
| `--ref PATH` | A canonical path, size and SHA-256 reference; no content upload | Needs independent access to the sender's retained file |
| `--file PATH` | Verified UTF-8 text evidence, 1 byte–5 MiB per file | Downloadable attachment; suitable for scripts, patches, configs and logs |
| `--attach PATH` | A validated image, up to 5 MiB per file | Image attachment with a preview/download |

Uploads work in both local and LAN Native Rooms. The combined attachment set
is limited to eight items and 20 MiB per message. Text evidence rejects NUL
bytes; uploaded scripts remain inert data. LAN transfers additionally use the
[shared storage and cache budgets](LAN_NATIVE.md#share-a-useful-bug-report).

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
Use `--text-file` when the full report should be read as the message, or
`--file` when it should remain separately downloadable evidence. A reference to
a path on another machine does not make that file accessible locally.

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
`--inline-max N` (with `--output-file`, 0–262,144, default 0) prints an envelope of
at most N bytes directly on stdout instead, exactly like a wait without
`--output-file`, and creates no file; a larger one is still saved with its
receipt. The same stdout-before-ack rule applies to both, and the printed
receive-only recovery command keeps the flag. A saved receipt for an envelope of at most 8 KiB suggests the flag; the skill does not repeat it.

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

This optional Native path borrows the active wait idea from [Orca's messaging loop](https://github.com/stablyai/orca/blob/403b62a8d8fa6e896a93acc4c15405be0f0b7dc7/skill-guides/orchestration/references/messaging-and-gates.md), not its Run/Task/Dispatch hierarchy. It reuses PairRoom's explicit send, associated bindings, inbox and acknowledgement. Install the updated CLI and relay skill; `pairroom relay --help` lists the relay commands the installed CLI supports, and an unknown command fails even with `--help`. An older CLI is not made compatible by installing the new skill alone.

After BOTH sessions are bound, tell one to receive and the other to start. Run these through the intended native agents' tools, not a third unrelated terminal:

```bash
# Participant B: wait without a PairRoom total deadline when the native harness permits it.
pairroom relay wait --timeout 0

# Participant A: choose a fresh client ID. Exchange defaults to a one-hour total wait.
pairroom relay exchange --id review-opening-01 --text "Review this proposal against the repository: ..."

# Participant B: after receiving and reviewing, use another fresh ID.
pairroom relay exchange --id review-findings-01 --text "I found this counterexample: ..." --timeout 0
```

These are separate commands in separate sessions, not a script to run sequentially in one shell. Follow-up rounds reuse the binding and native session, but each new message needs a fresh client ID. Exchange IDs allow 1–128 ASCII letters, digits, `-` or `_` (not `.`/`..`). Exchange also accepts `--text-file`, repeatable `--ref`, `--attach` and `--file` through the existing send path, plus `--output-file` for its incoming envelope. It targets only the peer; use `send --to @user` for human escalation.

Exchange is **send once, then receive next**, not an atomic conversation transaction or a promise to match a reply. An already-queued message or user steering can arrive first; handle the actual envelope rather than skipping it. No accumulated history or outgoing body is appended to the returned envelope. A separate process-owned collector lock rejects a second `wait` or `exchange` before it claims or publishes; a Stop hook still publishes but does not take input from the foreground collector. The lock is separate from the short-lived state lock, so status/send/unbind remain available. Do not enable another coordinator for the same pair. For a final peer-facing result, use `relay send` and finish instead of calling exchange and leaving both sides waiting for ceremonial acknowledgements.

Long foreground waits keep the existing HTTP window at most 30 seconds and renew only an explicit successful `{"claim":null}`. The loop runs in the CLI, not in the model. Both `wait` and `exchange` default to 3,600 seconds. Use a single long or unbounded background wait only when the harness wakes on tracked completion without model polling; otherwise prefer a foreground wait within its tool deadline, then end the turn instead of repeatedly polling. Empty expiries can themselves cost model turns. Both accept finite waits up to 21,600 seconds (6 hours), while `--timeout 0` removes PairRoom's total deadline and waits until delivery, caller/native cancellation, transport/auth failure, or process/service shutdown. A wait longer than one poll also stops, before claiming, once the native harness process that started it has exited, so an orphaned background collector cannot keep taking input, holding the collector lock or suppressing wake; a wait started outside a recognized harness has no such check. A finite last poll can round up by less than a second and completes its bounded transport/output/ack work; these budgets are not hard task-completion or cost limits. No slot-state lock is held during the wait, and the hook's 30-second park/45-second timeout remain separate.

| Outcome | Next action |
|---|---|
| Exchange returned an envelope | Process that exact input; send a new message only when needed, with a fresh ID |
| Finite publication wait expired with no input | Use the printed receive-only `relay wait` command; do not repeat send/exchange |
| Send outcome uncertain | Inspect `relay status --brief` first (full status only when needed); recover only the same publication with the original client ID and unchanged content, never a new ID |
| Collection, stdout or acknowledgement error | Inspect state/history and side effects first; the CLI stops and does not retry a possibly issued claim |
| Peer is idle, disconnected or outside a park | Start its foreground wait in that native session, rely on a wake-enabled Room's automatic Service wake for an eligible Claude/Codex-bound target, or use a human nudge; exchange itself cannot wake it |

Hook installation/approval and official session association remain required. `--timeout 0` does not guarantee a vendor tool can stay pending forever; native harness/tool cancellation remains authoritative. Model acceptance, uninterrupted long-running native tool calls and lower billed token usage require real vendor testing; synthetic transport tests do not establish them. No new Room mode, protocol version, schema, stage compiler, background model worker or process ownership is introduced. Existing send/wait and automatic Stop relay remain usable independently.

### Gemini CLI Native

Use `pairroom relay install --runtime gemini` to merge the BeforeTool/AfterAgent definitions and install its native skill. Run bind through Gemini run_shell_command after approving/reloading both hooks. Session identity comes from official BeforeTool metadata; `GEMINI_SESSION_ID` is not a shell-tool identity source. See [Gemini setup and limitations](NATIVE_RELAY.md#gemini-cli).

## Grok Build Native

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

Choose `--peer-runtime claude|codex|grok|gemini` when creating a specific pair. The
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
Once the cap is reached the Grok session goes idle, and no Service wake exists
for Grok, so an unattended run depends on that session holding one background
`relay wait`; see [long unattended runs](NATIVE_RELAY.md#long-unattended-runs-by-runtime).

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
