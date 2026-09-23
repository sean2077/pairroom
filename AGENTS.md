# PairRoom Agent Contract

## Project boundary

PairRoom is a local Go coordination layer for Claude Code, Codex, and Grok Build. Each Room has two stable slots, independently selecting a supported Runtime. Embedded owns adapters; Native relays between user-owned sessions. Neither replaces the vendor model/tool loop, credentials, or session store.

Read the applicable nested contract and [terminology](CONTEXT.md), then the owner of the surface being changed in the [documentation map](docs/README.md). Current code and verified contracts outrank historical plans; a plan is not evidence of implementation or new authorization.

## Development and verification

Use a task branch/worktree and submit a PR; do not push directly to `main` or run a merge/cleanup helper without authorization. Keep changes proportional to the task. Repair owning documentation and add regression evidence for changed state transitions, not just prompt wording.

`make docs-check` checks documentation contracts; `make check` covers static/unit/race/dependency/JavaScript/projection/release checks; `make smoke` exercises deterministic Mock recovery. Browser, desktop, release, and environment prerequisites are in [Contributing](CONTRIBUTING.md) and [Desktop development](desktop/README.md). Report the actual commands and results, separating local checks, CI, fixtures, and authenticated vendor E2E. Never claim vendor acceptance, billed-token savings, or production signing from synthetic tests.

## Durable invariants

