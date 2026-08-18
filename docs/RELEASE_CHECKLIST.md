# PairRoom Release Acceptance Checklist

A build is publishable only when every applicable item is complete.

## Source and history

- [ ] `VERSION`, `internal/version.Current`, the exact `vX.Y.Z` Git tag, and one non-empty canonical `CHANGELOG.md` heading agree.
- [ ] Working tree is clean.
- [ ] `git fsck --full` succeeds.
- [ ] No secrets, browser sessions, room data, private fixtures, or temporary Agent files are tracked.
- [ ] `go list -m all` contains only the PairRoom module.

## Verification

- [ ] `make check` succeeds.
- [ ] The release-contract tests and real current-version changelog extraction succeed.
- [ ] `make smoke` succeeds.
- [ ] Mock E2E covers chat, images, Reviewer snapshot, Turn summaries, pagination, verify, backup, restore, and diagnostics.
- [ ] Corrupt/traversal/archive rejection tests succeed.
- [ ] Browser session, CSRF, query-token rejection, and rate limiter tests succeed.
- [ ] Empty-Project unregister persists across Registry restart and leaves the Git worktree byte-for-byte untouched.
- [ ] Active and archived Rooms both block Project unregister with a structured `project_has_rooms` conflict.
- [ ] Project unregister racing Room provisioning has one atomic winner and never creates an orphaned Room.
- [ ] Project path refresh persists unavailable diagnostics and recovers after the canonical path returns.
- [ ] Batch Room archive validates before mutation, de-duplicates in first-seen order, treats already-archived items idempotently, and lets busy/missing failures coexist with successful later items without interrupting active Turns.
- [ ] Active Room permanent removal is rejected with `room_not_archived`; archived removal requires explicit irreversible acknowledgement, releases binding ownership, and unblocks final Project unregister without typed Room-ID confirmation.
- [ ] Batch Room removal validates the full request before mutation, rejects empty/oversized input, de-duplicates in first-seen order, supports 1–100 submitted IDs, and returns explicit per-Room success/failure results.
- [ ] A busy/invalid Room in a valid batch does not roll back successfully deleted Rooms; failed archived Rooms remain selectable for a later retry.
- [ ] Managed Room removal deletes Event Log/attachments without touching the Git worktree or Vendor Session/Thread; explicitly imported external Room directories remain byte-for-byte present.
- [ ] Busy, starting/stopping-uncertain, and failed-retained Runtimes prevent Room removal without interrupting work or changing durable data.
- [ ] Room deletion checkpoint failure restores Room data/indexes; crash recovery restores prepared deletion when the checkpoint owns it and completes deletion when the checkpoint omits it.
- [ ] Unknown/symlink/non-directory quarantine entries fail closed and are preserved; fail-closed cleanup retry cannot erase an uncommitted prepared Room.
- [ ] `cleanup_pending` is surfaced in Service summary/maintenance, and the maintenance retry endpoint clears committed quarantine without resurrecting Rooms.
- [ ] Management Shell rejects any typed Project-ID mismatch; Room lifecycle uses one selection for batch archive and cleanup; permanent cleanup uses an explicit irreversible acknowledgement checkbox, and remains free of native prompt/confirm or Web Storage credentials.
- [ ] Current real Claude Code and Codex smoke test is performed on the release machine or explicitly recorded as not available.

## Build artifacts

- [ ] Linux amd64, Windows amd64, macOS arm64, and macOS amd64 binaries build with `CGO_ENABLED=0` and `-trimpath`.
- [ ] Normal CI uploads one checksummed workflow artifact per supported target, then re-downloads and verifies the exact aggregate set before succeeding.
- [ ] Linux binary reports the expected version, commit, and build date.
- [ ] Source ZIP and TAR.GZ are generated from the tagged Git object.
- [ ] SHA-256 file, SPDX SBOM, and release provenance are generated and internally consistent.
- [ ] A clean extraction of the full Git package passes `git fsck`, `git status`, and `go test ./...`.
- [ ] The standalone Git bundle clones successfully.

## Documentation and GitHub

- [ ] README, security, privacy, operations, support, upgrade, and validation documents match actual behavior.
- [ ] PR description calls out native-runtime validation boundaries.
- [ ] The reviewed release commit is pushed and reachable from `main` before the tag is pushed.
- [ ] PR or protected-branch checks pass before the annotated tag is created.
- [ ] The tag workflow creates a non-draft, non-prerelease GitHub Release from that exact commit.
- [ ] The workflow downloads every published asset and validates asset-name parity plus `SHA256SUMS`.
