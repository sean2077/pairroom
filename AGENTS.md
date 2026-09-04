# PairRoom Agent Contract

## Project boundary

PairRoom is a local Go coordination layer for official Claude Code, Codex, and Grok Build harnesses. Each durable Room has two stable Agent slots; either slot may select any supported runtime, including the same runtime twice. It owns the room, browser/API, persistence, workspace policy, archives, and adapter projections; it does not replace any vendor's model loop, tool runner, credentials, or session store.

## Development and verification

- `make check` runs unit, race, vet, format, JavaScript syntax, dependency, Agent-projection, release-contract, and whitespace checks. Its race stage requires `CGO_ENABLED=1` and a Go-supported C compiler on `PATH`; on Windows, use an MSYS2 MinGW toolchain or an equivalent supported compiler.
- `make smoke` runs the deterministic Mock collaboration, media, backup, restore, and diagnostics flow.
- `make install` installs the current source to `GOBIN`, defaulting to `GOPATH/bin`; it reports PATH visibility but never edits PATH.
- `make cover` records diagnostic package coverage; coverage is not a release percentage gate.
- `make release` requires a clean tree and builds/verifies the complete local release payload. It does not publish or create a tag.
- The installed `agent-scaffold` skill's `verify --profile default --json` mode is the authoritative full harness check.

## Durable invariants

- Keep the root module on Go 1.25. The only approved direct dependency is the pinned CGo-free `modernc.org/sqlite`; `go run scripts/check_dependencies.go` must strictly accept the reviewed transitive closure and reject drift.
- Keep collaboration mechanics in `internal/protocol` and expose them through `pairroom protocol`; project only a compact versioned bootstrap at each vendor's native instruction layer, keep per-turn envelopes dynamic-only, and preserve the prompt byte-budget tests.
- Keep slot identity separate from runtime identity: durable `ActorID` values identify Agent 1/Agent 2 (legacy `claude`/`codex` slot IDs), while `RuntimeKind` selects Claude Code, Codex, or Grok Build. Every adapter must emit the configured slot actor; never hard-code a vendor as the event actor.
- New Rooms persist an immutable, secret-free `AgentSelection` for both historical ActorID slots. A native ProviderRef inherits the selected CLI's user/global configuration; a CC Switch ProviderRef is re-read at creation validation and each activation. Keep Grok Build prompt/instruction text out of process argv.
- Keep Room delivery single-owner: only the active participant may run, `next_turn` and cross-Agent inputs wait for its native Turn boundary, and an explicit Agent `@peer` address requests transfer; an implicit continuation still requires a compact `HANDOFF` plus `NEXT`, while `@human`/`@user` returns control to the human. A human request that both Agents interact still starts with the current Driver, who must address the peer rather than answering as a 1:1 chat.
- Natural workflows compile only explicit actor/action sequences. Keep plan/review/audit read-only, bind execution approval to the current plan revision, and surface human questions in the Room rather than leaving a native process on an unexposed prompt.
- CC Switch v3.20.1/schema 18 access is read-only and fail-closed. PairRoom never manages Providers, changes CC Switch current state, or persists credentials; secrets travel only in the selected child-process environment and never in argv, Room/Event Log, Registry, RuntimeInfo, API, browser, diagnostics, or logs.
- Preserve the append-only event log and fail-closed archive, attachment, authentication, workspace, and high-privilege request boundaries described in `docs/ARCHITECTURE.md` and `docs/PROTOCOL.md`.
- Keep every built-in Web listener restricted to numeric loopback addresses; reject wildcard, LAN, and hostname binds before opening repository or service state, and use SSH local port forwarding for remote access.
- `pairroom service` is the current-working-directory-independent multi-Project/multi-Room control plane; `pairroom serve` remains the legacy single-Room compatibility entry point.
- `pairroom daemon` installs and manages `pairroom service` through systemd, launchd, or Windows Task Scheduler; `daemon open` must validate the current authenticated numeric-loopback Management URL before opening it, normal stop/restart must preserve graceful active-Turn draining, crash-stale `service.lock` recovery remains explicit, and the Desktop host must reuse or start an installed daemon instead of silently creating a competing embedded Service.
- Treat each Room Event Log as durable fact and `service-registry.json` as a rebuildable index; a `new` binding acquires global `(agent, vendor_session_id)` ownership only when its first accepted native Turn atomically materializes that identity, while `existing` bindings always resume exactly. Preserve the transcript-boundary and active-Turn non-preemption guarantees described in `docs/ARCHITECTURE.md`.
- Do not claim real Claude Code/Codex/Grok Build runtime E2E unless the official CLIs were installed, authenticated, and actually exercised; Mock verification is reported separately.
- `VERSION`, `internal/version.Current`, the exact `vX.Y.Z` tag, and the canonical `CHANGELOG.md` heading `## [vX.Y.Z] — YYYY-MM-DD` must agree.
- `.github/workflows/ci.yml` must retain the supported Linux amd64, Windows amd64, macOS arm64, and macOS amd64 binaries as uniquely named checksummed workflow artifacts, then re-download and verify the complete set before CI is green.
- `.github/workflows/release.yml` owns publication: it validates/extracts changelog notes, builds and verifies CLI artifacts, creates the GitHub Release, then downloads and rechecks the published CLI payload. The desktop workflow attaches `pairroom-desktop-*` setup/app packages to that same Release on `v*` tags.