- Keep the root module on Go 1.25 with the approved pinned CGo-free SQLite dependency closure. Dependency checks must reject replacements and version drift; GUI dependencies remain in the separate desktop module.
- Persist canonical `slot1`/`slot2` actors independently of Runtime. Adapters emit their configured slot, never a hard-coded vendor actor. Keep the current strict formats and whole-root retirement boundary: Store 12/provisioning 5, checkpoint 3, explicit immutable `host_mode`; reject retired data before replay, recovery, repair, or rewrite. Never infer missing historical selections/Bindings from current defaults. [Storage](docs/STORAGE.md) and [Upgrading](docs/UPGRADING.md) own schema details.
- Room Event Logs own durable facts; Registry and current-work indexes are derived. Navigation order and Agent pair profiles are separate Service user preferences. Copy pair selections at creation rather than linking existing Rooms to mutable profiles. Names are metadata, not identity; preserve IDs and Bindings. See [Architecture](docs/ARCHITECTURE.md).
- Collaboration is creation-time `default` or `custom`, with versioned instructions preserved on activation. Simple tasks may finish directly; responsibilities are not permissions or mandatory workflow phases. Embedded defaults to YOLO while respecting explicit narrower/native-inherited policies. Native configuration is display-only. Never reintroduce role switching, role-bound workspaces, legacy imports, or automatic plan approval.
- Keep protocol mechanics in `internal/protocol/`: Embedded v7, Native v8. Preserve compact bootstrap/envelope budgets, full addressed replies, and same-Room explicit quotes without recursively expanding relay history. Only exact current runtime-derived peer handles route automatic relay; a peer handle wins over `@user`. Native unaddressed Stop replies record receipts, not Room message bodies. Explicit Native send uses its command target and may intentionally duplicate Stop publication. [Protocol](docs/PROTOCOL.md) owns matching and envelope rules; do not add hop limits, control markers, implicit relay, summaries, or a workflow engine.
- Embedded has one Room-owned FIFO and one native Turn owner. Typed steering outcomes distinguish accepted, unavailable, rejected, and unknown. Unknown submission is not safe to queue again; only reliable terminal evidence releases ownership. Permissions change only at an idle boundary, and native approval/question identity and scope remain visible. Native has per-slot FIFO with advisory ownership, not a process-start/Interrupt control or a writer lock.
- Embedded new Bindings materialize on accepted native execution; existing sessions resume exactly. Native associates at bind from official tool-call session metadata, then approved hooks confirm it. Check Runtime/session uniqueness across Rooms, including archived Rooms. Changing cwd does not retarget a bound session. Unconfirmed attempts stay outside active discovery, and replacement cannot undo accepted work.
- Native persists a delivery claim before releasing an envelope and acknowledges only after stdout. `handed_off` is not model acceptance. An original receipt may settle `unknown` while no explicit Retry is pending; otherwise inspect effects before explicit Retry. Atomically retain pending publication identity and reconcile that key, never automatically replay uncertain effects. Grok readiness does not claim inbox text; clipped replies require full-text publication.
- Native wake is a fixed body-free, rate-limited Service nudge through supported Claude/Codex capabilities, with durable reservation before the effect and no automatic retry of reserved effects. Only unattempted eligible heads may be deferred/rechecked. Keep audit outcomes conservative and omit task text, thread identity, and credentials. Claude capability sidecars are private; socket submission is not native acceptance. See [Native contracts](docs/PROTOCOL.md#native-host-protocol-v8).
- Selections remain immutable and secret-free. Embedded CC Switch access is read-only/fail-closed and revalidates references without changing global Provider state; empty overrides retain native inheritance. Resolved Provider secrets belong only in the selected child environment, never argv, projections, persistence, logs, or diagnostics. Grok prompts travel over native transport, not argv. Native relay credentials/capabilities stay in private owner-only files, not model context. [Configuration](docs/CONFIGURATION.md) and [Security](SECURITY.md) own the full boundaries.
- Preserve append-only integrity and fail-closed authentication, attachments, backups, workspace access, and high-privilege requests. Built-in listeners accept numeric loopback only, validated before state opens; remote access uses protected SSH forwarding, not wildcard/LAN/hostname binds.
- One Service owns a data root. Desktop never installs a daemon on launch or creates a competing Service; recover a stale lock only after its recorded PID has exited. Launch-at-login is explicit native Settings state. Closing a view does not archive/stop work; Native archive cannot stop user sessions. [Operations](docs/OPERATIONS.md) owns lifecycle and [Contributing](CONTRIBUTING.md#release-verification) owns release/version/artifact invariants.

## Product skill versus project harness

The distributed onboarding skill is product payload: edit `skills/pairroom-relay/SKILL.md`, then copy it byte-identically to `internal/relayclient/skill/pairroom-relay/SKILL.md`. The freshness test enforces the embedded projection. Do not relink either into `.agents/skills/` or independently edit generated host projections.

<!-- agent-scaffold:start — managed; keep project prose outside; upgrade refreshes this block. -->
<!-- agent-scaffold:profile=default -->
## Agent Harness

`.agents/` is the harness source; `.claude/` and `.codex/` hold generated projections. `CLAUDE.md` links to this contract.

### Session and task context

Honor the user's session entry; prefer task-local implementation/review. Resolve the task checkout and revision; use its `AGENTS.md` chain, terminology, skills, and tool working directories. Pass peers its absolute path and review revision. A shell `cd` does not reload host instructions or permissions; resolve access or guidance conflicts explicitly.

### Worktree-per-change (hard rule)

Never edit the primary worktree, including docs. Reuse an assigned linked worktree; otherwise, if the scaffold owns creation, run `bash .agents/tools/worktree.sh new <name>`.

Keep one lifecycle owner. `done --dir <absolute-wt>` merges, ff-only pushes, and removes the worktree; it requires explicit authorization and scaffold ownership, runs outside that worktree, and is not a PR/MR handoff. Leave externally managed worktrees to their owner instead of merging or cleaning them up. Bypass the trunk guard only with explicit user approval.

### Authority documents (hard rules)

`AGENTS.md` is the canonical repository-level Agent contract; read the applicable nested chain before acting. Keep it lean and current; route detail to project docs and nest only for real local differences. Repair durable guidance drift in the same change; follow higher-priority instructions and surface material disagreement instead of guessing. Interpret document status and freshness alongside repository evidence and user intent.

### Project terminology (hard rule)

Every Agent, project skill, and subagent uses the declared glossary, else root `CONTEXT-MAP.md`, then `CONTEXT.md`; read only relevant contexts before using project terms. A term and each language equivalent are equally valid names for one concept — use whichever is clearest and do not force one language. Reserve avoided names for history or compatibility. Resolve durable term changes with evidence and owner intent; update the glossary in the same change. Adopt an existing glossary; add definitions as durable concepts are resolved.

### Sources and projections

- Edit skills in `.agents/skills/`, then run `bash .agents/relink-skills.sh`; commit source and symlinks.
- Edit subagents in `.agents/subagents/`, then run `python .agents/tools/generate-subagents.py`; commit source and projections.
- Do not hand-edit host projections or scaffold runtime (`.agents/tools/**`, `.agents/relink-skills.sh`, `.agents/symlink-manager.py`); use `agent-scaffold upgrade`, then `agent-scaffold verify`.
- **Third-party skills** follow project-owned placement and installation policy; preserve unrelated entries.

For Codex, confirm project trust, agent discovery, and `/hooks` approval; re-review changed definitions. Restore symlink/hardlink targets with Git, not Claude checkpoints.
<!-- agent-scaffold:end -->
