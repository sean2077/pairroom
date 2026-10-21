---
name: pairroom-relay
description: Create, join, resume, or relay a PairRoom Native Room from the current agent session.
---
# pairroom-relay

Use this session's tools; reuse the Room, follow its collaboration rules. Do not rebind on cwd changes. Recover through the CLI, not private state; never bypass approvals. Install the daemon only with confirmation.

## Select the route

Read only the matching reference before its operation. Paths are relative to this skill directory, not the tool cwd; do not preload references.

- Enterprise origin/deployment, `--enterprise-service`, or an enterprise Binding: [Enterprise](references/enterprise.md), before local preflight.
- LAN invitation, sharing or admission: [LAN](references/lan.md).
- Current Runtime is Grok: [Grok](references/grok.md); Gemini: [Gemini](references/gemini.md).

## Local create or join

Before first local bind or on local setup failure run read-only `pairroom relay preflight`; follow `next_steps` until ready. Join intent outranks preflight create guidance.

- Create only when requested: `pairroom relay bind --create --name "<topic>"`. No Runtime types. Use `--slot` only on request; recover, never repeat `--create`.
- Display `peer_join_local` (same workspace) or `peer_join` unchanged to the user.
- Given a join command, run it unchanged with all selectors, including `--service-file`, `--repo` and `--slot`. A bare Room ID means `pairroom relay bind --room <exact-id>`; omit `--slot` unless requested and let the CLI pick. Never guess, create or replace implicitly. A Room ID is not a LAN invitation.
- Slots are `1|2`, not runtimes. Bind is ready; no nonce, turn/status check or Provider setup.

## Relay

On a new join to a bound peer, announce once via `pairroom relay send`; no wait or final peer handle. Skip on resume or binding first.

Use v9 `@agent1` / `@agent2`; legacy Rooms use printed handles. An addressed final reply publishes automatically. For an explicit publication use `pairroom relay send`; omit a final peer handle unless intentionally publishing twice. Reuse IDs only for identical publication content; changed evidence needs a new ID.

Receive with `pairroom relay wait`. Keep one collector only; resolve it before another. During work, one foreground wait/exchange within the tool limit is enough; an empty timeout ends the turn. Confirmed send plus timeout: receive-only wait, never resend. No idle background wait/poll unless [waiting guidance](references/waiting.md) confirms tracked completion; also read it for idle-wake diagnosis.

Read full-output files when shown only a preview; stdout/handed_off is not model acceptance. Before file transfer, referenced-file reads or evidence review, read [files and review](references/files-and-review.md). On uncertainty, failure, `peer_changed`, or a requested replacement, stop and read [recovery](references/recovery.md) before another effect; status/reconcile only on uncertainty/failure.
