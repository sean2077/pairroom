# PairRoom Agent Contract

## Project boundary

PairRoom is a local Go coordination layer for official Claude Code, Codex, and Grok Build harnesses. Each durable Room has two stable Agent slots; either slot may select any supported runtime, including the same runtime twice. It owns the room, browser/API, persistence, workspace policy, archives, and adapter projections; it does not replace any vendor's model loop, tool runner, credentials, or session store.

## Development and verification

- `make check` runs unit, race, vet, format, JavaScript syntax, dependency, Agent-projection, release-contract, and whitespace checks. Its race stage requires `CGO_ENABLED=1` and a Go-supported C compiler on `PATH`; on Windows, use an MSYS2 MinGW toolchain or an equivalent supported compiler.
- `make smoke` runs the deterministic Mock collaboration, media, backup, restore, and diagnostics flow.
- `make browser-check` runs Room/Management fixture regressions and the authenticated real-HTTP/SSE Mock Service/restart smoke. It needs the isolated pinned Playwright tools; vendor E2E remains separate.
- `make desktop-update` rebuilds and updates an existing native desktop installation, preserving user data and launch-at-login registration; use `DESKTOP_INSTALL_DIR` for custom or ambiguous paths.
- `make install` installs the current source to `GOBIN`, defaulting to `GOPATH/bin`; it reports PATH visibility but never edits PATH.
- `make cover` records diagnostic package coverage; coverage is not a release percentage gate.
- `make release` requires a clean tree and builds/verifies the complete local release payload. It does not publish or create a tag.
- `make bump-version NEW_VERSION=X.Y.Z` rewrites every synchronized version surface fail-closed in one step (root `VERSION`, `internal/version.Current`, `desktop/build/config.yml`, and the canonical `CHANGELOG.md` heading, moving Unreleased notes into the new section); it never commits or tags.
- The installed `agent-scaffold` skill's `verify --profile default --json` mode is the authoritative full harness check.

## Durable invariants

- Keep the root module on Go 1.25. The only approved direct dependency is the pinned CGo-free `modernc.org/sqlite`; `go run scripts/check_dependencies.go` must strictly accept the reviewed transitive closure and reject drift.
- Keep collaboration mechanics in `internal/protocol` and expose them through `pairroom protocol`; project the compact versioned bootstrap and stored collaboration instructions at each vendor's native instruction layer. Per-turn envelopes carry sender, body, attachments, and explicit user-quoted message context only; IDs stay in transport/persistence. Resolve explicit user quotes from the same Room at delivery, without recursively expanding Agent-relay correlation links. Preserve the prompt byte-budget tests.
- New Rooms select only `default` (Agent 1 Lead, Agent 2 Executor) or `custom` natural-language collaboration at creation. Default responsibilities are flexible: the addressed Agent completes simple tasks directly, with delegation and review proportional to complexity and risk. Persist versioned instructions; never rewrite them at activation. Both use the live workspace; responsibility is not a permission boundary. Default native policy is YOLO, with explicit narrower or empty/native overrides respected. Only independent permission profiles may change at an idle Room boundary.
- Read only Store schema 12/provisioning 5 with explicit immutable `host_mode`; reject retired schema ≤11 before replay, repair, recovery, or rewrite. Registry checkpoint schema 3 has strict canonical slot keys and retains host mode. A retired Service root fails closed as a whole; never infer missing collaboration, selections, or bindings from current defaults. No Legacy Room import, binding completion, role switching, or reviewer snapshot workspace remains. Preserve current-schema collaboration instruction versions verbatim.

