# Changelog

## [Unreleased]

### Added

- `pairroom version` now reports the built git commit and the commit count since the nearest tag in its text output, with `last_tag` and `commits_since_tag` added to the JSON build metadata; `make build`, `make install`, `make release`, and the CI/release workflows inject the values through ldflags.
- `pairroom-protocol/v2` with compact, durable `PAIRROOM:HANDOFF` packets for peer turns and staged `IMPLEMENTED → REVIEW_CHANGES/REVIEW_APPROVED` Driver/Reviewer handoffs.
- `pairroom-protocol/v3` natural-language actor/action workflows with durable stages, plan-revision approval gates, visible human waits, and native read-only/write policy projection.
- Independent Claude and Codex provider profiles, including redacted inspection and reference imports from cc-connect provider tables without copying credentials.
- Room View exposes an explicit **退出 Room** control that leaves the browser view without stopping either Agent.
- Room View shows a skeleton timeline during the initial snapshot fetch and a recoverable error state with a retry control when the session or snapshot load fails, instead of leaving the timeline blank.
- Room View `c` keyboard shortcut focuses the message composer, joining the existing `[`/`]`/`?`/`/` single-key bindings.

### Changed

- Room View batches high-volume transient Runtime telemetry (command output, logs, tool/plan/diff/usage updates) into bounded 500 ms render passes while keeping `text.delta` low-latency, and preserves Activity scroll position plus Turn-card and nested disclosure state across live re-renders.
- Single and batch Room archive now stop the active Agent Turn by default before suspending the Runtime, so the operator no longer has to open the Room and stop the Agent first; rename, binding completion, suspend, capacity eviction, and service shutdown still never interrupt a Turn.
- Unaddressed messages and the Room composer now target the single current Driver instead of invoking both Agents by default; explicit `@all` remains available.
- Reviewer snapshots are refreshed immediately before each safe new reviewer turn, preventing review of the pre-implementation startup snapshot.
- Agent-to-peer delivery uses the compact handoff packet, or a bounded fallback, instead of replaying an arbitrarily long final response.
- The native collaboration contract now asks for a second Agent only when independent work can materially change the outcome and keeps tool chatter in Inspector.
- The composer exposes server-resolved, role-stable `@Driver` and `@Reviewer` targets; replying to an Agent automatically targets that participant.
- Room View toasts now show a dismiss control, pause auto-dismiss on hover, and keep error toasts visible for 8 s (4.5 s otherwise), matching the Management Shell.
- Room View per-message actions (inspect, copy, thread, reply) stay visible on touch devices that cannot hover, instead of being unreachable on the mobile layout.
- Room View image viewer footer uses localized button labels ("复制"/"适应") instead of the English "Copy"/"Fit", matching the rest of the Chinese interface; "1:1" stays as a universal ratio.

### Fixed

