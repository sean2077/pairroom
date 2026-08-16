# Changelog

## [Unreleased]

### Added

- Cross-platform `pairroom daemon install|uninstall|start|stop|restart|status|logs` management for the multi-Project/multi-Room Service via systemd, launchd, and Windows Task Scheduler, including bounded log rotation, graceful Windows shutdown control, manager-aligned drain timeouts, and explicit crash-stale lock recovery.
- A routed Management Shell with Overview, Projects, Project detail, Runtimes, and grouped Settings views, plus cross-Project search, responsive navigation, semantic dialogs, connection feedback, empty states, and toast notifications.
- Service snapshot observability for effective Runtime policy, aggregate Project/Room/Runtime summary, explicit management capabilities, and per-Runtime capacity occupation.
- Safe `POST /api/v1/rooms/{room}/suspend` control that cancels queued activations or closes idle Runtimes while refusing to interrupt active Turns or discard cleanup-uncertain Runtime state.
- Interface, Runtime, daemon, Service diagnostics, and safety-boundary Settings sections; daemon guidance distinguishes restart from full service-definition replacement.
- Chromium visual smoke coverage for desktop and mobile Management Shell routes, dialogs, console errors, and horizontal overflow.

### Changed

- Project and Room lifecycle operations now use validated forms and dialogs instead of native browser `prompt`/`confirm`.
- Runtime management on narrow viewports renders as labelled cards instead of overflowing tables.
- Management UI preferences and bearer bootstrap state remain tab-memory-only; no Web Storage persistence was introduced.
- Management polling now defaults to 10 seconds with explicit slower/faster choices, and Runtime control conflicts consistently return `409 Conflict`.

### Fixed

- Windows Task Scheduler installations now launch the background Service through windowless Windows Script Host, preserve forwarded arguments and graceful shutdown control, and clean up legacy PowerShell launchers during reinstall or uninstall.
- All built-in Web listeners now reject wildcard, LAN, and hostname binds before opening repository or service state, closing the legacy `pairroom serve` LAN-exposure path.

### Documentation

- Added a current-main cc-connect UX research and adaptation record plus a Management Shell interaction/API contract.

## [v1.0.0] — 2026-08-14

### Added

- Stable single-room product contract for one human, official Claude Code, and official Codex.
- Release CI, tag-driven GitHub release workflow, reproducible four-platform build scripts, source archives, SHA-256 checksums, SPDX 2.3 SBOM, and build provenance.
- Operations, privacy, support, upgrade, release acceptance, release notes, Git history provenance, and PR documentation.
- Version JSON/build metadata and a full Mock collaboration/recovery smoke test used by CI and release acceptance.

### Changed

- Reviewer uses an independently materialized Git snapshot containing HEAD, dirty tracked changes, and untracked regular files; the live writable tree is no longer the default Reviewer workspace.
- `dist/` binaries are release artifacts rather than source-control contents.
- The 1.0 support boundary is intentionally one daemon, one repository, one human, one Claude Code participant, and one Codex participant.

### Security

- Browser bootstrap credentials are exchanged from a URL fragment for short-lived HttpOnly sessions and are never stored in Web Storage.
- Browser mutations require per-session CSRF; query tokens authorize no API; API requests are rate-limited.
- Backup/restore, attachments, Reviewer isolation, and unknown high-privilege runtime requests continue to fail closed.

## [v0.9.0] — 2026-08-14

### Added

- One-time URL-fragment bootstrap exchange for short-lived HttpOnly browser sessions.
- SameSite=Strict session cookies, per-session CSRF secrets, explicit logout, and sliding 12-hour expiry.
- Fixed-window per-client API abuse protection with bounded in-memory state.
- Browser-session and CSRF tests covering bootstrap, authenticated SSE, mutation rejection, and revocation.

### Changed

- Browser credentials are no longer placed in query parameters, browser history, `sessionStorage`, or `localStorage`.
- Native/API clients may continue using the configured Bearer token directly; browser EventSource now authenticates with its session cookie.
- Query-string tokens no longer authorize any API endpoint, including SSE.

## [v0.8.0] — 2026-08-14

### Added

- Windowed room snapshots and cursor-based history pagination for long-running conversations.
- Per-room persistent composer drafts, target/intent restoration, unread counts, and optional desktop notifications.
- Enhanced image viewer with 25%–800% zoom, rotation, fit/1:1 modes, wheel zoom, and clipboard copy.
- Message-window metadata that makes partial transcript loading explicit instead of silently hiding history.

### Changed

- The browser loads the newest 250 messages initially and fetches older pages without losing scroll position.
- Incoming Agent messages update unread state only when the room is hidden or the user has scrolled away from the latest discussion.
- Public snapshot pagination is transport-only; the event-sourced room retains the complete transcript.

## [v0.7.0] — 2026-08-14

### Added

- `pairroom verify` for strict event-sequence, metadata, room-ID, attachment-manifest, size, and SHA-256 validation.
- Self-describing `pairroom backup` archives with an internal manifest and full post-write restore validation.
- Strict `pairroom restore` with traversal, link, duplicate, undeclared-file, size, and hash rejection plus atomic target replacement.
- Redacted `pairroom diagnostics` bundles containing structural event headers and integrity results without transcript text or image bytes.

### Changed

- Runtime caches, reviewer worktrees, browser sessions, and temporary files are intentionally excluded from backups.
- Backup replacement and forced restore preserve the previous target until the new artifact has been fully committed.

## [v0.6.0] — 2026-08-14

### Added

- Durable vendor-neutral Turn summaries for tools, commands, plans, diffs, usage, final responses, failures, and duration.
- Compact Work Inspector cards that survive daemon restart while retaining recent native events for diagnostics.
- Message-to-Turn correlation in the Inspector, with bounded item and output retention for long sessions.

### Changed

- High-frequency command output updates the live summary without forcing a disk sync for every chunk; terminal Turn events persist the bounded projection.
- Incomplete work items are settled when their native Turn completes, avoiding permanently working Inspector rows.

## [v0.5.0] — 2026-08-14

### Added

- Explicit message intents: append to the active discussion, queue for the next turn, or supersede in-flight instructions.
- Per-target cancellation endpoint and UI controls with honest whole-turn/queue cancellation semantics.
- Durable `supersedes` references and visible intent markers in the shared timeline.
- Codex next-turn intent that avoids `turn/steer` and waits for a safe turn boundary.

### Changed

- Superseding a target interrupts its native runtime, marks every affected in-flight message, and prevents stale automatic handoff.
- Retries preserve the original message intent while remaining separate auditable messages.

## [v0.4.0] — 2026-08-14

### Added

- Reviewer Git snapshot that includes committed HEAD, dirty tracked changes, and untracked regular files.
- Durable workspace-boundary metadata with source HEAD, snapshot digest, dirty state, and read-only enforcement status.
- Safe workspace reconfiguration for Claude Code, Codex, and mock adapters.
- Atomic two-participant driver/reviewer switch event with rollback on adapter reconfiguration failure.
- Reviewer snapshot tests covering dirty files, untracked files, replacement, and symlink rejection.

### Changed

- Reviewer sessions now run from an isolated snapshot by default instead of the live driver working tree.
- POSIX snapshots are made filesystem read-only; Windows reports the weaker boundary explicitly and continues to rely on native reviewer sandboxing.

## [v0.3.0] — 2026-08-13

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

## [v0.2.0] — 2026-08-13

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

## [v0.1.0] — 2026-08-13

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
