# PairRoom

**English** · [简体中文](README.zh-CN.md)

**Two native coding agents, one task, one local Room.** PairRoom coordinates Claude Code, Codex, and Grok Build without replacing their native coding harnesses. Use one participant to plan and review, and another to implement, verify, and challenge the plan.

<p align="center">
  <img src="docs/images/pairroom-runtime-overview.png" alt="PairRoom collaboration interface">
</p>

## Why use it?

Use PairRoom when coordinating two existing coding sessions has become repetitive work: carrying answers between terminals, tracking which agent owns the task, passing review findings back, and checking what happened after an interruption.

- **Keep your native tools.** Each of the two slots independently selects a supported Runtime, Provider, model, effort, and additional instructions. The same Runtime can occupy both slots. Unspecified overrides inherit native CLI configuration; supported CC Switch Profiles are read-only references. Save your usual pair as an [Agent pair profile](docs/CONFIGURATION.md#agent-pair-profiles), optionally the default for new Rooms.
- **Collaborate on the same change.** Default mode gives Agent 1 the Lead responsibility and Agent 2 the Executor responsibility. Custom mode uses your natural-language rules. These are creation-time instructions, not a rigid phase machine.
- **See and control the relay.** One participant owns a native Turn at a time in each Room. An exact peer mention relays the complete response after that Turn; no peer mention ends relay. Observe tools and approvals, steer or queue input, cancel, interrupt, and inspect durable delivery state.

This is not a claim that two agents are always better or cheaper. A direct CLI is often enough for a small task. Cherry Studio already has executing Agents and documented cross-Session collaboration; native harnesses also support multi-agent work. Choose PairRoom for its particular local pair workflow, not an imaginary absence of those features elsewhere.

**[Why PairRoom](docs/WHY_PAIRROOM.md)** explains fit, costs, examples, and how to evaluate it. **[Alternatives](docs/ALTERNATIVES.md)** compares Cherry Studio, native Claude Code/Codex, Aider, Vibe Kanban, Conductor, and manual relay using dated primary sources.

## Install and try

Download a package from [Releases](https://github.com/sean2077/pairroom/releases/latest). Asset prefixes distinguish `pairroom-cli-…` from `pairroom-desktop-…`. Windows desktop packages end in `-setup.exe`; Linux uses `.deb`/`.AppImage`, and macOS uses `.app.zip`.

For the CLI on Linux, macOS, or Git Bash:

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh | sh
pairroom service --mock
```

Review the installer before executing it, or download the matching CLI asset directly. In Windows PowerShell, a downloaded executable can be started with `./pairroom.exe service --mock`.

**Prebuilt CLI and desktop packages do not require Go.** Start with a disposable Git repository and a fresh Mock Room. Mock does not launch vendor CLIs or consume model quota. Avoid a data-root conflict with an already-running Service; the [Getting started guide](docs/GETTING_STARTED.md) shows an isolated demo entry and the real-Agent transition.

In the Management Shell, register the repository as a Project, create a Room, select the two participants and their permissions, then send a small task to Agent 1. To exercise a useful pair workflow:

```text
Plan the smallest change, delegate implementation and verification to your peer,
then review the actual diff and test evidence. Stop when the result is complete.
Ask me about product decisions you cannot resolve from the repository.
```

This prompt describes an intended sequence, not an enforced approval gate. Before real use, ensure each selected CLI (`claude`, `codex`, and/or `grok`) is independently installed, authenticated, and working in that repository.

## Important boundaries

**New Rooms default to YOLO for both participants.** Both use the live workspace; Lead/Executor responsibilities do not restrict tool access. Select narrower native permissions explicitly. A Room's single-Turn rule is not an OS sandbox or a lock against other Rooms and external writers.

There is no automatic relay-count or cost limit. Durable recovery distinguishes queued work from uncertain or accepted native work; it does not blindly replay execution after a crash. Local storage also does not mean cloud-model requests stay on the machine. Read [Security](SECURITY.md), [Concepts](docs/CONCEPTS.md), and [Storage](docs/STORAGE.md).

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

## Native host mode (experimental)

Choose **Native** when creating a Room to keep both participants in their original Claude Code/Codex sessions. PairRoom supplies bindings, durable relay and audit, without spawning or interrupting processes. Install and approve the project Stop hooks, bind each slot, and return its one-time nonce. [Setup and recovery commands](docs/CLI_REFERENCE.md#native-relay-commands) explain bounded park, foreground collection and explicit Retry. Provider/model/effort/permissions remain native-controlled. Authenticated multi-round vendor E2E is still a release gate; synthetic tests are not evidence of model acceptance.
