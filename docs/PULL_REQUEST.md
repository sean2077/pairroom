# PairRoom 1.0.0: native Claude Code × Codex collaboration room

## Summary

This PR promotes PairRoom from the reconstructed v0.3 baseline to the first stable 1.0 release through six reviewable milestones:

- v0.4: independent Reviewer Git snapshot and safe role/workspace switching;
- v0.5: explicit append/next-turn/supersede/cancel semantics;
- v0.6: durable structured Work Inspector;
- v0.7: strict verification, backup, restore, and redacted diagnostics;
- v0.8: long-room pagination, drafts, unread state, notifications, and richer image viewing;
- v0.9: HttpOnly browser sessions, CSRF, query-token removal, and API limiting;
- v1.0: stable contract, CI, release automation, SBOM, provenance, operations, privacy, and acceptance docs.

## Design boundary

PairRoom launches and coordinates the official `claude` and `codex` Harnesses. It does not implement a replacement Agent loop, model gateway, terminal parser, or credential broker.

## Validation

- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go vet ./...`
- JavaScript syntax checks
- Mock end-to-end chat/image/isolation/Turn/pagination flow
- data verification + backup + destructive restore validation
- four-platform static cross-build
- clean-package `git fsck`, test, bundle clone, SHA-256, SBOM, and provenance validation

The build environment did not have authenticated official Claude Code/Codex accounts, so the PR does not represent Mock runs as real vendor-network E2E. `pairroom doctor` and a non-critical real-repository smoke test remain required on the maintainer machine before public release.

## Security

- Reviewer snapshot fails closed rather than silently using the live writable tree.
- Unknown high-privilege vendor requests fail closed.
- Browser bootstrap Token is exchanged from a URL fragment for an HttpOnly SameSite=Strict session.
- Browser mutations require CSRF; query tokens authorize no endpoint.
- Attachments are type/size/dimension/hash/path checked and remote Markdown images are not auto-fetched.
- PairRoom still has no built-in TLS and must not be exposed directly to the public internet.

## Git history note

The v0.1-v0.3 commits were reconstructed from complete release snapshots after the original temporary `.git` directory was unavailable. v0.4 onward is the retained development history used by this PR. See `HISTORY_PROVENANCE.md`.
