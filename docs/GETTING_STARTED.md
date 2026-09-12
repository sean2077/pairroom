# Getting started

This guide takes you from a release package to a first Room. For the adoption decision, read [Why PairRoom](WHY_PAIRROOM.md); for all options, use [Configuration](CONFIGURATION.md) and the [CLI reference](CLI_REFERENCE.md).

## Prerequisites

| Entry | Needed to use it | Needed only to build from source |
|---|---|---|
| Prebuilt CLI + browser | Git, a local Git repository, and a browser | Go 1.25 and the development tools in [Contributing](../CONTRIBUTING.md) |
| Prebuilt desktop | Git, a local Git repository, and the package's platform requirements | Go, pinned Wails, Python, and platform tools in [Desktop development](../desktop/README.md) |
| Real Agents, either entry | Each selected native CLI installed and authenticated for its selected Provider | No additional PairRoom source build |
| Mock, either entry | No vendor CLI or model account | None for a prebuilt package |

**Go is not required to run a prebuilt PairRoom binary.** Only install the native Runtimes you will actually select: Claude Code, Codex, and/or Grok Build. Two slots may use the same Runtime. Native host mode currently supports Claude Code and Codex, not Grok Build.

## Install a release

Choose the matching OS/architecture asset from [Releases](https://github.com/sean2077/pairroom/releases/latest). CLI assets start with `pairroom-cli-`; desktop assets start with `pairroom-desktop-`. A Windows desktop `-setup.exe` is not the standalone CLI `.exe`. Linux desktop assets are `.deb`/`.AppImage`; macOS uses `.app.zip`.

For Linux, macOS, or Git Bash, the CLI installer is:

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh -o install-pairroom.sh
# Inspect install-pairroom.sh before executing it.
sh install-pairroom.sh
pairroom version
```

Alternatively, download the CLI binary directly and verify it using the checksums provided with that release. Make it executable where required and put it on `PATH`. In Windows PowerShell, use `./pairroom.exe` when running a downloaded binary in the current directory.

Desktop packages are not claimed to be production-signed/notarized unless that release explicitly provides such evidence. See [Desktop development](../desktop/README.md#packages) for package boundaries.

## First run without vendor calls

Use a disposable Git repository and a separate PairRoom data root so the demo does not contend with an existing Service or reuse real Rooms.

Linux/macOS/Git Bash:

```bash
pairroom service --mock --data-root "$HOME/.pairroom-demo"
```

Windows PowerShell with the downloaded CLI:

```powershell
./pairroom.exe service --mock --data-root "$env:USERPROFILE\.pairroom-demo"
```

Open the Management URL printed at startup if the browser does not open automatically. The URL can contain an authentication token; do not publish it. Keep the foreground process running while using its Rooms. `Ctrl+C` requests normal shutdown.

In Management:

1. Register the absolute path of the disposable Git repository as a **Project**. Registration does not copy the repository.
2. Create an **Embedded Room** with two participants and new session Bindings. The name is optional; a generated name can be changed later.
3. Choose default Lead/Executor instructions or custom natural-language collaboration instructions. Inspect both Agent selections and native permissions before creating the Room.
4. Open the Room and send a small task to Agent 1. Inspect the conversation, Turn activity, message state, and participant diagnostics.

Mock is deterministic control-plane verification, not a language model. It does not demonstrate coding quality or prove a real Provider/CLI combination works. Use fresh real Rooms for real execution rather than treating a Mock transcript as a native session.

## First real Room

These steps use **Embedded**. To retain an independent Codex Desktop / Claude Code terminal, follow [Native setup](#keep-codex-desktop-a-native-room) instead; Embedded is not attachment to a live Desktop session.

Confirm that each selected CLI works independently as the same OS user and in the target repository:

```bash
# Run only the commands for Runtimes you intend to select.
claude --version
codex --version
grok --version

pairroom doctor --repo /absolute/path/to/repository --json
```

A version response or `doctor` probe is not an authenticated end-to-end coding test. Resolve missing executables, login, Provider, and policy problems in the native CLI first. PairRoom does not log in to a vendor for you.

Stop the isolated Mock Service, then start `pairroom service` without `--mock`, or open Desktop. Use a fresh Room in the normal or another explicitly chosen data root. In the Embedded creation form:

| Choice | Meaning |
|---|---|
| Runtime | Native Claude Code, Codex, or Grok Build; independent for each slot |
| Provider | Native CLI configuration, or a supported read-only CC Switch Profile reference |
| Model / effort / additional instructions | Explicit overrides; unspecified values retain native inheritance |
| Collaboration | Default Lead/Executor, or custom instructions; fixed at creation |
| Permissions | Native tool policy, separate from collaboration responsibility |
| Binding | New native session, or exact supported resumption of an existing one |

The catalog reports unavailable Runtimes and unsupported Profiles; it is not a network model marketplace. Only supported CC Switch configurations can be materialized. See [Configuration](CONFIGURATION.md) for the pinned schema and unsupported authentication/proxy cases.

**Both participants default to YOLO in new Embedded Rooms.** For the first real test, explicitly choose native read-only restrictions for both. Then ask Agent 1:

```text
Explain how this repository is built and tested. Ask your peer to check the
important claims against the files, then give me a short corrected answer.
Do not modify files. End when the answer is complete.
```

Check that one Agent works at a time, a needed peer response appears after the native Turn boundary, and the result contains actual repository evidence. A relay requires the peer's exact displayed mention handle; an agent's unaddressed reply intentionally ends the relay. See [Concepts](CONCEPTS.md#agent-relay).

Only after that smoke should you grant the permissions needed for implementation. Embedded permissions can change only at an idle boundary with no queued work or pending approvals. Runtime, Provider reference, model and saved collaboration remain creation-time selections; create another Room to change those choices.

## Keep Codex Desktop: a Native Room

Native is experimental. It keeps the original Claude Code/Codex sessions and does not own their processes. Configure models, Providers, effort and permissions in those harnesses; the Room's selection metadata does not apply overrides. Real authenticated multi-round acceptance remains a release gate, distinct from Mock and synthetic hook tests.

Start a non-Mock Service or use the one Desktop already owns. In the intended repository, install the relay hooks for the runtimes you will use:

```bash
pairroom relay install --runtime claude
pairroom relay install --runtime codex
```

Review and approve the exact project hooks in each native harness. Installation does not grant trust or bypass native approval. If using an isolated Service, follow the [CLI reference](CLI_REFERENCE.md#native-relay-commands) for its connection options rather than guessing defaults.

Open the two original sessions in that repository. For skill-based creation, ensure the Service's default pair matches those runtimes. With the installed `pairroom-relay` skill loaded, ask one session:

```text
/pairroom-relay Log-upload plan review
```

That session runs `pairroom relay bind --create`, returns its nonce and the other session's exact join command. Have the creator echo the returned nonce in its visible reply. Paste the join command into the **other** session and have it echo its own nonce. The approved Stop hooks establish the associations. Do not paste one session's nonce into the other; do not create another Room just to join the first.

Alternatively, create a Native Room in Management with the intended pair. **Skip the creation command above** and ask each recognized session to run `pairroom relay bind`, or use the exact Room/slot command returned by the CLI when there is ambiguity. After either setup path, check `pairroom relay status` from the bound session. Reuse the binding for later review rounds.

Only then send the review task below. Native hooks publish at response boundaries and wait for a bounded park window. Outside that window, a peer reply can remain queued rather than waking the idle model. Use the documented `relay wait` / nudge path in the already-associated session; inspect uncertain outcomes before explicit Retry. Do not start a duplicate session or assume that `handed_off` proves model acceptance. [Native protocol](PROTOCOL.md#native-host-protocol-v7) owns the limits and recovery semantics.

## Review first, execute where it fits

This is a task recipe, **not a new Room mode, phase machine or automatic approval gate**. Both participants can be high-capability reviewers. Use the actual peer handle provided by the Room/bootstrap; responsibilities such as Lead and Executor are not routing aliases.

### Discuss one plan

After setup, send this to either participant, followed by the concrete question, constraints and relevant file paths:

```text
Review this problem with your peer; do not implement or modify files yet.
Propose a solution, challenge each other's material assumptions with repository
evidence and counterexamples, and revise only where the evidence warrants it.
Work on the same question, not unrelated parallel tasks.

Keep follow-ups focused on new findings, changed assumptions and necessary
context; do not repeat the whole discussion or exchange acknowledgements.
Use your peer's exact current handle only when its response can materially
improve the result. When known material objections are resolved, or a product
decision needs me, return the plan, evidence, remaining uncertainty and tests
to me without another peer relay. Agreement is not proof. Wait for me to choose
implementation; review completion alone is not authorization to edit.
```

Select supported read-only native permissions when that boundary matters; the text above is not a sandbox. A useful finish is “no known unresolved material objection after checking these cases,” not an unsupported claim of a flawless plan. Both agents should verify claims rather than defer to each other's model name.

### Keep primary-checkout sessions and a task worktree

You can keep session entry points at the primary checkout while a project-owned helper manages `.worktrees/`. A session's start directory is not its authorization to edit. Before implementation or diff review, give both agents one exact task path and revision:

```text
Keep these sessions at the primary checkout. Follow this repository's existing
agent-scaffold/worktree and PR/MR rules. The task workspace is
C:/src/project/.worktrees/log-upload; verify that path and branch before work.
Read, edit, test and inspect Git changes there, not in same-named files in the
primary checkout. Use one writer at a time. Do not create another worktree for
each review round or run a merge/push/cleanup helper without authorization.
```

Replace the example path with the real, existing worktree. If creation is needed, let the project's normal owner create it first and share the result. PairRoom does not create an isolated worktree per participant or bypass host path restrictions. Confirm that a reviewer is looking at the writer's actual revision. Embedded's Turn rule is Room-scoped; Native has no enforced writer lock.

### Assign execution only when ready

In Native mode, give the chosen original session a direct instruction:

```text
Implement the reviewed plan in the agreed task workspace. Use your native tools,
skills and subagents as appropriate; do not add a separate orchestration layer.
Follow the project's tests and PR/MR policy. Ask the peer again only for a
material new uncertainty or when I request another review; otherwise finish
without a peer relay. Do not merge or delete workspaces without authorization.
```

The agent may execute without more pair turns; the other session can remain available for a later review. In Embedded, send the same instruction to the chosen participant in the Room. Before operating that same Embedded session from an external application, end/drain PairRoom's ownership and verify that runtime's supported resumption path. There is no automatic host-mode conversion or live attachment; do not run two owners of the same session.

A later review can simply name the diff/revision, acceptance criteria and unresolved risks. Reuse the existing pair instead of rebuilding its context or introducing a mandatory review ceremony. Newer user instructions take precedence over the initial recipe.

## Native sessions and identity

In Embedded, a new Binding materializes its native session as execution starts. An existing Binding must resume the exact selected session; it is not permission to silently substitute a new one. PairRoom does not import the vendor transcript from before the Binding. Native uses the approved hook/nonce association described above instead of spawning an adapter.

Room names and native session titles help you find the same task, but IDs remain authoritative. Rename through the Room context menu or explicit control; it waits for a safe boundary and does not interrupt a Turn. Embedded title synchronization can be pending, unsupported or failed without changing the underlying session. See the [API naming contract](API_REFERENCE.md#room-names-and-native-session-correspondence).

## Desktop, daemon, and exit

Opening Desktop **never installs a daemon**. It reuses an already-installed daemon, or owns an embedded Service when none is installed. An installed but unreachable daemon is an error to repair, not permission to create a second Service for the same data root.

**Settings → Desktop → Launch at login** is an explicit OS registration choice, independent of daemon installation. Close hides the window to the tray. Quit drains an owned embedded Service, but does not stop an external daemon. Closing a Room tab is not a command to stop its native work. Use explicit Room/participant lifecycle controls; [Operations](OPERATIONS.md) owns the full lifecycle, archive and shutdown rules.

Install a persistent background Service only as a separate intentional action with `pairroom daemon install`. It is not a prerequisite for trying the UI.

## Source development is a separate path

In a source checkout, install the dependencies from [Contributing](../CONTRIBUTING.md), then use `make dev`. This helper stops an installed daemon and runs the current-tree Service; do not use it as the next step after installing only a release binary.

For rebuilding/updating an existing desktop installation from source, use `make desktop-update` after quitting Desktop. It preserves data and login registration and does not install or reconfigure a daemon. Requirements and custom paths are in [Desktop development](../desktop/README.md#update-the-installed-desktop-from-source).

Before important work, read [Security](../SECURITY.md). For failures, start with [Troubleshooting](TROUBLESHOOTING.md); before changing versions, read [Upgrading](UPGRADING.md).
