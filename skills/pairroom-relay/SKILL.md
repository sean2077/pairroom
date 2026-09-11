---
name: pairroom-relay
description: Use when the user invokes /pairroom-relay, asks to create or join a PairRoom native room, bind this session for cross-agent relay, or tell this agent to discuss something with its peer agent through PairRoom.
---

# pairroom-relay

Bind this native Claude Code / Codex session to a PairRoom Room so the two agents relay through approved project Stop hooks. Prerequisites: `pairroom` on PATH, and the one-time human-approved `pairroom relay install` for each runtime (project hooks + this skill).

## Create a room — `/pairroom-relay <topic>`

```bash
pairroom relay bind --create --name "<topic>"
```

Creates the native Room, binds this session (its Agent slot is resolved from this harness's runtime; `--slot 1|2` overrides), and prints `bind_nonce`, the bootstrap instructions, and the peer's join command. Report that join command to the user for the OTHER session; it does not work in this one.

## Join an existing room

```bash
pairroom relay bind
```

Zero flags inside a recognized native session: resolves the workspace's sole active native Room and this session's slot. Otherwise run the exact join command the creator printed, or pass `--room`/`--slot` from the error's candidate list.

## After bind, once per session

1. Echo the returned `bind_nonce` verbatim in your visible final reply; the approved Stop hook associates this session.
2. Adopt the returned bootstrap/collaboration instructions as this session's relay protocol: handles, `@user`, park windows, publication and recovery rules all come from there, not from this skill.

## Rules

- Slots are Agent 1 / Agent 2 (`--slot 1|2`), never runtimes; the legacy IDs `claude`/`codex` remain accepted.
- After bind, foreground commands need no `--room`/`--slot`: `send`, `wait`, `status`, `peer`, `park`, `nudge`, `reconcile`.
- If bind reports a missing relay hook, run `pairroom relay install --runtime claude|codex` and have the user approve that exact project hook in the harness; installing never grants native trust and never bypasses approval.
- Never read or print `.pairroom/**/credentials`; the CLI owns all secrets.
- On failure, run the recovery command named in the error (for example `bind --replace`); never repeat `--create` after a created-Room failure.
