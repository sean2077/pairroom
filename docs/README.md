# Documentation map

Start with the question you need answered. Each document has one primary responsibility; follow its links rather than copying its contract into every guide.

## Choose, start, and operate

| Question | Document | Owns |
|---|---|---|
| Is this useful for my workflow? | [Why PairRoom](WHY_PAIRROOM.md) | Value, use cases, costs, limits, evaluation method |
| Why not a similar tool? | [Alternatives](ALTERNATIVES.md) | Dated primary-source comparison and selection tradeoffs |
| How do I complete a first task? | [Getting started](GETTING_STARTED.md) | Prebuilt/source entry points, Mock, first real Room |
| What does the Room do? | [Concepts](CONCEPTS.md) | User-facing Room, Binding, Turn, relay, permission, and recovery semantics |
| What can I configure? | [Configuration](CONFIGURATION.md) | Precedence, immutable Agent selection, Providers, supported native policy |
| How do I run and maintain it? | [Operations](OPERATIONS.md) | Desktop/daemon ownership, capacity, archive/delete, backup, shutdown |
| What should I check when it fails? | [Troubleshooting](TROUBLESHOOTING.md) | Symptom-to-action guidance, not a second specification |
| What changes on upgrade? | [Upgrading](UPGRADING.md) | Compatibility boundaries, migration actions, rollback |

## Integrate and develop

| Document | Owns |
|---|---|
| [CLI reference](CLI_REFERENCE.md) | Command responsibilities and source-derived flag inventory |
| [API reference](API_REFERENCE.md) | HTTP/SSE contracts, receipts, errors, source-derived routes |
| [Protocol](PROTOCOL.md) | Model-facing bootstrap, envelope, exact-handle relay, convergence |
| [Architecture](ARCHITECTURE.md) | Components, state ownership, lifecycle and implementation invariants |
| [Storage](STORAGE.md) | Durable/ephemeral state, schema, replay, recovery and archive boundaries |
| [Contributing](../CONTRIBUTING.md) | Development setup, verification layers, PR and documentation workflow |
| [Desktop development](../desktop/README.md) | Native build dependencies, packaging, source-based local update |

## Repository-wide documents

The documentation set includes more than this directory.

| Source | Responsibility and authority |
|---|---|
| [English README](../README.md) / [Chinese README](../README.zh-CN.md) | Equivalent product entry points and quick start; only these product overviews are translated |
| [Security](../SECURITY.md) | Threat model, authentication, permissions, privacy/data path, vulnerability reporting |
| [Support](../SUPPORT.md) | Support and compatibility scope, safe issue-reporting evidence |
| [Project language](../CONTEXT.md) | Canonical terms and English/Chinese equivalents |
| [Root Agent contract](../AGENTS.md) / [Desktop Agent contract](../desktop/AGENTS.md) | Contributor instructions for their respective scopes; `CLAUDE.md` is a projection, not a separate authority |
| [Skill boundary](../.agents/skills/README.md) / [Subagent boundary](../.agents/subagents/README.md) | Project-authored skill/subagent sources and generated projection ownership |
| [License](../LICENSE) / [Third-party notices](../THIRD_PARTY_NOTICES.md) | Legal terms and retained third-party notices, not product guidance |
| [Changelog](../CHANGELOG.md) | Historical release facts; older entries do not define current behavior |
| [History provenance](../HISTORY_PROVENANCE.md) | Reconstruction history, not current build or compatibility evidence |
| [Historical validation records](validation/README.md) | Scope and limitations of retained old JSON reports; not proof that today's source passes |

## Reading authority and status

For runtime facts, inspect the implementation, tests, and current reference for the relevant version. The CLI's `--help` owns exact flag defaults; source registrations own routes; configuration structs/parser own fields; the Store and replay code own schemas. Examples illustrate these contracts rather than overriding them.

The Why and Alternatives pages are explanations and analysis. Their `reviewed` metadata identifies the source-review date, not a successful runtime test. A comparison of a repository's main branch is not certification of a released package. A document marked `status: historical` is retained evidence, not an active specification. Proposals and one-off audit reports belong in Issues/PRs until accepted and implemented.

An older plan, transcript, validation JSON, or release note must not reintroduce removed workflow stages, role aliases, Provider fields, or security behavior. When documents disagree, correct the owning current reference against source first, then update summaries and links. Do not silently convert a future proposal into an implemented capability.

## Maintenance checks

Run `make docs-check` after documentation changes. It checks repository Markdown local links and images, including root support/security pages, nested guides, and newly added non-ignored files. It also preserves the curated top-level guide inventory and the source-derived flag/route/configuration inventories. Source archives without Git metadata are supported.

The checker handles common inline links, reference destinations, and HTML image/link targets; it ignores fenced examples and comments. It does not make network requests, certify external claims, validate every Markdown extension or heading fragment, or execute example commands. Review those manually, including both README languages. Do not weaken a check merely to retain a stale reference.

When changing a contract, update its owner and the smallest relevant summaries. Keep technical documentation in English, the two root READMEs equivalent, terminology aligned with `CONTEXT.md`, and historical records intact. Add a new page only for a distinct reader need; do not resurrect deleted one-off plans or duplicate privacy, compatibility, or troubleshooting pages.
