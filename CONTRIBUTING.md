# Contributing to PairRoom

## Development setup

```bash
git clone https://github.com/sean2077/pairroom.git
cd pairroom
make check
make smoke
```

`make check` runs format, static checks, unit tests, race / dependency checks, and the documentation contract. `make smoke` runs the full Mock collaboration / recovery scenario.

## Change workflow

1. Create a short-lived branch from latest `main`;
2. Write the behavior invariants and failure boundaries first;
3. Change the minimum necessary code and documentation;
4. Add tests that cover real state transitions;
5. Run verification and list it honestly in the PR;
6. Land through a PR; do not push directly to `main`.

Fixes involving concurrency and recovery should at least cover: the happy path, cancel, process exit, restart, late events, and duplicate callbacks.

## Documentation ownership

[docs/README.md](docs/README.md) defines the unique responsibility of each document. Do not copy the same collaboration semantics into the README, architecture, operations, and troubleshooting guides.

Rules:

- Long-lived Reference records current behavior only; plans and one-off reviews belong in an Issue / PR;
- Do not hard-code the current release in the README;
- CLI flags have `cmd/pairroom/` as the source of truth;
- HTTP routes have `internal/server/` and `internal/service/` as the source of truth;
- JSON fields have `internal/config/` struct tags as the source of truth;
- Event schema has `internal/model/types.go`, apply code, and `internal/store/` as the source of truth;
- Breaking changes update `CHANGELOG.md` and `docs/UPGRADING.md` together.

Before committing:

```bash
make docs-check
```

## Testing claims

Distinguish clearly:

- unit / race tests;
- Mock E2E;
- build and cross-build;
- real Claude Code / Codex / Grok Build native E2E.

Do not describe Mock results as native E2E when the official CLIs were not run.

## Pull request

A PR should include: the problem, design boundaries, user-visible changes, migration impact, verification commands, what was not verified, and how to recover. Prefer splitting a large refactor into independently reviewable commits, but do not keep temporary workflows that exist only to shuffle patches or export source.
