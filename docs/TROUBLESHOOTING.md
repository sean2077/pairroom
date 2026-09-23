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

In **Native**, PairRoom does not start Agents. Open the intended original sessions and follow [Native setup](NATIVE_RELAY.md). Verify `pairroom` in each Agent's tool shell, install/approve the project hooks, and bind as an Agent tool call. A detached terminal or Grok `!` shell cannot supply the required session association.

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