## Navigation

- Architecture and source-of-truth boundaries: `docs/ARCHITECTURE.md`
- Protocol and durable schema: `docs/PROTOCOL.md`
- Multi-Project/multi-Room service boundaries: `docs/ARCHITECTURE.md`
- Release acceptance: Durable invariants below (`make release`, `VERSION`/`CHANGELOG`, `release.yml`)
- Verified commands: `docs/CLI_REFERENCE.md`; native-runtime E2E limits: Durable invariants below

<!-- agent-scaffold:start — managed; keep project prose outside; upgrade refreshes this block. -->
## Agent Harness (Claude Code + Codex)

`.agents/` is the SSOT for harness-owned skills, subagents, and runtime; `.claude/` and `.codex/` contain host projections.

### Worktree-per-change (hard rule)

The primary worktree's checked-out branch is the active trunk (`--trunk` overrides); `new` records it and `done` merges back. Never edit the primary worktree directly, including docs:

```bash
bash .agents/tools/worktree.sh new <name>  # work in .worktrees/<name>/
bash .agents/tools/worktree.sh done        # merge, clean up, and ff-only push
```

On Windows, leave the target worktree and run `done --dir <absolute-wt>` from the primary worktree; `new` prints the exact command.

The trunk guard blocks non-ignored project-file edits in the primary worktree, regardless of branch name. Bypass it only with explicit user approval: `WORKTREE_ALLOW_TRUNK_EDIT=1`, or `touch .claude/allow-trunk-edit` for a 2 h flag.

### Authority documents (hard rules)

`AGENTS.md` is the canonical repository-level contract for Agent work. Read the root contract and applicable nested chain before acting.

- **Keep it current.** When a durable Agent-relevant change makes guidance stale, update it in the same change.
- **Keep it lean.** Keep only frequent or costly-to-miss behavior; route depth to project docs.
- **Keep scopes honest.** Add nested `AGENTS.md` only for a concrete local difference; directory structure alone never justifies one.
- **Resolve conflicts explicitly.** Surface conflicts, follow higher-priority instructions, ask the owner when authority is unclear, and repair stale guidance when authorized.

The authority-document budget hook remains advisory; projects may override its default line and character limits.

### Project terminology (hard rule)

Every Agent, project skill, and subagent uses the canonical terminology source declared in project-owned `AGENTS.md` prose. If none is declared, read root `CONTEXT-MAP.md` when present; otherwise use root `CONTEXT.md`. The map routes multi-context repositories to context-local `CONTEXT.md` files.

- **Load only what applies.** Before naming or interpreting project concepts, read the declared glossary or map and only the relevant context file.
- **Use canonical equivalents.** A glossary entry term and each `_Equivalent (<language-tag>)_` value are equally valid names for the same concept. Use whichever form is clearest in the current conversation or document; do not force one language. Keep at most one canonical name per language.
- **Recognize but do not propagate avoided names.** Record historical, ambiguous, mistranslated, or retired names under `_Avoid (<language-tag>)_`. Use them only for quotation, history search, migration, or an externally fixed compatibility boundary.
- **Close vocabulary drift.** When a durable concept, translation, ambiguity, or synonym appears, resolve it against repository evidence and project-owner intent, then update the applicable glossary in the same change. Do not silently introduce a competing name.
- **Keep glossaries focused.** Define project-specific concepts briefly and without behavior, architecture, or decision detail.
- **Evolve topology proportionally.** Keep one glossary while subject headings are sufficient; use `CONTEXT-MAP.md` and context-local glossaries only for durable semantic or ownership boundaries. Honor an explicit up-front or incremental modeling choice. If early evidence is insufficient, get project-owner input instead of inventing domains.

A multilingual glossary may list its `Canonical term languages` once. That list declares maintained coverage, not a preferred or mandatory discussion language. If no source is declared, adopt an existing project glossary rather than duplicating it; if none exists, create root `CONTEXT.md` only when the first durable project term is resolved. Never seed an empty glossary.

### Sources and projections

- Edit project skills in `.agents/skills/<name>/`, then run `bash .agents/relink-skills.sh`; commit source and symlink.
- Edit project subagents in `.agents/subagents/<name>/`, then run `python .agents/tools/generate-subagents.py`; commit source and projections.
- Do not hand-edit harness projections: `CLAUDE.md`, `.claude/skills/<name>` entries owned by `.agents/skills/`, `.claude/agents/*.md`, or `.codex/agents/*.toml`.
- Do not hand-edit scaffold runtime: `.agents/tools/**`, `.agents/relink-skills.sh`, or `.agents/symlink-manager.py`. Refresh it with `agent-scaffold upgrade`, then run `agent-scaffold verify`.
- **Third-party skills** follow project-owned placement and installation policy. The relinker preserves unrelated names and rejects same-name conflicts.

For Codex, trust the project, confirm generated agents are discoverable, and review each exact hook definition in `/hooks`; re-review changed definitions. Claude checkpoints do not rewind symlinked or hard-linked targets (`CLAUDE.md`, `.claude/skills/*`); inspect and restore the real target with Git.
<!-- agent-scaffold:end -->
