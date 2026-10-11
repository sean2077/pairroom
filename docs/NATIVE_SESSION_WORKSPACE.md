# Native session workspace discovery

A Native Room belongs to its confirmed binding, not to the directory of the
latest tool call. Session entry directory, current command directory, Git
checkout root and binding workspace are different concepts. Switching to a
subdirectory, another linked worktree, a nested repository or a non-Git temporary
directory does not change the bound Room, participant slot or credentials.

## Resolution and setup

Foreground relay commands and approved response hooks (Stop/StopFailure, or
Gemini AfterAgent) first locate the exact `(runtime, session_id)` binding and
read its original `State.Workspace`.
Explicit `--repo`, `--room` and `--slot` are selectors, not permission to switch
an already-bound session to a different target. `--service-file` selects a local
hosting Service; a LAN client retains its own host endpoint and pin and does
not use that flag. Conflicts
fail; multiple matches require explicit disambiguation. The caller identity is
checked again after acquiring the slot lock, including local-only unbind, so a
concurrent replacement cannot transfer another session's inbox to the caller.

For initial setup, an explicit `--repo` selects the Git workspace. A new
`bind --room <id>` without `--repo` obtains the project's root from the selected
Service, even when invoked outside Git. Otherwise cwd and, for a Claude caller,
`CLAUDE_PROJECT_DIR` provide workspace hints. If they identify different Git
checkouts and no existing binding resolves the choice, specify `--repo`; neither
the initial directory nor the nearest Git root silently wins. A bound session's
`bind --create` is rejected even after changing directories.

For an initial LAN join, run `preflight --join` and `join '<invite>'` in the
intended native session/workspace. This path reads no local Service endpoint and
starts no Service. A direct binding records its remote host independently of
local Service configuration. Different native sessions on one machine can use
locally hosted Rooms and several remote hosts concurrently; a single session
does not acquire multiple active identities by switching a global server.

