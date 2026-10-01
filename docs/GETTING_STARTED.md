# Getting started

Choose the host mode before following commands. **Embedded** lets PairRoom drive the adapters and is the quickest first trial; **Native** keeps your original sessions and is recommended for daily work, though still experimental. Neither is an in-place conversion of the other. See [Concepts](CONCEPTS.md) for the boundary and [Native setup](NATIVE_RELAY.md) for the complete Native workflow.

## Prerequisites

| Entry | Required to use it | Source-development requirements |
|---|---|---|
| Prebuilt CLI + browser | Git, a local Git repository, and a browser | Go and tools in [Contributing](../CONTRIBUTING.md) |
| Prebuilt Desktop | Git, a local repository, and platform package requirements | Separate Wails/native dependencies in [Desktop development](../desktop/README.md) |
| Real Agents | Selected native CLIs installed and authenticated | No extra PairRoom source build |
| Mock | No vendor CLI or model account | None for a prebuilt package |

Prebuilt packages do **not** require Go. Install only the Runtimes you will use: [Claude Code](https://code.claude.com/docs/en/overview), [Codex](https://github.com/openai/codex), [Grok Build](https://docs.x.ai/build/overview), and/or [Gemini CLI](https://github.com/google-gemini/gemini-cli). Both slots may use the same Runtime. [Core concepts](CONCEPTS.md#tool-names-and-configuration-layers) explains how Runtime, model, and Provider differ.

## Install a release

Follow [Installation](INSTALLATION.md) for the appropriate platform/channel, then verify `pairroom version`. A Desktop installation does not by itself prove that `pairroom` is discoverable in each native Agent's tool shell; check PATH there before Native setup, and use the CLI from the same release as the Service.

## First run without vendor calls

Use a disposable Git repository and an unused data root, separate from any real Service:

```bash
# Linux, macOS, or Git Bash
pairroom service --mock --data-root "$HOME/.pairroom-demo"
```

```powershell
# Windows PowerShell with a downloaded CLI
./pairroom.exe service --mock --data-root "$env:USERPROFILE\.pairroom-demo"
```

Open the printed Management URL if a browser does not open automatically. Do not share the authenticated URL. Keep the foreground process running; `Ctrl+C` requests normal shutdown.

In Management, register the disposable repository's absolute path as a **Project**, then create an **Embedded Room** with two new Bindings. Inspect the Agent selections, default/custom collaboration instructions, and permissions. Send Agent 1 a small task and inspect conversation, Turn activity, and message state. Project registration does not copy files.

Mock is deterministic control-plane verification, not a language model or a vendor-authentication test. Use fresh real Rooms for real execution, not a Mock transcript as an existing native session.

## First real Room

These steps are for **Embedded**. To retain an independent Claude Code terminal, Codex CLI/Desktop, Grok Build, or Gemini CLI session, use the [Native path](#keep-codex-desktop-a-native-room).

Confirm the chosen CLI works independently as the same OS user in the intended repository:

```bash
# Run only the version commands for Runtimes you select.
claude --version
codex --version
grok --version
gemini --version
pairroom doctor --repo /absolute/path/to/repository --json
```

`doctor` checks Claude Code plus Codex unless you pass `--config` for another pair; [Agent-assisted setup](AGENT_SETUP.md#3-check-git-and-the-runtimes) has an example, including the optional live check. Neither version output nor `doctor` checks that you are signed in. Resolve executable, Provider/login, and native-policy failures in the harness first. PairRoom does not log in for you.

Stop the isolated Mock Service, then start a non-Mock `pairroom service` or open Desktop without starting a competing owner. In a fresh Embedded Room, choose Runtime, Provider, model/effort, instructions, collaboration, permissions, and new/existing Bindings. Empty overrides inherit native configuration. A read-only [CC Switch](https://github.com/farion1231/cc-switch) Profile reference is optional; its catalog lists your local Profiles, not models for sale. See [Configuration](CONFIGURATION.md).

**Both participants default to YOLO, the most permissive native permission setting.** For the first real test, explicitly select supported native read-only restrictions, then send:

```text
Explain how this repository is built and tested. Ask your peer to check the
important claims against the files, then give me a short corrected answer.
Do not modify files. End when the answer is complete.
```

Check actual repository evidence, one active participant Turn at a time, and peer delivery after a reliable native Turn boundary. Relay needs the peer's exact displayed handle; an unaddressed Embedded answer remains visible but ends relay. Only then grant permissions needed for implementation. Effective permission changes require an idle Room with no queued work or pending approval; Runtime/Provider/model and saved collaboration remain creation-time selections.

## Keep Codex Desktop: a Native Room

This path also applies to Claude Code, Grok Build, and Gemini CLI. Native is experimental: it keeps the original sessions and does not own their processes. Provider, model, effort, and permissions remain native-controlled; displayed Room metadata does not apply overrides. Historical or synthetic evidence is not current authenticated multi-round acceptance.

Start/reuse a non-Mock Service. Install the intended project's hooks and approve them in each harness, following [Native setup](NATIVE_RELAY.md#one-time-project-setup):

```bash
pairroom relay install --runtime claude,codex
```

Reload each harness if it requires that, then run `pairroom relay preflight` in both sessions. Read its `next_steps` even when `ready` is `true`, because a CLI/Service version mismatch is only a warning. Preflight cannot see hook approval. [Agent-assisted setup](AGENT_SETUP.md#6-native-bind-two-existing-sessions) has the full sequence.

In the first Agent session, load the installed skill and invoke:

```text
/pairroom-relay Log-upload plan review
```

That session creates and binds one Room, then prints the peer's join command. Ask the **other Agent** to execute that command through its native tool environment. Bind reads official tool-call session metadata (Gemini uses its approved BeforeTool hook; see [Gemini setup](NATIVE_RELAY.md#gemini-cli)) and is immediately ready; do not echo a nonce, wait for an initial Stop, or add a status check after every success. A detached terminal or Grok `!` shell is not the intended tool-call environment.

Alternatively, create a Native Room with the intended pair in Management and bind to it; skip skill-based creation. Zero-flag bind works only when selection is unambiguous. Reuse the binding across review rounds. If creation succeeded but bind failed, use the printed recovery command rather than `--create` again.

To confirm setup once, let each bound session finish a turn and check that `pairroom relay doctor` shows a recent `last_hook_at`, which means the Stop hook ran. There is no need to repeat this every round.

A peer-directed Stop reply is published in full; `@user` publishes a human-facing result. Without either handle, the private reply body is **not** copied to the Room. Explicit `send`/`exchange` instead uses its command target; avoid a second peer-directed final reply after explicit publication unless duplication is intentional.

Hooks receive during bounded park windows. Outside them, eligible Claude inbox and Codex queue capabilities can receive a body-free Service wake. Grok/Gemini, unavailable capabilities, or restrictive inbound policies need the documented foreground `wait` / human fallback; harness-owned background completion is useful only where it is surfaced to the model. `handed_off` is stdout evidence, not model acceptance. Use [Native recovery](NATIVE_RELAY.md#recovery-and-review-surface) rather than starting a duplicate session or blindly resending.

## Review first, execute where it fits

This is a task recipe, not a Room mode, phase machine, or automatic approval gate. Both participants may be high-capability reviewers. Use the actual current peer handle, not Lead/Executor as an addressing alias.

### Discuss one plan

Send the question, constraints, and relevant file paths with:

```text
Review this problem with your peer; do not implement or modify files yet.
Challenge material assumptions with repository evidence and counterexamples.
Work on the same question, revise where evidence warrants it, and exchange
new findings rather than the whole discussion or acknowledgements.

Use the peer's exact handle only when another response can materially improve
the result. When known material objections are resolved or a decision needs me,
return the plan, evidence, unresolved decisions and remaining uncertainty.
Agreement is not proof. Wait for me to authorize implementation.
```

For a Native result that should appear in the Room, address `@user` rather than ending with a private unaddressed Stop reply. Supported native read-only permissions enforce the tool boundary; the recipe itself is not a sandbox. Prefer a bounded conclusion such as “no known unresolved material objection after these checks” over a claim of flawlessness.

### Keep primary-checkout sessions and a task worktree

Session entry and task workspace are different concerns. Before implementation or diff review, share the exact task checkout and revision:

```text
Keep these sessions at the primary checkout. Follow this repository's existing
worktree and PR/MR rules. The task workspace is
C:/src/project/.worktrees/log-upload; verify that path and branch before work.
Read, edit, test and inspect Git changes there, not in the primary checkout.
Use one writer at a time. Reuse this task worktree across review rounds.
Do not merge, push or clean up through a helper without authorization.
```

Replace the example path with a real worktree created the way your project normally creates them; PairRoom does not require any particular worktree tool. A shell `cd` does not change native permissions or reload host instructions. PairRoom follows the confirmed Native session binding across directory changes; it does not create/merge worktrees or grant path access. Confirm the reviewer sees the writer's actual revision. [Optional Git review versions](NATIVE_RELAY.md#recovery-and-review-surface) help identify evidence but do not approve execution.

### Assign execution only when ready

In Native, instruct the chosen original session directly:

```text
Implement the reviewed plan in the agreed task workspace. Use your native tools,
skills and subagents. Follow the project's tests and PR/MR policy.
Ask the peer only for a material new uncertainty or a requested review;
otherwise finish without another peer relay. Do not merge or delete workspaces
without authorization.
```

In Embedded, send the equivalent instruction to the chosen Room participant. Do not operate that same Embedded session from another application until ownership is drained and its supported resumption path is verified. PairRoom offers neither automatic host-mode conversion nor live attachment to a second owner.

### Implementation cross-review

Use this opt-in recipe when the human requests review of an implementation. Reuse the existing pair and task worktree; simple tasks still need no review round. The installed `pairroom-relay` skill carries the compact recipe. It adds no review state, mandatory phases, approval gate, or permission to edit, push, merge, or release. Newer human instructions and the Room's collaboration rules take precedence.

**Pin the question and evidence.** Name the authorized task checkout, acceptance criteria, intended base commit and actual head commit, and whether staged, unstaged or untracked changes are included. Resolve branch names to full SHAs before handoff; choose the base that represents the intended change (for a PR this may be its merge-base), not an unrelated moving branch. Pause the writer during a dirty-worktree review or use an immutable commit/artifact. A commit SHA alone does not identify uncommitted changes. Record ignored/generated inputs separately when relevant; never send credentials as evidence.

In Native, use the existing [Git review observation](NATIVE_RELAY.md#recovery-and-review-surface). From the bound session, with a prepared request file outside the reviewed worktree:

```bash
pairroom relay send --id impl-review-v1 --text-file /absolute/evidence/request.md \
  --review --review-repo /absolute/task-worktree --review-base <full-base-sha>
pairroom relay review --id <published-message-id> --review-repo /absolute/task-worktree
```

The second command uses the returned `published` message ID, not the client ID `impl-review-v1`. Both participants select their own authorized checkout; an incoming path grants no access. Check that the captured base/head match the request, retain its dirty hash, and inspect the actual diff and files independently. `unchanged_observation` only means the bounded observation still matches; it is not a review result. A `stale`, `different_workspace` or `unverified` result blocks reliance on that observation: resolve the scope/access/evidence first. Use a new publication ID for changed evidence. In Embedded, share the same pinned scope and evidence in messages; Native relay commands do not apply.

**Ask for actionable findings.** Adapt this prompt to the change:

```text
Review the implementation at <authorized checkout>, base <SHA>, head <SHA>,
including <clean tree or specified dirty evidence>. Check <acceptance criteria>.
Review independently; do not edit files. Report material findings with a stable
ID, severity, path/line at that revision, reproduction or concrete code evidence,
expected versus observed behavior, and impact. Separate hypotheses and missing
coverage from demonstrated defects. Say which checks you actually ran and their
results; identify blocked or unrun checks. No finding is also a valid result.
```

**Dispose of each finding with evidence.** Keep short updates in the existing conversation, keyed by finding ID; no separate tracker or schema is required:

- `open`: a substantiated issue or a hypothesis still requiring evidence; identify the next check or decision
- `resolved`: name the fix revision and actual retest result; the reviewer checks the relevant change and evidence before treating it as resolved. A proposed fix or the writer's agreement alone is insufficient
- `not reproducible`: record the attempted command/input, revision, environment and result. This is a qualified outcome, not proof the report is false; say what evidence is still missing
- `disagreed`: retain both positions and their evidence, explain the material consequence, and ask the human if the remaining tradeoff or acceptance decision belongs to them

Only the authorized writer implements fixes. Exchange new evidence or a targeted question, not repeated acknowledgements or whole transcripts. Do not seek automatic consensus or keep looping without a useful next check. If access or a human decision blocks progress, report it explicitly. After a fix or any scope change, publish the new revision and review the relevant delta; do not carry a prior conclusion onto an unchecked revision.

**Close with a compact handoff.** Recheck the observation before the final summary (or independently verify the immutable evidence). State the reviewed base/head and dirty evidence, findings and dispositions, commands/results and checks not run, remaining risks, and exact human decisions needed. “No known unresolved material finding in this scope after these checks” is a bounded conclusion; agreement is not validation or merge authorization. In Native, send the result explicitly to `@user` (or use the human-directed Stop reply described above); avoid adding a final peer handle that would start another round.

These are instructions, not model-quality guarantees. Offline tests can check skill projection, evidence transport and stale-revision detection; authenticated model review effectiveness remains separate validation.

## Native sessions and identity

Embedded new Bindings materialize when execution starts; existing Bindings must resume exactly. Native binds associate from the tool-call environment. Neither path imports pre-binding vendor history.

Rename changes Room display metadata, not its ID or Binding. Embedded title synchronization may be pending, unsupported, or failed without changing the session. Native Room rename does not reconfigure the original harness. [API naming](API_REFERENCE.md#room-names-and-native-session-correspondence) owns the details.

The Room context menu separates **Close Room tab** from confirmed **Archive Room**. Closing a view does not stop, archive, or delete work. Native archive does not interrupt user-owned sessions. See [Operations](OPERATIONS.md#project-archive-delete).

## Desktop, daemon, and exit

Desktop never installs a daemon on launch. It reuses an installed daemon or owns an embedded Service when none exists; an installed but unreachable daemon is an error, not permission to create a competing Service. A desktop-owned Service can host both Embedded and Native Rooms.

**Settings → Desktop → Launch at login** is separate OS registration. Close hides the window to the tray. Quit drains an owned embedded Service but does not stop an external daemon or user-owned Native harnesses. Install a persistent daemon only as a separate intentional action with `pairroom daemon install`. [Operations](OPERATIONS.md#desktop-lifecycle) owns lifecycle details.

## Source development is a separate path

Install the dependencies in [Contributing](../CONTRIBUTING.md), then use `make dev` from a source checkout. It stops an installed daemon and runs the current-tree Service; it is not a step after installing only a release binary.

For a local source-based desktop replacement, quit Desktop and use `make desktop-update`. It preserves user data and login registration without installing/reconfiguring a daemon. See [Desktop development](../desktop/README.md#update-the-installed-desktop-from-source).

Read [Security](../SECURITY.md) before important work, [Troubleshooting](TROUBLESHOOTING.md) for failures, and [Upgrading](UPGRADING.md) before changing versions.
