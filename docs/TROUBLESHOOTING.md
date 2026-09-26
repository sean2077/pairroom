# Troubleshooting

Identify the Service, Room **host mode**, native Runtime, and browser layer before acting. Preserve the first error and relevant IDs. Embedded owns adapters and Turns; Native does not start or interrupt the original sessions. Use [Operations](OPERATIONS.md) for lifecycle and [Support](../SUPPORT.md) for safe reports.

## Start with Diagnostics

Open **Settings → Diagnostics** (older Diagnostics links redirect there). **Check environment** makes no model call. **Test runtime** starts a fresh native test session and can consume Provider quota; confirm the charge and native-hook implications first. Selecting a Room uses its stored selections, not necessarily the current default pair. The test does not resume or prove the health of an already-running Native binding.

For an active **Native Room**, use its diagnostic button or `pairroom relay doctor` inside the intended session. This is read-only relay diagnosis: association, collection, queue/unknown state, capability, last wake, and suggested action. CLI doctor also checks local installation/version/hook observations. It does not call a model or implicitly activate a suspended Room; native hook approval and model acceptance remain unknown.

An installed executable is not proof of authentication, model access, or tool correctness. Mock never certifies a native Runtime. Management's safe report excludes sensitive paths/identities; ordinary CLI JSON and Room bundles need separate review before sharing. See [CLI diagnostic boundaries](CLI_REFERENCE.md#installation-versus-runtime-availability).

## Agent will not start

In **Embedded**, run the selected CLI directly as the same OS user in the same repository. Check executable, login/Provider, working directory, policy, Runtime info, and first error:

```bash
pairroom doctor --repo /absolute/path/to/repository --json
```

Only selected Runtimes need to work. Empty overrides inherit native settings; an existing Binding must resume exactly. CC Switch failures remain visible in `pairroom providers --json` and the catalog; correct the reference or create an appropriate new Embedded Room instead of expecting fallback credentials. See [Configuration](CONFIGURATION.md).

In **Native**, PairRoom does not start Agents. Open the intended original sessions and follow [Native setup](NATIVE_RELAY.md). Verify `pairroom` in each Agent's tool shell, install/approve the project hooks, run `pairroom relay preflight`, and bind as an Agent tool call. A detached terminal or Grok `!` shell cannot supply the required session association. [Native relay errors](#native-relay-errors) indexes the exact messages.

## Turn is quiet, or Codex reports an error while working

In Embedded, a stall notice means no recent Runtime event, not a dead process. Inspect tools, approvals, process state, and the first error. A generic Codex `error` is diagnostic; only a reliable terminal boundary or confirmed exit releases Turn ownership.

If evidence indicates an unresponsive Turn, consider supported steering, then explicit interruption or normal restart with side-effect risk in mind. Interruption may affect the whole Turn. Do not force another participant to run concurrently or blindly retry a possible write.

Native activity is observational. Inspect the original harness and relay diagnostics rather than treating quiet UI, `handed_off`, or a wake receipt as proof of a stuck/completed model. Stop native execution through the original harness, not a nonexistent PairRoom Interrupt control.

## Peer message remains Waiting, or relay stops too soon

Embedded Waiting is expected while another participant owns the Turn. Native `queued` instead needs the bound receiver to collect: check park/foreground wait, capability/inbound policy, and wake observations. A queued message does not prove that a model has been awakened.

Automatic relay needs the exact current peer handle (`@claude`, `@codex`, `@grok`, or the displayed duplicate-runtime suffix). Role aliases and control markers do not route. A peer handle wins over `@user`; do not include one merely to acknowledge a final answer.

