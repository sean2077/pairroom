# PairRoom

**English** · [简体中文](README.zh-CN.md)

**Two independent coding agents. One problem. Your native workflow.** PairRoom connects [Claude Code](https://code.claude.com/docs/en/overview), [Codex](https://github.com/openai/codex), [Grok Build](https://docs.x.ai/build/overview), and [Gemini CLI](https://github.com/google-gemini/gemini-cli) sessions on one machine or over a LAN. They can review each other's work against repository evidence while keeping their model loop, tools, skills, and subagents.

<p align="center">
  <img src="docs/images/pairroom-native-room.png" alt="A Native Room relaying between your own Claude Code and Codex sessions">
</p>

## Why use it?

Having Claude Code draft a plan, copying it to Codex for review, and pasting the review back gets tiring when it repeats. PairRoom lets the two Agents talk directly: one proposes, the other challenges, supplements, or executes. You see the whole exchange and can step in at any time.

- **One pair, any supported combination.** Each slot can use any supported Runtime, including the same one twice. Task decomposition, tools, and subagents stay with each harness.
- **Flexible responsibilities.** By default Agent 1 plans and reviews while Agent 2 implements, verifies, and supplements. Custom instructions can make both reviewers or divide implementation. Simple tasks can finish directly; a finished review does not by itself authorize implementation.
- **Visible collaboration.** Published messages are visible in the Room. In Native, each side's work stays in its own terminal or client, where you can stop or correct it.
- **A colleague's Agent can join.** Only the LAN Room's host needs a Service. Each machine keeps its own workspace, credentials, and native permissions; local and joined Rooms can coexist through different sessions.

| Host mode | Choose it for | Boundary |
|---|---|---|
| **Native** (default and recommended) | Your existing sessions, including Codex Desktop, in your usual terminal (such as [WezTerm](https://wezterm.org/)) or client | PairRoom supplies bindings, durable relay, and audit. The original harness owns configuration, permissions, and execution; Room selections are display-only. |
| **Embedded** (optional) | PairRoom's desktop/browser conversation and adapter controls, with supported per-slot Provider, model, effort, and instruction choices | PairRoom owns the adapters and runs one native Turn at a time per Room. Unset options inherit native configuration. |

PairRoom follows your existing repository instructions, worktrees, and PR/MR policy. Relay adds no coordinating model or mandatory phases and does not append accumulated Room history to each message. No comparative token or cost saving has been measured.

[Why PairRoom](docs/WHY_PAIRROOM.md) covers fit and limits, [Alternatives](docs/ALTERNATIVES.md) compares tools such as the [Orca](https://github.com/stablyai/orca) Agent workbench using dated sources, and [Core concepts](docs/CONCEPTS.md) defines Runtime, Provider, slot, and Binding. For ready-made prompts, see the [review-first recipe](docs/GETTING_STARTED.md#review-first-execute-where-it-fits).

Gemini CLI works in both modes, including two Gemini sessions in one Room. Embedded currently supports new sessions only; restoring an accepted session after its process exits is blocked to prevent replaying old replies. See [Gemini setup and boundaries](docs/NATIVE_RELAY.md#gemini-cli) for its hooks, native authentication, and current limits.

## A look around

Screenshots use synthetic demo conversations; they do not show model output.

**Embedded Room** — PairRoom hosts both Agents; turn summaries, diffs and approvals sit in the work inspector.

![Embedded Room with a Claude Code lead and a Codex executor](docs/images/pairroom-embedded-room.png)

**Management** — Projects, Rooms in either host mode, and runtime capacity in one local shell.

![Project detail listing Embedded and Native Rooms](docs/images/pairroom-management.png)

**Create a Room** — choose the host mode, collaboration style and each slot's Runtime.

![Create Room dialog](docs/images/pairroom-create-room.png)

## Install and try

Download a package from [Releases](https://github.com/sean2077/pairroom/releases/latest). On Windows, use the desktop `setup.exe`; check [official-source availability](docs/INSTALLATION.md#winget) before choosing WinGet. Prebuilt packages do **not** need Go. [Installation](docs/INSTALLATION.md) covers each platform, CLI availability, upgrades, and uninstalling.

To install only the CLI on Linux, macOS, or Git Bash, download the installer first:

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh -o install-pairroom.sh
```

Read `install-pairroom.sh`, then run it:

```bash
sh install-pairroom.sh
pairroom version
```

For daily work, install and sign in to the native CLIs you will use. To host a Room, open Desktop or run `pairroom service`, then follow [Native setup](#native-host-mode). To join a colleague's Room, use the [LAN path](#work-with-a-teammate-on-the-lan); no local Service is required. New Rooms default to Native; existing Rooms keep their stored mode. `pairroom version` confirms which CLI this shell runs, not native sign-in.

For an optional demonstration without a model account, start a foreground demo Service with an unused data root:

```bash
pairroom service --mock --data-root "$HOME/.pairroom-demo"
```

In Management, register a throwaway Git repository as a Project, explicitly select **Embedded** when creating the Room, and send a small task. Mock never launches a vendor CLI or uses quota, so it shows the workflow, not model quality. `Ctrl+C` stops it. The startup URL carries a login token; don't share it.

Choose **Embedded** explicitly when you need PairRoom-owned adapters or supported per-slot Provider overrides; follow the [Embedded walkthrough](docs/GETTING_STARTED.md#first-embedded-room).

To have your coding Agent walk you through installation and checks, point it at [Agent-assisted setup](docs/AGENT_SETUP.md):

```text
Read https://raw.githubusercontent.com/sean2077/pairroom/main/docs/AGENT_SETUP.md
and help me install PairRoom and check my environment. Ask before each change.
```

## Important boundaries

- **New Embedded Rooms default to YOLO for both participants**, the most permissive native permission setting. Choose narrower permissions explicitly when you want them; [Configuration](docs/CONFIGURATION.md) lists them. Native Rooms keep whatever permissions each harness already has.
- Responsibilities do not restrict tools, and neither mode locks the repository against other writers. In Native, Turn ownership is advisory.
- There is no automatic cap on relay rounds or cost.
- After a crash, queued work stays queued and uncertain deliveries wait for you to inspect them; nothing is replayed blindly.
- The hosting Service stores Room history; LAN guests keep their own private client state. Model requests still go to each Provider.

See [Security](SECURITY.md), [Concepts](docs/CONCEPTS.md), and [Storage](docs/STORAGE.md).

## Native host mode

For two sessions on your machine, make sure `pairroom` runs in both Agents' tool shells and one non-Mock Service is running for your data root. [Agent-assisted setup](docs/AGENT_SETUP.md#6-native-bind-two-existing-sessions) covers installation and checks. From the project worktree, install hooks for the Runtimes you use:

```bash
pairroom relay install --runtime claude,codex
```

Approve the hooks in each harness and reload it as required. Before binding locally, run `pairroom relay preflight` in each session to check PATH, the Service, and hook installation. Read its `next_steps`, including version warnings; it cannot see whether you approved the hooks.

In the first session, load the installed skill and run `/pairroom-relay <topic>`. It creates the Room and prints a join command; ask the second session's Agent to run that exact command. Bind reads official tool-call session metadata; Gemini needs its approved BeforeTool hook. Once both bind successfully, reuse the Room across rounds. After each session's first completed turn, a recent `last_hook_at` in `pairroom relay doctor` confirms its response hook ran. See the [first Native Room](docs/GETTING_STARTED.md#first-native-room) walkthrough.

How replies travel:

- A Stop reply that mentions the peer's exact handle (returned by `bind`) goes to the peer in full; `@user` publishes it to the Room for you.
- **A Stop reply with neither handle stays private and is not copied into the Room.**
- `relay send` / `exchange` deliver to the command's target regardless of mentions in the body. Using both paths for the same content produces two messages.

See [publication rules](docs/NATIVE_RELAY.md#what-is-published).

Response hooks collect during bounded waiting windows. Beyond them, eligible Claude Code/Codex sessions can receive a fixed, body-free wake; Grok/Gemini need their documented collector or human fallback. Waiting in the CLI makes no model calls; subsequent model work still costs quota. PairRoom never starts or interrupts native sessions. Check the [Runtime-specific continuation limits](docs/NATIVE_RELAY.md#long-unattended-runs-by-runtime) before relying on unattended work.

[Native relay](docs/NATIVE_RELAY.md) owns daily commands, file evidence, cwd/worktree discovery, and recovery. The skill is also available through `npx skills add sean2077/pairroom`; installing the skill alone does not install or approve hooks.

### Work with a teammate on the LAN

Only the Room's host needs a Service. Enable hosting in **Settings → LAN collaboration**, then create a Native Room with **Invite a teammate over LAN**, or run `pairroom relay bind --create --share lan`. Send the returned join command to the colleague. The colleague installs and approves hooks on their machine, then asks their Agent to run `pairroom relay preflight --join` followed by that command. Accept the exact receipt they return through your existing trusted channel; the guest repeats the same join command to finish admission. The invitation alone grants no Room access.

One machine can host its own Rooms and join other hosts concurrently through different native sessions; each binding retains its own host. Both owners can inspect shared history. An optional guest Service adds a **Joined Rooms** view, human messaging, and supported local wake observation; Agent commands and hooks work without it. With no live receive path, messages stay queued at the host. See [Native LAN collaboration](docs/LAN_NATIVE.md) for evidence uploads, reconnecting, leaving, and revoking access.

### Inspect and recover without replay

Use the Native Room's conversation and Work inspector, `pairroom relay doctor`, or `pairroom relay history --pending` to inspect delivery. `handed_off` means the CLI wrote the message to stdout, not that the model read or completed it. Inspect uncertain effects before an explicit Retry. Optional Git review versions record the reviewed evidence, not approval. [Native recovery](docs/NATIVE_RELAY.md#recovery-and-review-surface) owns receipt recovery and the controls available in each view.

## Desktop and source development

Desktop and the browser share the same Management Shell and Service. Desktop never installs a daemon at startup: it reuses an installed daemon or runs its own embedded Service. **Settings → Desktop → Launch at login** only changes the operating system's login registration. Closing the window hides it to the tray, and quitting does not stop an external daemon. Closing a Room tab neither archives the Room nor stops native work. See [Operations](docs/OPERATIONS.md).

With the source-development dependencies installed:

```bash
make dev            # stops an installed daemon; runs the current-tree Service
make docs-check
make check
make smoke
```

Desktop source builds use `make desktop-build`, `make desktop-package`, and `make desktop-update`; release packages need none of them. See [Contributing](CONTRIBUTING.md) and [Desktop development](desktop/README.md).

## Documentation and support

[Documentation map](docs/README.md) · [Configuration](docs/CONFIGURATION.md) · [CLI](docs/CLI_REFERENCE.md) · [API](docs/API_REFERENCE.md) · [Troubleshooting](docs/TROUBLESHOOTING.md) · [Upgrading](docs/UPGRADING.md) · [Support](SUPPORT.md)

Documentation on `main` tracks development. For an installed release, read that tag's documentation and use the binary's `--help` for flags. The [Changelog](CHANGELOG.md) records history; the reference pages describe current behavior. [Support](SUPPORT.md) distinguishes compatibility and validation evidence; synthetic demos and dated working-session reports do not certify current vendor combinations. Desktop packages are not production-signed or notarized. The interface is available in English and Simplified Chinese; technical documents are in English.

## Friends

- [LINUX DO](https://linux.do) — a technical community for sincere sharing and friendly discussion, where PairRoom's own discussion and feedback are also published

## License

[MIT](LICENSE) · [Third-party notices](THIRD_PARTY_NOTICES.md)
