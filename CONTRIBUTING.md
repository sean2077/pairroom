# Contributing to PairRoom

PairRoom has a deliberately narrow boundary: coordinate official Claude Code and Codex without replacing either Harness. Changes that add a model proxy, generic Agent loop, terminal-output parser, hosted credential service, or hidden cloud dependency are unlikely to be accepted.

## Development requirements

- Go 1.23+
- Git
- Node.js for JavaScript syntax checks
- Bash, curl, Python 3, and standard archive tools for the release smoke test
- official Claude Code/Codex only for optional native smoke testing

The Go module and browser runtime intentionally have no third-party dependencies.

## Local workflow

```bash
git switch -c feature/short-name
make check
make smoke
```

Useful commands:

```bash
make cover
go run ./cmd/pairroom serve --repo . --mock
```

A change to room events, persistence, message lifecycle, role/workspace policy, approval handling, authentication, or archives must include focused tests and a migration/recovery explanation.

## Pull requests

A PR should state:

1. the user-visible behavior;
2. the durable state or protocol changes;
3. failure and rollback behavior;
4. security/privacy impact;
5. tests run;
6. whether current real Claude Code and Codex were exercised.

Do not claim native-runtime E2E when only Mock or fixture adapters were used.

## Release work

`docs/RELEASE_CHECKLIST.md` is the release gate. A tagged release is generated with:

```bash
make release
```

This runs unit/race/vet/static checks, the complete Mock collaboration/recovery smoke test, four-platform builds, source archives, SBOM/provenance generation, and artifact verification.

## Git history

The retained v0.1-v0.3 commits were reconstructed from release snapshots; v0.4 onward is native retained history. See `HISTORY_PROVENANCE.md` before rebasing milestone commits.
