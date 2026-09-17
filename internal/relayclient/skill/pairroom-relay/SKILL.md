---
name: pairroom-relay
description: Create or join a PairRoom Room and collaborate with another native session.
---

# PairRoom relay

Run through this native session's tools in the bound project's Git workspace. Reuse its Room for follow-up work; follow the bind result's collaboration rules. Do simple work directly and involve the peer only when useful.

**Create** (`/pairroom-relay <topic>`): `pairroom relay bind --create --name "<topic>"`. Only if preflight explicitly says no Room was created, choose the intended peer and retry once with `--peer-runtime claude|codex|grok`. Otherwise follow recovery instructions, never repeat `--create`. Give the peer the returned `peer_join_local` for the same workspace, or `peer_join` with its explicit paths; do not reconstruct it.

**Join/resume**: `pairroom relay bind`. Follow candidates only when ambiguous; slots are `--slot 1|2`, not runtimes. Bind is immediately usable: no nonce, status check or extra finished turn is required. Do not re-enter session/provider settings.

**Discuss**: receiver runs `pairroom relay wait`; sender runs `pairroom relay exchange --id <stable-id> --text "<message>"` (stdin for long text). Both wait up to one hour by default, renewing in the CLI without model turns. Reuse an ID only for the same publication. After a confirmed send times out, use `wait`, not another send. Finish with `send` without another wait or a final peer handle unless a second publication is intended.

**Stay reachable** only while a joint task is active. Before ending a non-terminal work turn, perform this check: if the harness supports tracked background completion and no collector is live, start exactly one background `pairroom relay wait --timeout 300` in the binding's workspace. For Codex this is required, not a convenience. A linked task worktree can lack that binding; do not rebind merely to make `wait` work. Resolve the tracked result before another collector. Re-hang only after real input or explicit confirmation to continue, never after an empty timeout; cancel at task end. A poll-only harness needs explicit polling in the active turn. Stop parking is bounded, not deep-idle wake; automatic Service wake is a rate-limited, billable Codex fallback, not a substitute for an active collector. An idle peer otherwise needs its next turn or a human nudge; agents never run a printed vendor wake command.

For Grok, review `/hooks`, reload with `r` after installation, and leave folder trust to the user. A readiness hint means run `wait`; clipped replies require explicit complete text, never a truncated prefix or a resend of confirmed text.

On Service failure, run `pairroom daemon status`; ask the user to start it, or run `pairroom daemon install` only with confirmation. Use CLI status/recovery, not private state inspection. Never bypass approvals or run concurrent collectors. A deliberate session change requires explicit `bind --replace`.