- Room View accessibility and undefined-variable fixes: replaced the undefined `--surface-1` token with defined surfaces, lightened dark-theme `--faint` and darkened light-theme `--faint` so small secondary text clears WCAG AA contrast on both themes, added `aria-label`/`aria-pressed` to lightbox and notification/theme controls, and gated `scrollIntoView` smooth scroll and the message flash animation behind `prefers-reduced-motion`.
- Room archive no longer fails with `room runtime close state is uncertain` on Windows when the just-interrupted Agent process briefly still holds the reviewer worktree handle; reviewer worktree removal now retries for a short grace window while the OS releases the handle.
- Stable Room projections now preserve streaming and durable message DOM identity, while explicit peer mentions no longer get lost behind generic stop markers or bypass terminal review controls and compiled-workflow sequencing.
- Management Room action controls now share consistent dimensions, and live Agent output updates only the affected streaming or message rows instead of repeatedly rebuilding the full chat timeline.
- New unthreaded `append`/`supersede` input to the same Agent now suppresses an older automatic handoff, while explicit `next_turn` remains the independent-task path.
- Mentions inside compact handoffs participate in routing, API replies inherit the replied-to Agent when no stronger target is supplied, and context-aware lifecycle serialization keeps Reviewer startup, snapshot refresh, and delivery ordered without ignoring cancelled callers.
- Compact peer handoffs survive auditable retry instead of falling back to the full human-facing response.
- A newer human message suppresses automatic handoff only in the same discussion thread, so independent tasks can progress concurrently.
- Handoff-only Agent finals are retained, oversized handoffs stay within the durable limit, and staged transfer fails closed when the evidence packet is missing or control markers conflict.
- Reviewer snapshot refresh and role changes serialize with both participant submission paths, preventing a live Driver write from racing snapshot capture.
- Workflow approval parsing distinguishes approval from negation, the first completed plan is revision `1`, intermediate `DONE` signals advance instead of terminating the sequence, failed current stages reopen safely on retry, Claude restores its configured role after plan-only stages, `discuss` compiles consistently with the public model, and explicitly addressed messages retain ordinary routing during an active workflow.
- Natural workflow state advances the Store schema to `8`, so older binaries reject event logs containing the new durable `workflow.updated` projection.
- Redacted provider inspection no longer returns raw custom arguments or URL credentials, and Codex provider header values are projected through environment variables instead of command arguments.
- Room View composer target picker (`@Driver`/`@Reviewer`/`@All`/`@Claude`/`@Codex`) now exposes `aria-pressed` on each toggle, and the toast live region uses `polite` instead of `assertive` so success notices stop interrupting screen readers.
- Management Shell static dialogs (Project, Room, Rename, Binding, Confirm) now carry `aria-labelledby` pointing at their heading so screen readers announce each dialog's purpose when it opens, matching the command palette and Room image viewer.
- Management Shell segmented controls (theme, density, Room open behavior) and the settings section nav now expose `aria-pressed` and a group `aria-label`, so the active option is announced instead of relying on the `active` class alone.
- Management Shell Runtimes "最后活动" column no longer renders the Go zero timestamp (`0001-01-01`) as a nonsensical "1年1月1日" date; a never-active Runtime now shows "—" like other empty timestamps.

## [v1.1.0] — 2026-08-21

### Added

- Management Shell credential login for direct-origin access, accepting either the configured Service Token or a complete Management URL and exchanging it for the existing HttpOnly browser session without Web Storage persistence.
- `pairroom protocol` as the deterministic, versioned source of truth for collaboration rules, with actor/role/routing filters and machine-readable JSON output.
- Responsive Room and Management interaction layers with adjustable Room panels, focus and density controls, mobile drawers and bottom navigation, a Management command palette, keyboard workflows, stronger accessibility semantics, and reduced-motion/high-contrast fallbacks; Management enhancements remain tab-memory-only with no Web Storage writes.

### Changed

- Native-agent instruction projection now keeps the compact PairRoom bootstrap at the vendor instruction layer, projects Codex through thread `developerInstructions`, and sends dynamic-only per-turn envelopes guarded by byte-budget tests.

### Fixed

- Management Shell Project cards now keep maintenance actions visible for unavailable worktrees, expose retained and archived Room blockers, and let empty Projects whose local path was deleted use the existing Registry-only unregister flow.
- Missing Room data directories are no longer recreated as partial Event Logs during archive; the Management archive/delete flow now archives only the Registry projection when the whole data directory is already absent, recognizes and removes narrowly validated archive stubs left by affected versions, and completes cleanup as `already_missing`.

## [v1.0.0] — 2026-08-18

### Added

