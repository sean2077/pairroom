# Support

PairRoom 1.0 is a local-first open-source project. Support is provided on a best-effort basis through the GitHub repository.

## Before opening an issue

Run:

```bash
pairroom version --json
pairroom doctor --repo /path/to/repository --json
pairroom verify --data-dir /path/to/room-data --json
pairroom diagnostics --data-dir /path/to/room-data --output pairroom-diagnostics.tar.gz
```

Read `SECURITY.md` before sharing diagnostics. The diagnostics command is designed to omit transcript text and attachment bytes, but you should still inspect the archive for environment-specific paths or metadata.

## Bug reports

Include:

- operating system and architecture;
- PairRoom build metadata;
- current official Claude Code and Codex versions;
- the exact action and visible delivery/processing state;
- whether the problem reproduces in `--mock` mode;
- a minimal repository when possible.

Do not post API tokens, cookies, private prompts, source code, screenshots, event logs, or approval payloads from a confidential project.

## Compatibility policy

PairRoom follows current stable public interfaces of Claude Code and Codex. It does not maintain a permanent compatibility matrix for obsolete vendor CLI versions. Run `pairroom doctor` after updating either CLI.

## Scope of 1.0 support

The stable product boundary is one local daemon, one Git repository, one human, one Claude Code participant, and one Codex participant. Multi-user hosting, cloud synchronization, team RBAC, and additional Agent vendors are outside the 1.0 support contract.