- Room names are display metadata, not lookup keys. Generate omitted names once; preserve IDs/Bindings through `service.room.renamed`. Native title projections use metadata-only APIs at activation, never model Turns, instructions, or direct vendor-store edits; distinguish desired, configured, and acknowledged names.
- Keep slot identity separate from runtime identity: durable `ActorID` values are `slot1`/`slot2` for Agent 1/Agent 2, while `RuntimeKind` selects Claude Code, Codex, or Grok Build. `claude`/`codex` are relay CLI input aliases only, never durable identities. Every adapter must emit the configured slot actor; never hard-code a vendor as the event actor.
- Project/Room display order is Service-scoped user preference in `navigation-order.json`, independent of Registry rebuilds, runtime scheduling, and Room ownership. Reordering never appends Room events or moves Rooms across Projects.
- Agent pair profiles are Service-scoped user configuration in `agent-pair-profiles.json`, separate from the rebuildable Registry. Copy their two selections at creation; never link existing Rooms to mutable profiles, capture native credentials, or skip Provider revalidation.
- New Rooms persist an immutable, secret-free `AgentSelection` for both historical ActorID slots. A native ProviderRef inherits the selected CLI's user/global configuration; a CC Switch ProviderRef is re-read at creation validation and each activation. Keep Grok Build prompt/instruction text out of process argv.
- CC Switch v3.20.1/schema 18 access is read-only and fail-closed. PairRoom never manages Providers, changes CC Switch current state, or persists credentials; secrets travel only in the selected child-process environment and never in argv, Room/Event Log, Registry, RuntimeInfo, API, browser, diagnostics, or logs.
- Empty Provider/model/effort/instructions and runtime-policy overrides inherit the selected native CLI's user/global configuration. Add only explicit PairRoom overrides, and keep Grok Build prompt/instruction text out of process argv.
- In embedded Rooms, keep delivery single-owner: only the active participant may run; cross-Agent and explicit `queue` inputs wait for its native Turn boundary, while same-target `steer` uses the adapter's typed accepted/unavailable/rejected/unknown result. The Room FIFO is the sole queue, safe queued work survives restart, and unknown native ownership fails for explicit retry instead of automatic replay.
- Treat the current runtime-derived `mention_handle` as the sole Agent-relay signal: unique runtimes use `@claude` / `@codex` / `@grok`, duplicate runtimes use stable slot suffixes `0/1`, an exact Agent handle wins over `@user` in the same response, `@user` alone ends relay, self-mentions do not route, and a response without the exact peer handle ends relay. Never reintroduce hop limits, implicit relay, legacy aliases, control markers, compact handoffs, or Workflow orchestration.
- Keep native permissions and human questions visible in the Room. The general adapter bridge may surface an unsupported interactive question with `@user`, but must not replace vendor approval semantics or leave an unexposed native prompt.
- Preserve the append-only event log and fail-closed archive, attachment, authentication, workspace, and high-privilege request boundaries described in `docs/ARCHITECTURE.md` and `docs/PROTOCOL.md`.
- Keep every built-in Web listener restricted to numeric loopback addresses; reject wildcard, LAN, and hostname binds before opening repository or service state, and use SSH local port forwarding for remote access.
- `pairroom service` is the current-working-directory-independent multi-Project/multi-Room control plane; `pairroom serve` starts a current-format standalone Room for local development and diagnostics; it does not import old Rooms into the Service.
- `pairroom daemon` installs and manages `pairroom service` through systemd, launchd, or Windows Task Scheduler; `daemon open` must validate the current authenticated numeric-loopback Management URL before opening it, and normal stop/restart must preserve graceful active-Turn draining. Desktop and `pairroom daemon start` recover a crash-stale `service.lock` after verifying the recorded PID is gone, then start or reuse the installed daemon; a live lock owner still fails closed. The Desktop host must not start a competing embedded Service or implicitly install a daemon. Without an installed daemon, Desktop owns an embedded Service; only the native Settings switch may change Desktop launch-at-login registration.
- Treat each Room Event Log as durable fact and `service-registry.json` as a rebuildable index; a `new` binding acquires global `(agent, vendor_session_id)` ownership only when its first accepted native Turn atomically materializes that identity, while `existing` bindings always resume exactly. Preserve the transcript-boundary and active-Turn non-preemption guarantees described in `docs/ARCHITECTURE.md`.
- Do not claim real Claude Code/Codex/Grok Build runtime E2E unless the official CLIs were installed, authenticated, and actually exercised; Mock verification is reported separately.
- `VERSION`, `internal/version.Current`, the exact `vX.Y.Z` tag, and the canonical `CHANGELOG.md` heading `## [vX.Y.Z] — YYYY-MM-DD` must agree.
- `.github/workflows/ci.yml` must retain the supported Linux amd64, Windows amd64, macOS arm64, and macOS amd64 binaries as uniquely named checksummed workflow artifacts, then re-download and verify the complete set before CI is green.
- `.github/workflows/release.yml` owns publication: it validates/extracts changelog notes, builds and verifies CLI artifacts, creates the GitHub Release, then downloads and rechecks the published CLI payload. The desktop workflow attaches `pairroom-desktop-*` setup/app packages to that same Release on `v*` tags, then submits the Windows installer's `inno` winget manifest to `microsoft/winget-pkgs` through a fork pull request using the `WINGET_TOKEN` secret (classic PAT, `public_repo`); winget submission is idempotent per version and its failure never rewrites the Release.

## Navigation

- Project terminology: `CONTEXT.md`
- Architecture and source-of-truth boundaries: `docs/ARCHITECTURE.md`
- Protocol and durable schema: `docs/PROTOCOL.md`
- Multi-Project/multi-Room service boundaries: `docs/ARCHITECTURE.md`
- Installation channels and per-channel upgrade/uninstall mechanics: `docs/INSTALLATION.md`
- Release acceptance: Durable invariants below (`make release`, `VERSION`/`CHANGELOG`, `release.yml`)
- Verified commands: `docs/CLI_REFERENCE.md`; native-runtime E2E limits: Durable invariants below

<!-- agent-scaffold:start — managed; keep project prose outside; upgrade refreshes this block. -->
## Agent Harness

