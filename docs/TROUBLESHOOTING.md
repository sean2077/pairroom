# Troubleshooting

Start by identifying the layer: Service, Room, participant/native Runtime, or browser. Preserve the first error and relevant IDs before restarting. Use [Support](../SUPPORT.md) for a safe bug report and [Operations](OPERATIONS.md) for lifecycle commands.

## Start with Diagnostics

Open **Diagnostics** from the sidebar (or the command palette, `G D`). **Check environment** does not call a model. Use **Test runtime** with a selected Agent and configuration to check native startup plus a real response; confirm the possible Provider charge and native-hook behavior first. To diagnose one failing Room, use its Diagnostics action in Runtimes or its Project and select its stored configuration rather than the current default pair. Leaving the page cancels an in-flight check. Download the allowlisted report for support; raw native output is intentionally omitted.

An installed CLI is not proof of authentication, model access, or a usable response. Mock results never certify a native Runtime. A passing live response is a point-in-time connectivity check, not proof of every tool, native hook, working repository, or long-running task. [CLI reference](CLI_REFERENCE.md#installation-versus-runtime-availability) explains `doctor --live` and report boundaries.

## Agent will not start

Run the selected native CLI directly as the same OS user and in the same repository. Check its executable, authentication, Provider, working directory, and policy. Then inspect `pairroom doctor --repo /absolute/path/to/repository --json`, the participant's Runtime info, and `LastError`.

Only the Runtimes selected for your two slots need to work. Empty PairRoom overrides inherit native configuration; PairRoom does not replace vendor login. A configured existing Binding must resume exactly. A missing native session is not permission to silently start a different one.

For CC Switch failures, inspect `pairroom providers --json` and the catalog's disabled reason. Missing/deleted Profiles, unsupported credential or proxy arrangements, locked databases, and schema mismatch fail closed. Correct the referenced Profile or create a new Room with the intended selection; do not expect a fallback to native credentials. See [Configuration](CONFIGURATION.md).

## Turn is quiet, or Codex reports an error while working

A stall notice means no recent runtime event, not “the process is dead.” Inspect tool activity, approvals, native process state, and the first error. A long command, context compaction, or unexposed native step can be quiet. A generic Codex `error` notification is diagnostic; only a reliable terminal boundary or confirmed process exit releases the native Turn owner.

Do not infer a hang solely from elapsed time or the absence of `turn/completed`. When the evidence indicates the native Turn is unresponsive, consider steering if supported, then explicit interruption or normal restart according to side-effect risk. Interruption can affect the whole native Turn. Never force another participant to run concurrently or blindly retry an uncertain write.

## Peer message remains Waiting, or relay stops too soon

Waiting is expected while the current participant still owns the native Turn. Check the owner and queue depth before treating the queue as stuck.

Agent relay requires the other participant's exact current handle: unique Runtimes use `@claude`, `@codex`, or `@grok`; duplicate Runtimes use the displayed `0/1` suffixes. An unsuffixed duplicate is ambiguous. Role aliases and old control markers do not route. Without an exact peer handle, the Agent's reply intentionally ends relay, even if the human expected a discussion.

`@user` alone returns the decision to the human; a peer handle in the same answer takes priority. Do not add the peer handle merely to acknowledge a final answer. There is no hop limit, so stop unwanted repeated relay with Cancel/Interrupt or a newer instruction. Exact matching exclusions and removed aliases are in [Protocol](PROTOCOL.md).

## A message did not resume after restart

Only queued input that provably never crossed native submission is automatically rebuilt. Input caught in the acceptance window fails for explicit Retry; accepted unfinished work is cancelled without replay. This avoids guessing whether an external side effect already happened.

Inspect the repository and native session before retrying. A retry creates a new Message ID. HTTP 409 on Retry can mean a direct retry of that source/participant is already waiting or working; inspect that attempt instead of sending duplicates. Never hand-edit old JSONL IDs or processing state. See [Storage](STORAGE.md).

## Cancel affected more than one input

A waiting FIFO item can be cancelled precisely. After a native Runtime accepts input, interruption may be scoped to the whole active Turn, including additional inputs steered into it. PairRoom retains unrelated Room FIFO entries, but cannot promise vendor-level per-input cancellation after acceptance.

## Cannot change a model, mode, or permission

Runtime, Provider reference, model, effort, additional instructions, and collaboration are creation-time selections. Create another Room to change them. They are not Settings edits on an existing Room.

Only effective native permissions can change inside a modern Room, and only when both participants are idle, no queued work exists, and no approvals are pending. A failed transition must not broaden access. Role controls and old Room formats are unsupported; create a new Room rather than editing schema markers. Lead/Executor are responsibilities, not permission profiles or mention aliases.


## UI jumps, refreshes repeatedly, or loses live state

Check the actual binary/version, browser console, and SSE reconnects. The UI should update incrementally, preserve drafts and disclosure state, and rebuild current state from a fresh snapshot after a replay gap. A bounded event-tail reset is not a completed Turn.

Record the Room ID, sequence, browser, viewport, and minimal reproduction. Do not repeatedly click Send after an ambiguous network failure: inspect durable Message state first. For a Room removed by archive/delete, refresh Management rather than reusing an obsolete embedded surface.

## Port, hostname, or token failure

All built-in listeners accept **numeric loopback only**. LAN/public addresses, wildcard binds, `localhost`, and other hostnames are rejected **even with a token**. Use a numeric loopback address; use SSH local forwarding for remote access, retaining normal authentication. PairRoom does not become a remote server by setting `--token`.

For an occupied port, inspect the existing process rather than opening a competing Service on the same data root. Browser sessions can expire or be invalidated by a Service restart; reopen the current authenticated Management URL or use its token login. Never share complete startup URLs, cookies, or tokens. See [Security](../SECURITY.md).

## Desktop, daemon, and service.lock conflict

Desktop never installs a daemon. Without an installed daemon it owns an embedded Service; with one installed it must reuse that owner. It may recover a crash-stale lock only after confirming its recorded PID has exited, then start/restart the daemon. A live owner or an installed daemon that remains unreachable still fails closed.

Run `pairroom daemon status`. Stop a confirmed installed-daemon owner with `pairroom daemon stop` and allow graceful drain. Do not delete a live lock or force-kill an unrelated process. Explicitly selected data roots/configurations can refer to a different Service; compare the reported paths before taking action.

Closing the desktop window hides it. Quit stops only an embedded Service it owns, not an external daemon. Launch-at-login registration is separate from daemon installation. Updating the desktop from source requires the build tools and a quit desktop, not another `daemon install`; see [Desktop development](../desktop/README.md#update-the-installed-desktop-from-source).

## Backup, restore, or Room activation fails

Preserve the original data directory. Check the first identity/schema/replay error with `pairroom verify --data-dir /absolute/path/to/room --json`. Current readers accept Store schemas 9 and 10; do not edit metadata to make another schema appear supported.

A missing, empty, gapped, or replaced Event Log is not an empty existing Room. Restore a verified matching backup rather than recreating or renumbering history. Check repository availability, exact native Binding, and complete backup contents separately.

Backup and diagnostics outputs must be outside the source Room directory, including symlink aliases. A Room backup is not a full Service-root, repository, or native-session backup. Damaged gzip/trailer or manifest validation failures need a sound backup, not disabled validation. Follow [Storage](STORAGE.md), [Operations](OPERATIONS.md#backup), and [Upgrading](UPGRADING.md).
