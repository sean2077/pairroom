# Changelog

## 0.2.0 — 2026-08-13

Reliability and observability release focused on real Claude Code/Codex operation.

### Added

- Separate per-target delivery and processing lifecycles (`waiting`, `working`, `completed`, `cancelled`, `failed`)
- Auditable per-target retry messages with `retry_of`
- Runtime capability/version probing and machine-readable `doctor --json`
- Claude CLI optional-flag negotiation and graceful capability degradation
- Runtime information, warnings and stall indicators in participant cards
- Configurable no-runtime-event warnings
- Message search, dark/light theme, Markdown/JSON transcript export
- Message-to-Turn Inspector filtering
- Event-store schema versioning and forward-version rejection
- Host-header protection for tokenless loopback deployments
- API and tests for export/retry, restart recovery and processing lifecycle
- Regression coverage ensuring each Claude stream-json submission is written and queued exactly once

### Changed

- Runtime execution failures no longer overwrite an already accepted transport delivery result
- Adapter lookup/submit failures now settle both delivery and processing, avoiding orphaned waiting states
- Restart/stop now settles orphaned processing and expires connection-local approvals
- Codex request construction uses documented App Server fields, including `clientUserMessageId` correlation
- Codex plan projection supports current `item/plan/delta` and whole-plan compatibility events
- JSON transcript exports omit verbose Inspector events by default
- UI automatically resynchronizes when it detects an SSE sequence gap

### Fixed

- Partial JSONL tails are truncated before subsequent appends
- Multiple inputs steered into one Codex Turn are all settled on completion
- Fast runtime events can no longer be overwritten by a late Submit result
- Incomplete `claude --help` output no longer causes a false stream-json incompatibility rejection
- Stale pending/working state no longer survives a PairRoom restart
- Case-colliding duplicate documentation filenames were removed for Windows/macOS archives

## 0.1.0 — 2026-08-13

Initial standalone MVP.

### Added

- Native Claude Code stream-json adapter
- Native Codex app-server adapter with active-turn steering
- Shared three-party room and IM-style Web UI
- Manual, mention and bounded roundtable routing
- Driver, Reviewer and Peer roles
- Runtime Inspector, Git status/diff and Codex approvals
- Append-only JSONL persistence and session recovery
- Mock mode, doctor command and configuration file support
- Protocol, architecture, security and product-plan documentation