`.agents/` is the harness source; `.claude/` and `.codex/` hold generated projections. `CLAUDE.md` links to this contract.

### Session and task context

Honor the user's session entry; prefer task-local implementation/review. Resolve the task checkout and revision; use its `AGENTS.md` chain, terminology, skills, and tool working directories. Pass peers its absolute path and review revision. A shell `cd` does not reload host instructions or permissions; resolve access or guidance conflicts explicitly.

### Worktree-per-change (hard rule)

Never edit the primary worktree, including docs. Reuse an assigned linked worktree; otherwise, if the scaffold owns creation, run `bash .agents/tools/worktree.sh new <name>`.

Keep one lifecycle owner. `done --dir <absolute-wt>` merges, ff-only pushes, and removes the worktree; it requires explicit authorization and scaffold ownership, runs outside that worktree, and is not a PR/MR handoff. Leave externally managed worktrees to their owner instead of merging or cleaning them up. Bypass the trunk guard only with explicit user approval.

### Authority documents (hard rules)

`AGENTS.md` is the canonical repository-level Agent contract; read the applicable nested chain before acting. Keep it lean and current; route detail to project docs and nest only for real local differences. Repair durable guidance drift in the same change; follow higher-priority instructions and surface material disagreement instead of guessing. Judge document metadata against evidence and user intent: drafts/superseded notes are not settled guidance; missing metadata is not a blocker.

### Project terminology (hard rule)

Every Agent, project skill, and subagent uses the declared glossary, else root `CONTEXT-MAP.md`, then `CONTEXT.md`; read only relevant contexts before using project terms. A term and each language equivalent are equally valid names for one concept — use whichever is clearest and do not force one language. Reserve avoided names for history or compatibility. Resolve durable term changes with evidence and owner intent; update the glossary in the same change. Adopt an existing glossary. Never seed an empty glossary.

### Sources and projections

- Edit skills in `.agents/skills/`, then run `bash .agents/relink-skills.sh`; commit source and symlinks.
- Edit subagents in `.agents/subagents/`, then run `python .agents/tools/generate-subagents.py`; commit source and projections.
- Do not hand-edit host projections or scaffold runtime (`.agents/tools/**`, `.agents/relink-skills.sh`, `.agents/symlink-manager.py`); use `agent-scaffold upgrade`, then `agent-scaffold verify`.
- **Third-party skills** follow project-owned placement and installation policy; preserve unrelated entries.

For Codex, confirm project trust, agent discovery, and `/hooks` approval; re-review changed definitions. Restore symlink/hardlink targets with Git, not Claude checkpoints.
<!-- agent-scaffold:end -->

## Native host boundary

Native Rooms use user-owned Claude Code/Codex/Grok Build sessions, never adapters or an Interrupt control. Enforce binding uniqueness and per-slot durable FIFO; Owner Turn is advisory. Publish full Stop replies through protocol v8 (embedded v7); explicit `relay send` ignores body mentions and can intentionally duplicate a same-turn Stop publication. Associate at bind from the official session id the harness exposes to its tool-call environment (Claude Code `CLAUDE_CODE_SESSION_ID`, Codex `CODEX_SESSION_ID`, Grok `GROK_SESSION_ID`); the approved Stop hook confirms that identity and relays replies, without implicit association. Grok hook feedback carries only readiness, never claimed inbox content; clipped outgoing replies require explicit full-text publication. Keep unconfirmed local bind attempts out of active discovery and preserve committed credentials until confirmation. Keep relay credentials in owner-only files, never model context, argv or logs. Persist `delivering` before releasing an envelope; ack only after stdout. A lease-expired `unknown` still settles on the original claimer's receipt-matched acknowledgement while no explicit Retry is pending; otherwise `unknown` requires explicit Retry. Atomically save report sequence plus pending body and reconcile its original identity before recovery. Provider/model/effort/permissions are display-only. Per-Room automatic wake (default on, changeable only at an idle Room boundary) is Service-executed and uses the vendor-sanctioned Codex queue or a confirmed Claude Code inbox capability: fixed body-free nudge, durable pre-command reservation, rate-limited, audited without thread identity or bodies, fail-closed, never automatically retried. Claude inbox address/token remain in an owner-only workspace sidecar, never public projections; socket writes record `submitted`, not vendor acceptance. Real vendor E2E is a release gate, not established by synthetic hooks or Mock. See [native contracts](docs/PROTOCOL.md#native-host-protocol-v8).

The distributable `pairroom-relay` onboarding skill is product payload, not harness SSOT: edit root `skills/pairroom-relay/SKILL.md` (published through `.claude-plugin/plugin.json` and skill installers), then copy it byte-identically over the `go:embed` projection `internal/relayclient/skill/pairroom-relay/SKILL.md`, which a freshness test enforces. Never relink either copy into `.agents/skills/`.
