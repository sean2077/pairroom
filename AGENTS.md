# PairRoom Agent Contract

## Project boundary

PairRoom is a local Go coordination layer for the official Claude Code and Codex harnesses. It owns the room, browser/API, persistence, workspace policy, archives, and adapter projections; it does not replace either vendor's model loop, tool runner, credentials, or session store.

## Development and verification

- `make check` runs unit, race, vet, format, JavaScript syntax, dependency, Agent-projection, release-contract, and whitespace checks. Its race stage requires `CGO_ENABLED=1` and a Go-supported C compiler on `PATH`; on Windows, use an MSYS2 MinGW toolchain or an equivalent supported compiler.
- `make smoke` runs the deterministic Mock collaboration, media, backup, restore, and diagnostics flow.
- `make install` installs the current source to `GOBIN`, defaulting to `GOPATH/bin`; it reports PATH visibility but never edits PATH.
- `make cover` records diagnostic package coverage; coverage is not a release percentage gate.
- `make release` requires a clean tree and builds/verifies the complete local release payload. It does not publish or create a tag.
- The installed `agent-scaffold` skill's `verify --profile default --json` mode is the authoritative full harness check.

## Durable invariants

- Keep the Go module dependency-free beyond the standard library; `go list -m all` must contain only this module.
- Keep collaboration mechanics in `internal/protocol` and expose them through `pairroom protocol`; project only a compact versioned bootstrap at each vendor's native instruction layer, keep per-turn envelopes dynamic-only, and preserve the prompt byte-budget tests.
- Preserve the append-only event log and fail-closed archive, attachment, authentication, workspace, and high-privilege request boundaries described in `docs/ARCHITECTURE.md` and `docs/PROTOCOL.md`.
- Keep every built-in Web listener restricted to numeric loopback addresses; reject wildcard, LAN, and hostname binds before opening repository or service state, and use SSH local port forwarding for remote access.
- `pairroom service` is the current-working-directory-independent multi-Project/multi-Room control plane; `pairroom serve` remains the legacy single-Room compatibility entry point.
- `pairroom daemon` installs and manages `pairroom service` through systemd, launchd, or Windows Task Scheduler; `daemon open` must validate the current authenticated numeric-loopback Management URL before opening it, normal stop/restart must preserve graceful active-Turn draining, and crash-stale `service.lock` recovery remains explicit.
- Treat each Room Event Log as durable fact and `service-registry.json` as a rebuildable index; a `new` binding acquires global `(agent, vendor_session_id)` ownership only when its first accepted native Turn atomically materializes that identity, while `existing` bindings always resume exactly. Preserve the transcript-boundary and active-Turn non-preemption guarantees described in `docs/MULTI_ROOM_SERVICE.md`.
- Do not claim real Claude Code/Codex runtime E2E unless both official CLIs were installed, authenticated, and actually exercised; Mock verification is reported separately.
- `VERSION`, `internal/version.Current`, the exact `vX.Y.Z` tag, and the canonical `CHANGELOG.md` heading `## [vX.Y.Z] — YYYY-MM-DD` must agree.
- `.github/workflows/ci.yml` must retain the supported Linux amd64, Windows amd64, macOS arm64, and macOS amd64 binaries as uniquely named checksummed workflow artifacts, then re-download and verify the complete set before CI is green.
- `.github/workflows/release.yml` owns publication: it validates/extracts changelog notes, builds and verifies artifacts, creates the GitHub Release, then downloads and rechecks the published payload.

## Navigation

- Architecture and source-of-truth boundaries: `docs/ARCHITECTURE.md`
- Protocol and durable schema: `docs/PROTOCOL.md`
- Multi-Project/multi-Room service boundaries: `docs/MULTI_ROOM_SERVICE.md`
- Release acceptance: `docs/RELEASE_CHECKLIST.md`
- Verified commands and native-runtime limits: `docs/VALIDATION.md`

<!-- agent-scaffold:start — managed by the agent-scaffold skill. Edit project prose OUTSIDE these markers; `agent-scaffold upgrade` refreshes this block. -->
## Agent Harness (Claude Code + Codex)

