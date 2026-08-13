# PairRoom Release Acceptance Checklist

A build is publishable only when every applicable item is complete.

## Source and history

- [ ] `VERSION`, `internal/version.Current`, changelog, release notes, and Git tag agree.
- [ ] Working tree is clean.
- [ ] `git fsck --full` succeeds.
- [ ] No secrets, browser sessions, room data, private fixtures, or temporary Agent files are tracked.
- [ ] `go list -m all` contains only the PairRoom module.

## Verification

- [ ] `make check` succeeds.
- [ ] `make smoke` succeeds.
- [ ] Mock E2E covers chat, images, Reviewer snapshot, Turn summaries, pagination, verify, backup, restore, and diagnostics.
- [ ] Corrupt/traversal/archive rejection tests succeed.
- [ ] Browser session, CSRF, query-token rejection, and rate limiter tests succeed.
- [ ] Current real Claude Code and Codex smoke test is performed on the release machine or explicitly recorded as not available.

## Build artifacts

- [ ] Linux amd64, Windows amd64, macOS arm64, and macOS amd64 binaries build with `CGO_ENABLED=0` and `-trimpath`.
- [ ] Linux binary reports the expected version, commit, and build date.
- [ ] Source ZIP and TAR.GZ are generated from the tagged Git object.
- [ ] SHA-256 file, SPDX SBOM, and release provenance are generated and internally consistent.
- [ ] A clean extraction of the full Git package passes `git fsck`, `git status`, and `go test ./...`.
- [ ] The standalone Git bundle clones successfully.

## Documentation and GitHub

- [ ] README, security, privacy, operations, support, upgrade, and validation documents match actual behavior.
- [ ] PR description calls out native-runtime validation boundaries.
- [ ] `main` base and `release/v1.0.0` head are pushed.
- [ ] PR checks pass before merge.
- [ ] Tag and GitHub Release are created from the merged/reviewed source commit.
