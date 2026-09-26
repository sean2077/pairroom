# Documentation map

Start with the question, not every document. **Embedded** owns vendor adapters and native Turn scheduling; **Native** relays between user-owned sessions. A desktop-owned embedded Service can host either Room type. Mode-specific behavior must not be inferred from a generic mention of “Runtime” or “Room”.

## User guides

| Need | Owner |
|---|---|
| Product overview in English / Chinese | [README](../README.md) / [简体中文](../README.zh-CN.md) |
| Decide whether PairRoom fits | [Why PairRoom](WHY_PAIRROOM.md) and dated [Alternatives](ALTERNATIVES.md) |
| Install, upgrade, or uninstall a package | [Installation](INSTALLATION.md) |
| Let a coding Agent install and check the environment step by step | [Agent-assisted setup](AGENT_SETUP.md) |
| First Mock/Embedded Room and review-first task recipes | [Getting started](GETTING_STARTED.md) |
| Native install, create/join, publication, wake, UI, and recovery | [Native relay](NATIVE_RELAY.md) |
| Native cwd/worktree identity and locator recovery | [Native session workspace discovery](NATIVE_SESSION_WORKSPACE.md) |
| Host modes, Bindings, responsibilities, controls, and receipts | [Core concepts](CONCEPTS.md) |
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

Run `make docs-check` after edits. It checks local Markdown paths and selected inventories, not all heading fragments, external sources, example execution, or factual accuracy. Check changed anchors and user commands separately, and distinguish actual runs from source inspection in PR evidence.

Maintain current technical pages in English and equivalent root English/Chinese overviews. Preserve stable entry paths/anchors where practical. Add a page only for a distinct owner; prefer links over repeated protocol, setup, or release text. See [documentation contribution rules](../CONTRIBUTING.md#documentation-changes).