An unaddressed Embedded answer remains visible in the Room. An unaddressed **Native Stop** records only a publication receipt, not its private body. Use `@user` for a Native result intended for the Room/human. Explicit `send`/`exchange` uses its command target rather than body mentions; explicit send followed by peer-directed Stop may produce two messages. [Native publication rules](NATIVE_RELAY.md#what-is-published) explain this distinction.

There is no relay-count ceiling. Cancel queued work; use the appropriate host's interruption controls for accepted work. Do not assume Native enforces Embedded's stale-input cancellation or one-writer scheduling.

## A message did not resume after restart

In Embedded, only definitely pre-submission queued input is rebuilt automatically. Unknown acceptance fails for explicit Retry; accepted unfinished work is cancelled without replay. Inspect side effects first. A new execution retry has a new Message ID; a conflict may mean a retry for that source/participant already exists.

In Native, queued input persists. Interrupted delivery becomes `unknown`; the original claim's matching receipt may settle it while no Retry is pending. Otherwise inspect history and the workspace before explicit Retry. `handed_off` is terminal stdout evidence and cannot be re-collected simply because the model did not surface the output.

A lost **publication response** is different from retrying execution: retain the original client ID and immutable payload, check its receipt, and recover that publication identity rather than sending a new message. Browser refresh only queries; Forget does not cancel server work. Use `relay history --pending` / `--id ID` for read-only inspection. `status`/`reconcile` may reconcile pending Stop publication, so they are not pure history reads. See [Storage](STORAGE.md) and [Native recovery](NATIVE_RELAY.md#recovery-and-review-surface).

## Native relay errors

CLI failures print on stderr as `pairroom: <message>`, and Stop-hook notes as `PairRoom: <message>`. Below, `<...>` marks variable parts. Pick the check for your stage:

- **Before binding**, from any shell in the Project, run `pairroom relay preflight`. It changes nothing, prints JSON, and exits nonzero until `ready`. Read `next_steps` even when `ready` is `true`: a Service version mismatch, or a `pairroom` on PATH that is a different file, only warns.
- **After binding**, run `pairroom relay doctor` inside the bound session. It is read-only and shows `local.hook_installation`, `last_hook_at`, and version/protocol matches. Use `pairroom relay status --brief` for queue, unknown-delivery, and pending-publication counts. Unlike doctor, `status` may reconcile a pending Stop publication under its original sequence.

[Native relay](NATIVE_RELAY.md) owns the workflow, [CLI reference](CLI_REFERENCE.md#native-relay-commands) owns the flags, and [Protocol](PROTOCOL.md#native-host-protocol-v8) owns delivery semantics.

### pairroom is not on the PATH hooks use

- Preflight `cli.hint`: `The bare pairroom command is not on this shell's PATH, but the relay hooks run exactly that. ...`, or `The pairroom on PATH is a different file from the one running now. ...`
- In a bound session, `last_hook_at` stays empty after a finished turn because the Stop hook could not run `pairroom relay hook`.

Fix PATH for the harness itself, then restart that harness session. A running session keeps the environment it started with. On Windows, Desktop Setup adds `<install dir>\bin` to the machine PATH through its default-on `addtopath` task. If you opted out, or installed before v5.6.0, run Setup again with the task selected. On macOS, use **Install Command Line Tool…** in the menu bar (or **Update Command Line Tool…** after moving the app) to link `/usr/local/bin/pairroom`. The Linux AppImage needs a separately installed matching CLI. See [Installation](INSTALLATION.md#the-pairroom-command-on-path) and [the macOS section](INSTALLATION.md#macos-desktop).

### Service unavailable or stopped

- Bind/Management: `Service unavailable; verify current endpoint file`, or an OS error that names the missing `relay-endpoint.json`.
- Bound commands: `read current Service endpoint (is PairRoom running?): <error>` or `relay <action> transport unavailable`.
- Preflight `service.hint`: `No running Service found. ...` or `The endpoint file exists but the Service did not answer; it may have stopped uncleanly. ...`

Start or reuse the one Service that owns the intended data root; see [Desktop, daemon, and service.lock conflict](#desktop-daemon-and-servicelock-conflict). The first bind to a custom data root needs `--service-file <root>/relay-endpoint.json` as a path. If you stop the Service mid-session, the Stop hook saves the first reply before contacting it and prints `PairRoom: publication pending or unknown; inspect relay status. No new-ID replay was attempted.` The next hook or `pairroom relay reconcile` publishes that reply under its original sequence. A **later** Stop reply before the Service returns is not kept; send it explicitly with `relay send` if it matters.

After a Service restart, a Native Room stays suspended until a relay call or opening it in Management activates it. Until then, `relay doctor` fails with `relay doctor: room runtime is not active`, because doctor never activates a Room. Run `relay status --brief` first.

### Bind reports a missing session identity

- `<VAR> is missing; run bind as a tool call inside your native <runtime> session, not a plain terminal`, where `<VAR>` is `CLAUDE_CODE_SESSION_ID`, `CODEX_SESSION_ID`, or `GROK_SESSION_ID`
- `GROK_SESSION_ID is missing; run bind as an agent tool call inside your native grok session, not a plain terminal or Grok shell mode (!)`
- `bind requires --slot 1|2 (Agent 1/2) outside a recognized native session`, or `run bind as a tool call inside your native session; use --runtime when the harness cannot be identified`
- `conflicting native session metadata; run the command in the intended native session without inherited outer-session variables`
- Hook: `PairRoom: this bound harness reported a different session identity; no reply was published or collected. ...`

Ask the Agent to run the command as its own tool call inside the intended session. In Grok, paste the command as a normal prompt; `!` shell mode receives no `GROK_SESSION_ID` and does not run the Stop hook. Never set or copy a session variable by hand. Preflight's `caller` shows which harness and session it detected. See [Grok Build Native](CLI_REFERENCE.md#grok-build-native).

### Bind rejected because the Stop hook is missing

- `zero approved relay-hook setup is unsupported: run pairroom relay install --runtime <runtime>, review the project hooks in your harness, then bind again` (Grok's variant also suggests the reused Claude Code hook)
- `hooks are disabled; native association requires an approved Stop hook`, when the hook file sets `disableAllHooks`
- Preflight `hooks.<runtime>.status` is `missing`, `disabled`, or `error`

Despite the wording, bind checks only that the hook is **installed**; PairRoom cannot see approval. Run `pairroom relay install --runtime <runtime>` in the Project's worktree, then approve the exact definition: Codex `/hooks`, Claude Code project hook consent, Grok `/hooks` (press `r` to reload) plus folder trust. Review again after reinstalling or editing the file. An installed but unapproved hook lets bind, send, and wait succeed, but Stop replies never publish and `last_hook_at` stays empty after a finished turn. See [one-time project setup](NATIVE_RELAY.md#one-time-project-setup).

### No matching binding, or an ambiguous Room or slot

Service-side rejections are prefixed with `Service rejected request:` (or `relay <action>:`), and bind adds a retry note.

| Message contains | Meaning and fix |
|---|---|
| `this native session has no matching associated binding in this workspace; ...` | This session was never bound, or another session replaced its slot. Run `pairroom relay bind` in it; preflight's `caller.bound` shows the state |
| `no relay binding in this workspace; ...` / `no unique relay binding matches this caller; pass one explicitly: ...` | A plain terminal cannot pick a binding. Run inside the bound session, or pass one printed `--room`/`--slot` pair to inspect it |
| `no active native Room exists for this workspace; ...` / `multiple active native Rooms match this workspace; pass --room explicitly: <ids>` | Create with `bind --create`, or pass `--room`; archived Rooms are never candidates |
| `the "<runtime>" harness does not match exactly one slot of Room <id> (...); pass --slot 1\|2` | Both slots or neither use this Runtime; pass the intended slot |
| `this session's runtime does not match the selected Room slot` | The chosen slot selects another Runtime |
| `slot is occupied; run bind in the original session or explicitly --replace (cannot stop native work)` | Resume from the original session; `--replace` only for an intentional session change |
| `binding identity is already owned: native session is already associated with another Room slot` | This session is bound elsewhere, archived Rooms included. Reuse that binding or unbind it first |
| `this native session is already associated; use pairroom relay bind to resume it, not bind --create` | Resume the existing binding instead of creating a Room |
| `explicit relay target conflicts with this native session's binding; ...` / `native session matches multiple relay bindings; ...` | Drop the conflicting `--repo`/`--room`/`--slot`/`--service-file`, or pass all of them to choose one |
| `relay <action>: relay authentication failed: binding, generation and associated session must match` | The Room is archived, the slot was replaced, or the command reached another Service. Restore the Room or rebind in the intended session |

For a workspace that moved, was deleted, or has a broken locator, follow [workspace recovery](NATIVE_SESSION_WORKSPACE.md#upgrade-and-recovery). If `--create` made a Room but bind failed, run the printed recovery command rather than `--create` again.

### Uncertain send or delivery

- Send: `<error>; publication uncertain: retry with the SAME --id <id>, not a new ID`, or `publication receipt invalid; ...`
- Same `--id` with a changed payload: `relay send: client message ID already refers to a different body, target, attachments, quote, or review evidence; retry the original request unchanged; publication uncertain: ...`. The original was accepted, so do not send it again.
- Collection: `stdout written but acknowledgement unavailable; inspect Room delivery state, never automatically replay`. The acknowledgement was missing or was not `handed_off: true`.
- Collection: `wait response missing claim; outcome uncertain, collection stopped`, or `publication <id> confirmed; collection failed: ...`

Run `pairroom relay status --brief` for unknown counts and recovery IDs, and use read-only `relay history --id <id>` or `--pending`. Recover an uncertain send by rerunning the same command with the same `--id` and unchanged body and target. A new ID is a new message. Despite its `publication uncertain` suffix, the payload conflict above confirms the original was accepted. `--attach` always hits it, because re-uploaded images get new IDs. So do `--ref` or `--review` after the file or checkout changed. When stdout was written, the envelope is already in the tool output; read it and inspect workspace side effects before any explicit Retry. `relay reconcile --resend` or `--discard` decides an unknown Stop publication explicitly. See [delivery evidence](NATIVE_RELAY.md#delivery-evidence-remains-conservative) and [A message did not resume after restart](#a-message-did-not-resume-after-restart).

### Another collector is active

`another collector is active for this slot; keep its native tool pending or cancel it before collecting again; no message was sent or claimed by this invocation`

Another local `wait` or `exchange` holds this slot, often a background wait from an earlier turn. `exchange` checks this before publishing, so nothing was sent. `relay doctor` shows `collector_active`. Let that command return and handle its result, or cancel its tool call; a process that exits releases the lock. A Stop hook still publishes while a collector is active but leaves the inbox to it.

### Wait or exchange timed out

- `exchange`: `publication <id> confirmed; no incoming message before wait timeout (not task completion); continue with pairroom relay wait ..., not another send/exchange`. The message was sent; run the printed receive-only command.
- `wait`: an empty timeout exits 0 and prints nothing.
- `foreground wait timeout must be 0–21600 seconds (0 waits until cancellation)`

The one-hour default is a PairRoom budget; the harness's own tool timeout can end a wait sooner. Keep foreground waits within that limit, and use a long background wait only where the harness reports its completion. See the [foreground discussion loop](CLI_REFERENCE.md#foreground-discussion-loop).

### CLI and Service versions differ

- Preflight `service.version_match: false` with `The Service runs a different PairRoom version from this CLI. ...`. This only warns and does not block `ready`.
- `relay doctor` `local.service_version_match` or `protocol_match` is `false`.
- Grok hook: `Grok hook received a claim instead of readiness; update CLI and Service together; acknowledgement withheld`

Use the CLI from the Service's release, such as the one bundled with Desktop, and restart the harness sessions. Update CLI, Service, and the relay skill together; see [Upgrading](UPGRADING.md).

## pairroom prints nothing inside an Agent session

If `pairroom version` or another non-relay command exits without output, the shell may have inherited `PAIRROOM_LOG_FILE`. An installed daemon sets it, and Embedded Agents it starts pass it on to their tool shells. Every subcommand except `pairroom relay` then sends its stdout and stderr, errors included, to that log file; the exit code is unchanged. Relay is exempt so hook decisions and envelopes stay on stdout. Remove the variable for that command:

```bash
env -u PAIRROOM_LOG_FILE pairroom version
```

In PowerShell, clear it for the session with `Remove-Item Env:PAIRROOM_LOG_FILE`. Output already written this way appears in `pairroom daemon logs`.

## Cancel affected more than one input

In Embedded, waiting items can be cancelled precisely; after native acceptance, Interrupt may stop the whole Turn, including steered inputs. Unrelated Room FIFO entries are retained, but vendor per-input cancellation is not guaranteed. Native Cancel removes only queued work and cannot undo delivered messages or stop user-owned processes.

## Cannot change a model, mode, or permission

Embedded Runtime, Provider reference, model, effort, additional instructions, and collaboration are creation-time selections. Create a new Room for those changes. Only effective Permission profiles can change, at an idle boundary with no queue or pending approvals; failure must not broaden access.

Native Provider/model/effort/permissions belong to the original harness. Changing its configuration does not make stored Room metadata an effective policy readout. Host mode and stored collaboration cannot switch in place. Lead/Executor are responsibilities, not permissions or mention aliases; retired schemas/role controls cannot be restored by editing metadata.

## UI jumps, refreshes repeatedly, or loses live state

Check the running binary, console errors, and SSE reconnects. A tail reset calls for a fresh projection, never resubmitting commands. Record Room ID, sequence, browser, viewport, and a minimal reproduction. Preserve an uncertain send's original identity instead of clicking Send repeatedly.

Native Participants/Details panels are independent on wide screens and mutually exclusive on compact screens. Pending items are separate from recent chat: an old unresolved message is not lost merely because it left the tail. Closing a Room tab only closes the view; restore an archived Room before opening its surface. See [Native UI](NATIVE_RELAY.md#recovery-and-review-surface).

## Port, hostname, or token failure

Built-in listeners accept **numeric loopback only**. LAN/public addresses, wildcards, `localhost`, and other hostnames are rejected even with a token. Use protected SSH local forwarding for remote access.

For an occupied port, identify its owner; do not create another Service over the same root. Reopen the current authenticated Management URL after a restart invalidates browser sessions. Never share bootstrap URLs, cookies, or tokens. A custom Native Service uses `--service-file <root>/relay-endpoint.json` as a path, not pasted endpoint credentials. See [Security](../SECURITY.md).

## Desktop, daemon, and service.lock conflict

Desktop reuses an installed daemon or owns an embedded Service when none exists. It never installs a daemon on launch. Stale-lock recovery requires the recorded PID to have exited; live owners and unreachable installed daemons fail closed.

Run `pairroom daemon status`; stop a confirmed installed-daemon owner with `pairroom daemon stop` and allow graceful drain. Do not delete a live lock or kill an unrelated process. Compare data-root paths before acting. Close hides the desktop; Quit does not stop an external daemon or Native harness. Launch-at-login and daemon installation are separate. Source updates require a quit desktop and build tools, not another daemon installation.

## Backup, restore, or Room activation fails

Keep the original data. Inspect the first identity/schema/replay error:

```bash
pairroom verify --data-dir /absolute/path/to/room --json
```

Readers accept Store schema 12 with explicit host mode; earlier formats are retired before replay/repair. Never change metadata to make unsupported data appear valid. Missing/empty/gapped/replaced logs are not fresh Rooms. Restore a verified matching backup rather than renumbering history.

Backup/diagnostic outputs must stay outside the source Room directory, including symlink aliases. A Room backup excludes the Service root's user configuration, repository, vendor stores, and Native workspace credentials. Archive cannot stop Native work. Follow [Storage](STORAGE.md), [backup procedure](OPERATIONS.md#backup), and [Upgrading](UPGRADING.md).

For Native workspace/locator failures after changing cwd, follow [workspace recovery](NATIVE_SESSION_WORKSPACE.md#upgrade-and-recovery). Do not copy credentials into another worktree, bypass a corrupt locator with a different Room, or replace a valid binding merely to fix discovery.
