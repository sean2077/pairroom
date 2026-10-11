# Documentation map

Start with the question, not every document. **Native** is the default and recommended mode, relaying between user-owned sessions. **Embedded** is an optional mode that owns vendor adapters and native Turn scheduling. A desktop-owned embedded Service can host either Room type. Mode-specific behavior must not be inferred from a generic mention of “Runtime” or “Room”.

Documentation on `main` tracks development, which may be ahead of the package you installed. Check `pairroom version`, read the matching release tag, and trust that binary's `pairroom <command> --help` for flags. The CLI and the running Service can still differ in version; [Native setup](AGENT_SETUP.md#6-native-bind-two-existing-sessions) checks for that.

## Suggested reading paths

| Starting point | Read in this order | Where you end up |
|---|---|---|
| New to PairRoom or the tool names | [Overview](../README.md) → [Core concepts](CONCEPTS.md) → [Why PairRoom](WHY_PAIRROOM.md) | Knowing whether Native or Embedded suits you |
| Connect two existing coding sessions | [Agent-assisted setup](AGENT_SETUP.md) → [Native relay, including Gemini CLI](NATIVE_RELAY.md) | Both sessions bound, and a Stop hook seen running in each |
| Collaborate with a teammate on the LAN | [Native LAN collaboration](LAN_NATIVE.md) → [Security boundary](../SECURITY.md) | One shared Room with an accepted teammate and independent local native permissions |
| Try the interface without a model account | [Installation](INSTALLATION.md) → [Getting started](GETTING_STARTED.md) | A throwaway Mock Room with Embedded explicitly selected (no real model involved) |
| Change versions or investigate stalled work | [Upgrading](UPGRADING.md) or [Troubleshooting](TROUBLESHOOTING.md) → [Operations](OPERATIONS.md) | A specific recovery step that keeps state and does not replay work |

None of these paths requires reading the technical contracts below.

## User guides

| Need | Owner |
|---|---|
| Product overview in English / Chinese | [README](../README.md) / [简体中文](../README.zh-CN.md) |
| Decide whether PairRoom fits | [Why PairRoom](WHY_PAIRROOM.md) and dated [Alternatives](ALTERNATIVES.md) |
| Install, upgrade, or uninstall a package | [Installation](INSTALLATION.md) |
| Let a coding Agent install and check the environment step by step | [Agent-assisted setup](AGENT_SETUP.md) |
| First Mock/Embedded Room and optional plan/implementation review guidance | [Getting started](GETTING_STARTED.md) |
| Native install, create/join, publication, wake, UI, and recovery | [Native relay, including Gemini CLI](NATIVE_RELAY.md) |
| Invite a colleague, accept their receipt, share evidence, or leave | [Native LAN collaboration](LAN_NATIVE.md) |
| Native cwd/worktree identity and locator recovery | [Native session workspace discovery](NATIVE_SESSION_WORKSPACE.md) |
| Tool names, host modes, Bindings, responsibilities, controls, and receipts | [Core concepts](CONCEPTS.md) |
| Agent selections, Provider references, permissions, and saved pairs | [Configuration](CONFIGURATION.md) |
| Service/Desktop lifecycle, capacity, archive, and backup procedure | [Operations](OPERATIONS.md) |
| Diagnose a specific failure | [Troubleshooting](TROUBLESHOOTING.md) |
| Supported formats and version-change recovery | [Upgrading](UPGRADING.md) |

## Technical contracts

| Surface | Written owner | Implementation authority |
|---|---|---|
| Commands and flags | [CLI reference](CLI_REFERENCE.md) | CLI source and the matching binary's `--help` |
| HTTP/SSE and request semantics | [API reference](API_REFERENCE.md) | Production Service/Room registrations and handlers |
| Model-facing routing/envelopes and Native delivery | [Protocol](PROTOCOL.md) | `internal/protocol/`, prompts, relay state machine, contract tests |
| Process, state, identity, and component ownership | [Architecture](ARCHITECTURE.md) | Service, Embedded Room, Native relay, and adapter code |
| Schemas, persistence, and replay | [Storage](STORAGE.md) | Model/Store/apply code and integrity tests |
| Configuration fields | [Configuration](CONFIGURATION.md) | Strict parser and model structs |

CLI/API/config inventories are source-derived gap checks, not substitutes for field semantics. Do not copy their detailed limits into every quickstart. Concepts explains what behavior means; Protocol/Storage specify exact transitions; Native relay provides the operator workflow.

## Contributor and policy entry points

| Need | Document |
|---|---|
| Repository Agent contract and harness sources | [AGENTS.md](../AGENTS.md) |
| Canonical English/Chinese terminology | [CONTEXT.md](../CONTEXT.md) |
| Development commands, test layers/design, commit and PR evidence, release verification | [Contributing](../CONTRIBUTING.md) |
| Separate desktop module and local installation update | [Desktop development](../desktop/README.md) and [nested contract](../desktop/AGENTS.md) |
| Threat model and private-data boundaries | [Security](../SECURITY.md) |
| Compatibility policy and safe support requests | [Support](../SUPPORT.md) |
| Release history and its provenance | [Changelog](../CHANGELOG.md) and [History provenance](../HISTORY_PROVENANCE.md) |
| Third-party licensing | [Third-party notices](../THIRD_PARTY_NOTICES.md) |

## Design and validation history

[Design records](design/README.md) distinguish implemented rationale from superseded plans. They are not a second setup manual. [Validation records](validation/README.md) are dated evidence tied to their recorded environment; neither old measurements nor synthetic tests certify the current vendor combination.

Keep published historical evidence intact. When a completed plan is retired, preserve the decision and a pinned historical source rather than leaving future-tense instructions active. Review external comparisons against their stated date/revision, not as an undated capability guarantee.

## Maintenance

Run `make docs-check` after edits. It checks local Markdown paths, selected inventories, and the current storage format window against source declarations; it does not check all heading fragments, external sources, example execution, or factual accuracy. Check changed anchors and user commands separately, and distinguish actual runs from source inspection in PR evidence.

Link an external product to its official documentation or repository where a page first discusses it, and say what role it plays: supported Runtime, optional integration, example terminal, or comparison project. Fixing a link or rewording a comparison does not refresh its review date.

Maintain current technical pages in English and equivalent root English/Chinese overviews. Preserve stable entry paths/anchors where practical. Add a page only for a distinct owner; prefer links over repeated protocol, setup, or release text. See [documentation contribution rules](../CONTRIBUTING.md#documentation-changes).