`relay install` remains an explicit workspace-scoped setup operation: install in
the intended project or pass `--repo`. Hook installation and native consent are
unchanged. Finding a binding does not cause a harness to load hooks it has not
approved, and no global hooks, directory-change hooks or vendor transcript scans
are introduced. Gemini's approved BeforeTool hook supplies its official session
identity to the next `run_shell_command` relay call; it has a separate private
caller record and a two-minute freshness boundary. It does not make cwd or a
shared `node` PID sufficient identity. See [Gemini setup](NATIVE_RELAY.md#native-setup).

## Passive hook boundary

Installing hooks does not bind every session in a project. A valid unbound
response-hook invocation returns only the neutral `{}` hook result, with no
state or credential writes, slot/collector lock, publication,
collection, park wait, or Service startup. It writes one stderr line only when
this workspace holds a confirmed binding for the same Runtime that belongs to
another session (see [upgrade and recovery](#upgrade-and-recovery)). It does not read the Service endpoint
or make HTTP requests, even when that endpoint is missing, malformed, stale, or
points to a stopped Service. This also applies to Grok reusing the Claude hook.
A shared or reused harness PID is not evidence that a fresh session is bound.
Gemini BeforeTool is a setup exception: a relay shell call records the official
session/process observation needed for bind, without publishing, collecting or
contacting a Service. Unrelated Gemini shell calls remain inert.

Cold hook discovery ignores unreadable workspace candidates and refuses unsafe
walks without using partial results; it never follows a symlink or borrows another
session's credentials. Confirmed per-session locators, matching binding identity
checks, and exact bound environment/session disagreements still fail closed.
An already-bound session's Service or credential failures are not hidden, and
explicit foreground commands retain their existing diagnostics. Recover a broken
older binding through an explicit command, not automatic hook repair.

## Disposable locators

The user configuration directory contains
`pairroom/relay-sessions/<hash(runtime,session)>/<hash(workspace,room,slot)>.json`.
Each owner-only file holds only a schema number, workspace, Room, slot, bind ID
and generation. It contains no session transcript, publication body, Service
token, relay credential or vendor configuration. Filenames never use raw session
metadata. Directories reject symlinks; locator and workspace state reads retain
bounded regular-file and private-file checks. Windows inherits the user's
configuration-directory ACL, as with the existing workspace credential files.

Confirmed bind writes the locator after promoting private state, before deleting
its bind journal. A locator failure reports that the binding was committed and
prints an exact idempotent bind recovery command; do not repeat `--create`.
Ordinary foreground use and approved hooks refresh missing locators from confirmed
state, without rewriting unchanged files. Successful unbind removes its locator;
replacement cleans the previous session's pointer when possible. Failed cleanup
cannot authorize the old session: every lookup rechecks the original private
state, runtime, session ID, bind ID and generation. The hosting Service still
validates the local credential or admitted LAN key and binding generation on
transport requests. Direct bindings also require a matching, accepted per-user
client record: dashboard leave/detach or explicit replacement immediately makes
old workspace files and locators ineligible. Missing or malformed authority
fails closed; discovery does not repair it or replay its saved publication state.
A locator is not a second
source of binding truth and cannot restore removed credentials.

Session locators have no shared mutable index file or cross-session map lock. Each binding
owns a separate atomic locator file. Discovery is bounded to at most 128 directory
entries per session. Redirected indexed workspaces, malformed records,
unsafe paths and ambiguous matches fail rather than falling through to cwd. A
bound workspace that no longer exists (for example a removed task worktree) is
skipped like a removed slot directory. The missing workspace no longer supplies
a confirmed session binding, even if a separate LAN client record remains in
the per-user store. A retained client key does not authorize a different native
session to inherit that binding. Other intact bindings remain usable.

## Upgrade and recovery

Existing local schema-2 binding files remain valid. Direct LAN bindings use
schema 3 and their own private per-user client store; this does not migrate
existing local bindings or change their host target. A cold direct lookup checks
the bounded same-user client catalog for the exact native Runtime/session before
any local Service discovery. It resolves the original workspace and fixed host
even after disposable locators are removed and cwd changes; missing original
workspace or conflicting binding state still fails closed. This read-only path
also supports direct preflight and approved hooks without a local Service.
On a cold local foreground
lookup, the CLI checks workspace hints and then,
when no explicit workspace was supplied, at most 128 project roots registered by
the selected local Service. This rebuilds discovery without scanning disks or vendor
stores. A successful ordinary relay call or idempotent bind records the locator.
For an older binding under a custom Service, provide its `--service-file` once
or use the original workspace explicitly:

```bash
pairroom relay bind --repo "/absolute/path/to/bound-workspace"
```

Run this in the original native session; it resumes the existing binding rather
than rotating its identity. Subsequent commands can omit workspace and Service
flags. Inspect `relay status`'s local `binding_workspace` when diagnosing routing.
An indexed session needs no Management discovery HTTP or Git subprocess merely
to find its workspace. Stop hooks never query Management to discover an unbound
session: old hooks without a locator use their cwd/project hints; a foreground
call can rebuild a locator when both hints have moved. Unbound hooks are inert,
including outside Git: stdout is `{}`, the exit code is zero and nothing is
written or sent. When the workspace has a confirmed binding for the same Runtime
that belongs to another session (typically after Claude `/clear`/resume or a new
Codex thread), the hook adds one stderr line naming that Room and slot and the
explicit local `bind --replace` decision, without session identities or reply
text; it never rebinds. A LAN session replacement requires a fresh invitation
with explicit `join --replace`, after the host confirms the previous membership
ended. Use the original workspace and host route and obtain fresh receipt
approval; retained old receipts and publication state are never replayed. Hook environment/session disagreement remains an error,
not an invitation to associate another identity.

A corrupted disposable locator fails closed; the reported error names the exact
locator file, which must be inspected or removed before recovering through an
explicit original workspace. Explicit `--repo` does not bypass a locator that
cannot resolve. Do not remove workspace
credentials or copy `.pairroom` into a task worktree as a path workaround.

## File and permission boundaries

Discovery never calls `chdir`. Relative `--text-file`, `--ref`, `--attach`, `--file`,
`--output-file` and explicit `--service-file` paths are interpreted relative to
the actual command invocation, not the binding workspace. A session can therefore
publish evidence from a task checkout while continuing in its original Room.
File access remains subject to native harness permissions and existing PairRoom
validation. Different worktrees are not merged through `git --git-common-dir`.
A fresh session in the same directory never inherits a prior session's binding.

The regression tests cover native runtime identities, directory/worktree drift,
explicit conflicts, cold lookup, custom Services, stale generation/replacement,
private/symlink checks, CLI relative files and a synthetic Stop publish/collect
cycle. These are deterministic tests, not authenticated vendor-runtime E2E.
