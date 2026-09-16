---
name: pairroom-relay
description: Create or join a PairRoom Room and collaborate with another native session.
---

# PairRoom relay

Run commands through this session's tools in the project's Git workspace.
Reuse its Room for follow-up work unless the user asks for a new one.

- **Create** (`/pairroom-relay <topic>`): `pairroom relay bind --create --name "<topic>"`. If preflight says this runtime matches no Service-pair slot, retry once with `--peer-runtime claude|codex|grok`; no Room was created, so do not inspect private state. For a peer on the same machine, Service data root, and workspace, give `pairroom relay bind --room <id> --slot <n>`; otherwise give the returned join command.
- **Slot vocabulary**: durable Room and relay slots are `slot1` and `slot2`; use `--slot 1|2` or `agent1|agent2`. `claude` and `codex` are accepted only as CLI input aliases and are never durable identities.
- **Join**: `pairroom relay bind`. Use the returned candidate guidance when a Room or slot is ambiguous.
- **Collaborate**: follow the bind result's protocol and collaboration instructions. Do simple work directly; involve the peer when useful.
- **Discuss in a tool call**: the receiver runs `pairroom relay wait`; the sender runs `pairroom relay exchange --id <stable-id> --text "<message>"`. Reuse an ID only for the same publication; after a confirmed send times out, collect with `wait` rather than sending again.
- **Stay reachable while a joint task is active**: in a harness that wakes an idle session when tracked background work completes, end each turn with exactly one background `pairroom relay wait` (`--timeout` at most 300) pending. It owns the sole collector — the Stop hook still publishes but never claims; resolve the wait's result before the next `wait`/`exchange`. Re-hang only after real peer input or an explicit confirmation that the joint task continues; never auto-re-hang on an empty timeout; cancel at task end or Room exit. In a poll-only harness, a held wait is reachable only by explicit polling within the same active turn; the only automatic continuation PairRoom promises is the bounded Stop-park window at each turn end, never a deep-idle wake. A message queued to an idle peer waits for its next turn or a human nudge; when the CLI provides a human-executable vendor wake template, agents never run it themselves.

If the Service is unavailable, run `pairroom daemon status`, then tell the user to wait for it or, only after confirmation, run `pairroom daemon install`; do not expand diagnosis with help or state inspection.

Follow setup and recovery errors instead of bypassing approvals or retrying `--create` except for the documented preflight mismatch. A different session requires explicit `bind --replace`. Never run two relay collectors in one session; resolve a woken background wait before opening a foreground collector.
Use the CLI for status and recovery; never inspect or expose its private state.

Follow runtime-specific hook guidance; publish complete text explicitly when a reply is clipped, and collect with `wait` when prompted.
