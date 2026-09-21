# PairRoom

**English** · [简体中文](README.zh-CN.md)

**Two independent coding agents. One problem. Your native workflow.** PairRoom connects supported Claude Code, Codex, and Grok Build sessions for evidence-based cross-review without replacing their native coding harnesses. Review a plan together, then let the chosen agent execute normally; its tools, skills and subagents remain its business.

<p align="center">
  <img src="docs/images/pairroom-runtime-overview.png" alt="PairRoom collaboration interface">
</p>

## Why use it?

Use PairRoom when carrying proposals, objections and corrections between two existing sessions has become repetitive work. The goal is a better decision with less coordination, not another mandatory agent hierarchy.

- **Review the same problem.** Both participants can be high-capability reviewers. Default Lead/Executor responsibilities are flexible; simple tasks stay with the addressed agent. Custom natural-language instructions need no phase compiler. Review completion does not automatically authorize implementation.
- **Keep the interaction surface you need.** Embedded provides PairRoom controls over supported native adapters. Experimental Native keeps your own Claude Code / Codex / Grok Build sessions, including the intended Codex Desktop workflow, with approved hooks and bounded relay rather than process ownership.
- **Keep the project's workflow.** Retain repository instructions, agent-scaffold, worktrees and PR/MR policy. Relay forwards the complete addressed reply without appending accumulated Room history. Compact byte budgets are not a guarantee of lower billed tokens or greater accuracy.

