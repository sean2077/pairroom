# PairRoom 1.0 Validation Record

Validation date: **2026-08-14**

This document distinguishes PairRoom-controlled verification from vendor-network verification. The release pipeline can fully exercise PairRoom's room, storage, browser API, Mock adapters, workspace isolation, media, archive, and packaging behavior. It cannot honestly claim a real Claude Code/Codex model run unless both official CLIs are installed and authenticated in the build environment.

## 1. Source checks

The following release gate succeeds on the retained `release/v1.0.0` branch:

```bash
make check
```

It runs:

```text
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
gofmt cleanliness
node --check internal/server/assets/app.js
node --check internal/server/assets/richtext.js
go list -m all dependency assertion
git diff --check
```

The source contains **111 top-level Go test functions**. Race-enabled tests pass across all testable packages.

Coverage snapshot:

| Package | Statement coverage |
|---|---:|
| `internal/agent` | 46.5% |
| `internal/archive` | 69.5% |
| `internal/attachment` | 62.1% |
| `internal/config` | 80.0% |
| `internal/prompt` | 75.0% |
| `internal/room` | 71.0% |
| `internal/server` | 65.2% |
| `internal/store` | 69.2% |
| `internal/version` | 100.0% |
| `internal/workspace` | 68.9% |

Coverage is used as a diagnostic rather than a release percentage gate. The most important state transitions, archive rejection paths, workspace failure paths, browser-session boundary, and adapter correlation logic have focused tests.

## 2. Full Mock collaboration and recovery smoke test

```bash
make smoke
```

The smoke test builds PairRoom, creates a temporary Git repository, starts the daemon with both Mock Harnesses, then exercises:

1. human message delivered to Claude and Codex;
2. independent Driver/Reviewer workspace assignment;
3. durable Turn summaries;
4. raster-image upload and attachment transcript;
5. bounded snapshot plus cursor message API;
6. graceful daemon shutdown;
7. strict room verification;
8. self-verifying backup creation;
9. restore into a fresh directory;
10. strict verification of restored data;
11. redacted diagnostics generation.

Observed machine-readable result:

```json
{
  "pairroom_version": "1.0.0",
  "message_count": 5,
  "turn_count": 3,
  "latest_seq": 88,
  "attachment_messages": 1,
  "verification_ok": true,
  "restored_verification_ok": true,
  "event_count": 96,
  "attachment_count": 1
}
```

The complete result is in [`validation/v1.0.0-mock-e2e.json`](validation/v1.0.0-mock-e2e.json). The diagnostics archive hash varies because the archive records current timestamps.

## 3. Reviewer workspace verification

Tests create temporary Git repositories containing:

- committed HEAD;
- dirty tracked text and binary changes;
- untracked regular files;
- unsafe symlink cases;
- repeated refresh/cleanup and role-switch rollback.

The Reviewer snapshot must reproduce the source state without changing the Driver tree. Unsafe symlinks, invalid patch application, unavailable HEAD, or unsafe role transition fail explicitly. POSIX read-only enforcement and Windows advisory boundary metadata are separately represented.

## 4. Message and Turn lifecycle verification

Focused tests cover:

- pending/started/injected/queued/failed/skipped delivery;
- waiting/working/completed/cancelled/failed/superseded processing;
- append, next-turn, supersede, per-target cancel, and retry;
- stale human-message precedence and hop limits;
- Codex current-Turn steer versus next-Turn queue;
- Claude queue/cancel and process-exit settlement;
- multiple inputs correlated to one vendor Turn;
- restart settlement of orphaned processing and approvals;
- durable bounded Turn/Tool/Command/Plan/Diff/Usage summaries.

## 5. Browser/API security verification

Server and browser-asset tests cover:

- URL-fragment bootstrap instead of query Token;
- HttpOnly, SameSite=Strict browser-session cookie;
- session sliding expiry and revocation;
- CSRF rejection/acceptance for browser mutations;
- authenticated SSE with the browser cookie;
- query Token rejection for all endpoints;
- Bearer API compatibility;
- same-origin and Host/DNS-rebinding checks;
- fixed-window API rate limiting;
- CSP, `nosniff`, frame, referrer, and permissions headers;
- absence of Token persistence in `sessionStorage` or `localStorage`;
- JavaScript syntax and embedded asset serving.

The retained v0.3 browser E2E screenshots continue to document the rich conversation and lightbox UI. The 1.0 release environment had a managed system Chromium policy that blocked local navigation, so no new Chromium screenshot is represented as having passed. This does not affect the Go/HTTP/session tests above, but a maintainer browser smoke remains part of the public-release checklist.

## 6. Attachment verification

Tests and smoke flow verify:

- PNG/JPEG/GIF/WebP signature and decode checks;
- per-image, per-message, dimension, and pixel limits;
- opaque attachment IDs and absence of host paths in transcript JSON;
- immutable SHA-256 validation at browser and Harness boundaries;
- authenticated media reads, ETag, CSP, and `nosniff`;
- symlink/repository escape rejection for Agent-generated images;
- remote Markdown images not auto-fetched;
- durable attachment references surviving backup and restore.

## 7. Archive and corruption verification

Archive tests cover:

- event sequence, metadata schema, room identity, attachment manifest, size, and SHA-256 verification;
- partial final JSONL line recovery;
- mid-log corruption rejection;
- archive traversal, absolute path, links, duplicate paths, undeclared files, oversized entries, and hash mismatch rejection;
- atomic forced restore preserving the prior destination until validation succeeds;
- diagnostics redaction of message text and attachment bytes.

## 8. Native runtime boundary

The release environment reports:

```text
Git: available
Claude Code CLI: not installed
Codex CLI: not installed
```

See [`validation/v1.0.0-doctor.json`](validation/v1.0.0-doctor.json).

Therefore this validation does **not** claim:

- a real authenticated Claude Code long Turn;
- a real Codex `turn/steer` network/model round trip;
- current vendor approval prompts against a live account;
- real Skills/MCP/Hooks and long-context compaction/resume;
- Windows vendor sandbox behavior on a physical Windows host.

The adapter request/response shapes, control state machines, image input encoding, approvals, role policies, process exits, and correlation are covered by fixtures and unit tests. Before using an important repository, run `pairroom doctor` and one full real session on the target machine.

On a machine where both official CLIs are installed and authenticated, the repeatable native acceptance test creates deferred bindings, executes one real Turn through each harness, rebuilds the Registry from disk, and executes a second Turn through the exact same Claude Session and Codex Thread:

```powershell
$env:PAIRROOM_NATIVE_E2E='1'
go test ./internal/service -run TestNativeSessionMaterializationAndExactResume -count=1 -v
```

## 9. Release artifact gate

`scripts/release.sh` performs four static cross-builds and generates source ZIP/TAR.GZ, SHA-256, SPDX 2.3 SBOM, build provenance, and version evidence. `scripts/verify-artifacts.sh` then checks:

- required artifact set;
- all checksums;
- ELF/PE/Mach-O architecture signatures;
- source archive integrity;
- embedded version/commit/build date;
- SBOM/provenance consistency;
- checksum coverage of every release file.

The exact commit and artifact hashes are written into the generated full-package validation report rather than hard-coded in this source document.
