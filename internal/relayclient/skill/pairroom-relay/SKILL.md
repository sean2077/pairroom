---
name: pairroom-relay
description: Create or join a PairRoom Room and collaborate with another native session.
---

# PairRoom relay

Run commands through this session's tools in the project's Git workspace.
Reuse its Room for follow-up work unless the user asks for a new one.

- **Create** (`/pairroom-relay <topic>`): `pairroom relay bind --create --name "<topic>"`. If preflight says this runtime matches no Service-pair slot, retry once with `--peer-runtime claude|codex|grok`; no Room was created, so do not inspect private state. For a peer on the same machine, Service data root, and workspace, give `pairroom relay bind --room <id> --slot <n>`; otherwise give the returned join command.
- **Join**: `pairroom relay bind`. Use the returned candidate guidance when a Room or slot is ambiguous.
- **Collaborate**: follow the bind result's protocol and collaboration instructions. Do simple work directly; involve the peer when useful.
- **Discuss in a tool call**: the receiver runs `pairroom relay wait`; the sender runs `pairroom relay exchange --id <stable-id> --text "<message>"`. Reuse an ID only for the same publication; after a confirmed send times out, collect with `wait` rather than sending again.

If the Service is unavailable, run `pairroom daemon status`, then tell the user to wait for it or, only after confirmation, run `pairroom daemon install`; do not expand diagnosis with help or state inspection.

Follow setup and recovery errors instead of bypassing approvals or retrying `--create` except for the documented preflight mismatch. A different session requires explicit `bind --replace`.
Use the CLI for status and recovery; never inspect or expose its private state.

Follow runtime-specific hook guidance; publish complete text explicitly when
a reply is clipped, and collect with `wait` when prompted.