| Host mode | Configuration and control | Boundary |
|---|---|---|
| **Embedded** | Each slot independently selects a supported Runtime, Provider, model, effort and instructions; unspecified overrides inherit native configuration. Save a usual pair as an [Agent pair profile](docs/CONFIGURATION.md#agent-pair-profiles). | PairRoom schedules one participant Turn at a time and exposes its Room controls; this is not an independent Codex Desktop UI. |
| **Native (experimental)** | Each original harness controls its Provider, model, effort, tools and permissions; PairRoom supplies bindings, durable relay and audit. | Room configuration fields do not override the native process. Automatic continuation is bounded; authenticated multi-round vendor E2E remains a release gate. |

Native harnesses already have subagents and multi-agent features. Orca also supports real same-problem collaboration, not only parallel jobs, and offers a broader workbench. Choose PairRoom for its particular cross-session review workflow, not an imaginary absence of those capabilities elsewhere.

**[Why PairRoom](docs/WHY_PAIRROOM.md)** explains fit, costs and limits. **[Alternatives](docs/ALTERNATIVES.md)** compares Orca, native Claude Code/Codex, Cherry Studio and other options using dated primary sources. **[Review-first workflow](docs/GETTING_STARTED.md#review-first-execute-where-it-fits)** has prompts for discussing a plan and returning execution to the chosen harness.

## Install and try

Download a package from [Releases](https://github.com/sean2077/pairroom/releases/latest); on Windows, install the desktop app with `winget install PairRoom`. The [Installation guide](docs/INSTALLATION.md) covers every platform and channel: prerequisites, silent installation, per-channel upgrade/uninstall, and the one-time Windows NSIS transition.

Fastest CLI path on Linux, macOS, or Git Bash (review the installer before executing it):

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh | sh
pairroom service --mock
```

**Prebuilt CLI and desktop packages do not require Go.** Start with a disposable Git repository and a fresh Mock Room. Mock does not launch vendor CLIs or consume model quota. Avoid a data-root conflict with an already-running Service; the [Getting started guide](docs/GETTING_STARTED.md) shows an isolated demo and both real-host paths.

In Management, register the repository as a Project, create an Embedded Room, select both participants and permissions, then send a small task. For a real discussion-only test, first select native read-only restrictions where supported, then use:

```text
Review this plan with your peer against the repository. Challenge material
assumptions and revise using evidence; exchange only useful new findings.
Do not implement. Return the reviewed plan, unresolved decisions and remaining
uncertainty to me. Stop without acknowledgement-only relays.
```

This is a task instruction, not an enforced approval gate. Before real use, each selected CLI must be independently installed, authenticated, and working. Keeping Codex Desktop requires the Native path below, not Embedded attachment to a live Desktop session.

## Important boundaries

**New Embedded Rooms default to YOLO for both participants.** Select narrower native permissions explicitly. Native Rooms retain the original harness's permissions. Responsibilities do not restrict tool access. Embedded's single-Turn rule is not an OS sandbox or a lock against other Rooms, subagents or external writers; Native ownership is advisory.

There is no automatic relay-count or cost limit. Durable recovery distinguishes safe queued work from uncertain delivery; it does not blindly replay execution after a crash. Local storage does not mean cloud-model requests stay on the machine. Read [Security](SECURITY.md), [Concepts](docs/CONCEPTS.md), and [Storage](docs/STORAGE.md).

## Native host mode (experimental)

See [Native setup and usage](docs/NATIVE_RELAY.md) for installation prerequisites, project approval, joining and recovery. The guide is also available inside the browser/desktop app.

Choose **Native** when creating a Room to keep both participants in their original Claude Code/Codex/Grok Build sessions. PairRoom supplies bindings, durable relay and audit, without spawning or interrupting processes. Install and approve the project Stop hooks, then run bind inside each session; it associates immediately from the harness's session-ID environment.

**Highlight — a conversation loop where waiting is free and every message costs one turn.** Both sessions keep their own harness; the loop works like this:

- Two commands to set up: `/pairroom-relay <topic>` in the first session, and the short `bind --room <id> --slot <n>` it prints in the second.
- Your visible reply is the transport: the approved Stop hook publishes the complete addressed reply into the Room FIFO — no retelling, no summary turn, no human copy-paste. Mention handles (`@peer`, `@user`) route it; a reply without a handle ends the relay.
- Reachability across turns: a 30-second park window after each turn collects fast answers; in a wake-enabled Room the Service can nudge an existing Claude Code session through its captured inbox socket, or a Codex peer through `codex queue` (fixed body-free nudge, rate-limited, audited, no automatic retry). Claude inbound policy still applies; socket submission is not model acceptance. Background `relay wait` remains a fallback where the harness surfaces completion. See [Claude inbox setup and limits](docs/design/claude-inbox-wake.md). PairRoom never starts or interrupts agent sessions.
- Waiting lives in the CLI process, not the model: HTTP polls renew internally, so idle time costs zero tokens, and every delivered message costs the receiver exactly one native turn.
- Dated working-session evidence (2026-09-16/17, Windows; Claude Code 2.1.273 + codex-cli 0.154.0, both authenticated): two native sessions ran a full overnight loop without human relaying of message content — delegation, four adversarial design-review rounds, implementation, line-level review, merge — with zero message loss. Working-session evidence; it does not replace the release-gate vendor E2E. Wake surfaces: [verified vendor wake surfaces](docs/NATIVE_RELAY.md#verified-vendor-wake-surfaces).

The `pairroom-relay` skill ships in `skills/` for skill installers (`npx skills add sean2077/pairroom`) and is also written by `relay install`. Once loaded, `/pairroom-relay <topic>` creates the Room and binds that session and reports the peer's join command; `pairroom relay bind` runs zero-flag inside a recognized session. Reuse that binding for follow-up reviews rather than creating a Room per round.

[Native setup](docs/GETTING_STARTED.md#keep-codex-desktop-a-native-room) and [recovery commands](docs/CLI_REFERENCE.md#native-relay-commands) explain bounded park, foreground collection and explicit Retry. Provider/model/effort/permissions remain native-controlled. Authenticated multi-round vendor E2E is still a release gate; synthetic tests are not evidence of model acceptance.

Grok uses [foreground collection](docs/CLI_REFERENCE.md#grok-build-native) to avoid clipped hook feedback; clipped outgoing replies require explicit full-text send/exchange.

## Desktop and source development

Desktop and browser use the same Management Shell and Service. Desktop startup never installs a daemon: it reuses an installed daemon or owns an embedded Service. **Settings → Desktop → Launch at login** changes only native login registration. Closing the window hides it to the tray; quitting does not stop an external daemon. See [Operations](docs/OPERATIONS.md#desktop-lifecycle).

From a source checkout, with the development dependencies installed:

```bash
make dev            # stops an installed daemon and runs the current-tree Service
make docs-check
make check
make smoke
```

For desktop builds and updating an existing local installation, use `make desktop-build`, `make desktop-package`, and `make desktop-update`; see [Desktop development](desktop/README.md). These are source-development commands, not prerequisites for using a release package.

## Documentation and support

[Documentation map](docs/README.md) · [Configuration](docs/CONFIGURATION.md) · [CLI](docs/CLI_REFERENCE.md) · [API](docs/API_REFERENCE.md) · [Troubleshooting](docs/TROUBLESHOOTING.md) · [Upgrading](docs/UPGRADING.md) · [Contributing](CONTRIBUTING.md) · [Support](SUPPORT.md)

PairRoom is evolving. [Changelog](CHANGELOG.md) records release history; current behavior belongs in the reference documents. Mock and browser-fixture tests are not authenticated vendor E2E, and desktop packages are not claimed to be production-signed or notarized. The interface supports English and Simplified Chinese; maintained technical documents are in English.

## License

[MIT](LICENSE) · [Third-party notices](THIRD_PARTY_NOTICES.md)