This repo carries a vendored, dual-host agent harness. `.agents/` is the single source of truth (SSOT); `.claude/` and `.codex/` are wired to the **same** implementations under `.agents/tools/`.

### Worktree-per-change (hard rule)

**Never edit trunk (`main`) directly** — every change, however small ("just docs" is NOT an exception), starts in its own worktree cut from the trunk tip:

```bash
bash .agents/tools/worktree.sh new <name>   # edit inside .worktrees/<name>/  (branch feat|fix|docs|chore/<name>)
bash .agents/tools/worktree.sh done         # merge back to local trunk (--no-ff) + clean up + ff-only push
```

`.agents/tools/hooks/trunk_edit_guard.sh` (PreToolUse) mechanically blocks edits to tracked files while on trunk. Escape hatch — only when the user explicitly authorizes a trunk edit: `touch .claude/allow-trunk-edit` (auto-expires in 2 h) or `WORKTREE_ALLOW_TRUNK_EDIT=1`.

### Authority documents (hard rules)

`AGENTS.md` is the canonical repository-level contract for Agent work. Read and follow the root contract and its applicable nested contract chain before acting; higher-priority instructions still govern.

- **Keep it current.** When a durable change affects an Agent-relevant command, invariant, ownership boundary, risk boundary, or navigation path, update or remove the affected contract guidance in the same change. If the detail lives in linked project docs, update it there and keep the contract summary and link accurate.
- **Keep it lean.** Keep only concise, actionable guidance that changes Agent behavior and is frequently needed or costly to miss. Move explanations, rationale, history, long procedures, examples, and low-frequency detail to project docs and link to it.
- **Keep scopes honest.** Root rules are project-wide. Create a nested `AGENTS.md` only for a concrete local difference from the nearest ancestor; directory structure alone never justifies one.
- **Resolve conflicts explicitly.** If applicable instructions conflict, or contract guidance disagrees with verified repository facts, do not guess or silently ignore either. Surface the conflict, follow higher-priority instructions, request owner direction when authority is unclear, and repair stale guidance in the same change when authorized.

The authority-document budget hook remains advisory; projects may override its default line and character limits when justified.

### SSOT layout

| Path | Role | Commit? |
|---|---|---|
| `.agents/skills/<name>/SKILL.md` | project skill source | ✅ |
| `.agents/subagents/<name>/{metadata.json,instructions.md}` | subagent source | ✅ |
| `.claude/skills/<name>` | symlink → `.agents/skills/<name>` (CC discovery; Codex reads `.agents/` directly) | ✅ |
| `.claude/agents/*.md`, `.codex/agents/*.toml` | **generated** subagent projections — do NOT hand-edit | ✅ |
| `.agents/tools/hooks/` | scaffold-managed hook runtime (doc budget + optional trunk guard) — **managed copies, do NOT hand-edit** | ✅ |
| `.agents/tools/worktree.sh` | worktree lifecycle — **managed copy, do NOT hand-edit** | ✅ |
| `.claude/allow-trunk-edit` | worktree escape hatch | ❌ ignored |
| `.claude/settings.local.json` | personal overrides | ❌ ignored |

- **Change managed runtime**: everything under `.agents/tools/` is a copy the skill owns. Edit the skill's bundled source and run `agent-scaffold upgrade` to refresh — a hand-edit here is drift, and `agent-scaffold verify` reports it.
- **Add a skill**: edit `.agents/skills/` → run `bash .agents/relink-skills.sh` → commit source + symlink.
- **Add a subagent** (needs python): edit `.agents/subagents/` → run `python .agents/tools/generate-subagents.py` → commit source + generated. Wire `--check` into the project's own CI or hook manager when desired.
- **Third-party skills** follow project-owned placement and installation policy. The relinker manages only names sourced from `.agents/skills/`, preserves unrelated entries, and fails on same-name ownership conflicts.

**Codex trust**: project-level `.codex/` (config + hooks + agents) only loads for a **trusted** project; until trusted it is silently skipped. Trust once: run `codex` here and accept, or add `[projects."<repo abs path>"] trust_level = "trusted"` to `~/.codex/config.toml`.
<!-- agent-scaffold:end -->