- Downloadable checksummed Linux amd64, Windows amd64, macOS arm64, and macOS amd64 binaries on every successful CI run, with aggregate artifact-name, target-format, checksum, and embedded build-metadata verification.
- Permanent archived-Room removal in the Management API and Shell, with selection-based single/batch cleanup, explicit irreversible acknowledgement instead of typed Room-ID confirmation, Runtime admission gating, binding-ownership release, and a complete path to empty-Project unregister.
- Crash-consistent managed-data deletion through a hidden quarantine journal, durable intent/commit markers, checkpoint-aware startup recovery, fail-closed ambiguity handling, and observable/retryable physical cleanup.
- Non-destructive removal for explicitly imported external Rooms: PairRoom unregisters the Room and releases bindings while retaining the external directory.
- `POST /api/v1/maintenance/room-deletions/retry` plus Service snapshot maintenance diagnostics for committed cleanup that could not be physically completed immediately.
- `POST /api/v1/rooms/batch-archive` for 1–100 submitted Room IDs, idempotent already-archived results, non-interrupting busy-item failures, and ordered partial-success processing that continues later Rooms.
- `POST /api/v1/rooms/batch-delete` for 1–100 submitted Room IDs, first-seen de-duplication, sequential safety gating, and explicit per-Room partial-success results without rolling back completed deletion.
- Safe Project maintenance in the Management Shell and API: explicit path revalidation plus typed-confirmation unregister for empty Projects, with no Git worktree, Room data, attachment, or vendor Session/Thread deletion.
- Durable Registry removal with atomic checkpoint publication, restart persistence, remove-vs-provision serialization, and structured `project_has_rooms` conflicts that count active and archived Rooms.
- Cross-platform `pairroom daemon install|uninstall|start|stop|restart|status|logs|open` management for the multi-Project/multi-Room Service via systemd, launchd, and Windows Task Scheduler, including bounded log rotation, graceful Windows shutdown control, verified Management Shell opening, manager-aligned drain timeouts, and explicit crash-stale lock recovery.
- A routed Management Shell with Overview, Projects, Project detail, Runtimes, and grouped Settings views, plus cross-Project search, responsive navigation, semantic dialogs, connection feedback, empty states, and toast notifications.
- Service snapshot observability for effective Runtime policy, aggregate Project/Room/Runtime summary, explicit management capabilities, and per-Runtime capacity occupation.
- Safe `POST /api/v1/rooms/{room}/suspend` control that cancels queued activations or closes idle Runtimes while refusing to interrupt active Turns or discard cleanup-uncertain Runtime state.
- Interface, Runtime, daemon, Service diagnostics, and safety-boundary Settings sections; daemon guidance distinguishes restart from full service-definition replacement.
- Chromium visual smoke coverage for desktop and mobile Management Shell routes, dialogs, console errors, and horizontal overflow.
- Stable single-room product contract for one human, official Claude Code, and official Codex.
- Release CI, tag-driven GitHub release workflow, reproducible four-platform build scripts, source archives, SHA-256 checksums, SPDX 2.3 SBOM, and build provenance.
- Operations, privacy, support, upgrade, release acceptance, release notes, Git history provenance, and PR documentation.
- Version JSON/build metadata and a full Mock collaboration/recovery smoke test used by CI and release acceptance.

### Changed

- Project and Room lifecycle operations now use validated forms and dialogs instead of native browser `prompt`/`confirm`; one persistent Room selection supports batch archive followed directly by batch cleanup.
- Runtime management on narrow viewports renders as labelled cards instead of overflowing tables.
- Management UI preferences and the Bearer bootstrap secret remain tab-memory-only; the browser exchanges the secret for an HttpOnly session and keeps its CSRF token in memory, with no Web Storage persistence.
- Management polling now defaults to 10 seconds with explicit slower/faster choices, and Runtime control conflicts consistently return `409 Conflict`.
- Reviewer uses an independently materialized Git snapshot containing HEAD, dirty tracked changes, and untracked regular files; the live writable tree is no longer the default Reviewer workspace.
- `dist/` binaries are release artifacts rather than source-control contents.
- The 1.0 support boundary is intentionally one daemon, one repository, one human, one Claude Code participant, and one Codex participant.

### Fixed

- Windows Task Scheduler installations now launch the background Service through windowless Windows Script Host, preserve forwarded arguments and graceful shutdown control, and clean up legacy PowerShell launchers during reinstall or uninstall.
- All built-in Web listeners now reject wildcard, LAN, and hostname binds before opening repository or service state, closing the legacy `pairroom serve` LAN-exposure path.

### Documentation

- Rebuilt the documentation around the current multi-Project/multi-Room architecture instead of the original single-daemon boundary.
- Added a documentation hub, end-to-end quick start, core concepts, complete CLI reference, symptom-oriented troubleshooting guide, and source-oriented development guide.
- Reworked architecture, operations, security, privacy, support, upgrade, Runtime compatibility, Management Shell, Service, contribution, and product-plan documentation into explicit sources of truth.
- Documented the separately scoped Management and Room fragment-to-HttpOnly-session/CSRF flows, direct Bearer compatibility for API clients, loopback-only listeners, and exact durable Runtime resume semantics.
- Added a current-main cc-connect UX research and adaptation record plus a Management Shell interaction/API contract.

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
