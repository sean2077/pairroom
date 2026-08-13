## [0.7.0] - 2026-08-14

### Added

- `pairroom verify` for strict event-sequence, metadata, room-ID, attachment-manifest, size, and SHA-256 validation.
- Self-describing `pairroom backup` archives with an internal manifest and full post-write restore validation.
- Strict `pairroom restore` with traversal, link, duplicate, undeclared-file, size, and hash rejection plus atomic target replacement.
- Redacted `pairroom diagnostics` bundles containing structural event headers and integrity results without transcript text or image bytes.

### Changed

- Runtime caches, reviewer worktrees, browser sessions, and temporary files are intentionally excluded from backups.
- Backup replacement and forced restore preserve the previous target until the new artifact has been fully committed.

## [0.6.0] - 2026-08-14

### Added

- Durable vendor-neutral Turn summaries for tools, commands, plans, diffs, usage, final responses, failures, and duration.
- Compact Work Inspector cards that survive daemon restart while retaining recent native events for diagnostics.
- Message-to-Turn correlation in the Inspector, with bounded item and output retention for long sessions.

### Changed

- High-frequency command output updates the live summary without forcing a disk sync for every chunk; terminal Turn events persist the bounded projection.
- Incomplete work items are settled when their native Turn completes, avoiding permanently working Inspector rows.

## [0.5.0] - 2026-08-14

### Added

- Explicit message intents: append to the active discussion, queue for the next turn, or supersede in-flight instructions.
- Per-target cancellation endpoint and UI controls with honest whole-turn/queue cancellation semantics.
- Durable `supersedes` references and visible intent markers in the shared timeline.
- Codex next-turn intent that avoids `turn/steer` and waits for a safe turn boundary.

### Changed

- Superseding a target interrupts its native runtime, marks every affected in-flight message, and prevents stale automatic handoff.
- Retries preserve the original message intent while remaining separate auditable messages.

## [0.4.0] - 2026-08-14

### Added

- Reviewer Git snapshot that includes committed HEAD, dirty tracked changes, and untracked regular files.
- Durable workspace-boundary metadata with source HEAD, snapshot digest, dirty state, and read-only enforcement status.
- Safe workspace reconfiguration for Claude Code, Codex, and mock adapters.
- Atomic two-participant driver/reviewer switch event with rollback on adapter reconfiguration failure.
- Reviewer snapshot tests covering dirty files, untracked files, replacement, and symlink rejection.

### Changed

- Reviewer sessions now run from an isolated snapshot by default instead of the live driver working tree.
- POSIX snapshots are made filesystem read-only; Windows reports the weaker boundary explicitly and continues to rely on native reviewer sandboxing.

# Changelog

## 0.3.0 — 2026-08-13

Rich conversation, native approvals and reviewer-policy release.

### Added

- Safe DOM-based Markdown rendering for headings, lists, task lists, quotes, tables, inline formatting, links and fenced code with copy controls
- Durable PNG/JPEG/GIF/WebP message attachments with paste, drag/drop, picker upload and image-only messages
- Native multimodal delivery: Claude image content blocks and Codex App Server `localImage` inputs
- Message-scoped image galleries, full-screen lightbox, navigation, zoom and original-image access
- Safe discovery and import of repository-local images referenced by Agent final responses
- Thread-focus view in addition to quote reply and message-to-Inspector correlation
- Claude Code native control-protocol initialization and unified tool/`AskUserQuestion` approvals
- Structured Claude single-select, multi-select and free-text question responses
- Native reviewer policies: Claude plan mode plus write-tool deny rules; Codex read-only sandbox per turn
- Attachment API with authenticated fetch, ETag, immutable cache semantics and durable-reference protection
- Attachment content hash verification and image dimension/pixel limits
- Conservative common image limits: 5 MiB per image, 20 MiB per message, 8000 px per side and 64 MP decoded pixels
- Browser E2E validation for rich conversation, image preview and mobile layout

### Changed

- Runtime policy now follows the current stable/latest Claude Code and Codex public interfaces rather than maintaining a historical version matrix
- Role changes are applied to the native adapter before room state is persisted and are rejected during active turns or pending approvals
- Codex role changes now also reject starting inputs, queued inputs and active App Server turns before changing sandbox policy
- Unknown Claude control requests and unsupported Codex high-privilege requests fail closed
- User and Agent messages may contain images without requiring accompanying text
- Normal transcript exports include attachment metadata but never host-local attachment paths
- CSP permits only local/data/blob image rendering; remote Markdown images are represented as explicit placeholders instead of being fetched
- Store schema advanced to version 3

### Fixed

- Claude permission and interactive-question requests are no longer invisible to the PairRoom UI when the native control handshake is available
- Reviewer role no longer depends only on prompt instructions
- Orphaned uploaded images can be removed before send, while attachments already referenced by the durable transcript cannot be deleted
- Same-size local attachment tampering is detected by SHA-256 verification
- Image upload cancellation, retry and composer cleanup no longer leak object URLs or silently discard durable messages
- Long messages, image galleries and the composer remain usable in narrow/mobile layouts
- Missing favicon no longer creates a browser console error

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

### Changed

- Runtime execution failures no longer overwrite an already accepted transport delivery result
- Adapter lookup/submit failures now settle both delivery and processing
- Restart/stop settles orphaned processing and expires connection-local approvals
- Codex request construction uses documented App Server fields, including `clientUserMessageId`
- JSON transcript exports omit verbose Inspector events by default
- UI automatically resynchronizes when it detects an SSE sequence gap

### Fixed

- Partial JSONL tails are truncated before subsequent appends
- Multiple inputs steered into one Codex Turn are all settled on completion
- Fast runtime events can no longer be overwritten by a late Submit result
- Incomplete `claude --help` output no longer causes a false protocol rejection
- URL query tokens are accepted only by the read-only SSE endpoint
- Stale pending/working state no longer survives a PairRoom restart

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
