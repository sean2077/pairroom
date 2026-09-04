# Documentation map

This directory keeps only documentation that still needs maintenance. Historical designs, one-off reviews, release snapshots, and old screenshots stay in Git history and are not copied forward in the current tree.

## Read by task

| Goal | Document | Unique responsibility |
|---|---|---|
| First run | [GETTING_STARTED](GETTING_STARTED.md) | From install or desktop launch through the first Room |
| Understand behavior | [CONCEPTS](CONCEPTS.md) | Project, Room, Turn, FIFO, roles, Workflow, and approval semantics |
| Change configuration | [CONFIGURATION](CONFIGURATION.md) | JSON configuration, Providers, runtime policy, and safety boundaries |
| Look up commands | [CLI_REFERENCE](CLI_REFERENCE.md) | Command entry points, how to discover flags, and the auto-checked inventory |
| Call HTTP | [API_REFERENCE](API_REFERENCE.md) | Management / Room HTTP and SSE contract |
| Change the implementation | [ARCHITECTURE](ARCHITECTURE.md) | Components, state ownership, invariants, and code navigation |
| Understand persistence | [STORAGE](STORAGE.md) | Event Log, Binding, attachments, backup, and restart recovery |
| Deploy and maintain | [OPERATIONS](OPERATIONS.md) | Desktop, Service, Daemon, archive, delete, diagnostics, and recovery |
| Diagnose problems | [TROUBLESHOOTING](TROUBLESHOOTING.md) | Symptom-oriented common failures |
| Upgrade across versions | [UPGRADING](UPGRADING.md) | Breaking changes, backup, verification, and rollback |
| Change the Agent contract | [PROTOCOL](PROTOCOL.md) | Input envelopes, control markers, handoff, and convergence rules |

The top-level [README](../README.md) owns product positioning and the shortest path to a first run. [CONTRIBUTING](../CONTRIBUTING.md) owns the development process. [CHANGELOG](../CHANGELOG.md) owns version history. Desktop toolchain and packaging commands live in [desktop/README](../desktop/README.md).

## Content boundaries

Each fact should have one detailed explanation:

- Collaboration semantics belong in `CONCEPTS.md`;
- Code structure, desktop-host boundaries, and concurrency invariants belong in `ARCHITECTURE.md`;
- Exact command and interface names belong in Reference documents;
- Failure handling belongs in `TROUBLESHOOTING.md`;
- Version migration belongs in `UPGRADING.md`.

Other documents link to that explanation instead of copying it. Short-lived plans that tests or source cannot verify belong in an Issue / PR, not in long-lived Reference.

## Maintenance rules

When the following code changes, update the matching documents:

| Code area | Documents |
|---|---|
| `desktop/` | `GETTING_STARTED.md`, `ARCHITECTURE.md`, `OPERATIONS.md`, and `desktop/README.md` |
| `internal/room/`, `internal/agent/` | `CONCEPTS.md`, `ARCHITECTURE.md`, `PROTOCOL.md` |
| `internal/config/`, Provider parsing | `CONFIGURATION.md` |
| `cmd/pairroom/` | `CLI_REFERENCE.md` |
| `internal/server/`, `internal/service/` HTTP handlers | `API_REFERENCE.md` |
| `internal/store/`, archive / backup | `STORAGE.md`, `OPERATIONS.md`, `UPGRADING.md` |

Before committing, run:

```bash
make docs-check
```
