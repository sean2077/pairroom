---
name: pairroom-relay
description: Use when the user invokes /pairroom-relay, asks to create or join a PairRoom native room, bind this session for cross-agent relay, or tell this agent to discuss or review a problem with its peer through PairRoom.
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

## When the user asks for joint review

Reuse the associated Room for follow-up reviews unless the user requests another. Review the same problem and exact workspace/revision; do not create a worktree or subagent hierarchy just to exchange opinions. Challenge material assumptions with evidence and counterexamples. Keep replies focused on new findings and necessary context, not repeated transcripts. Stop when known material objections are resolved or need a user decision; agreement is not proof. Review completion does not authorize implementation. Once the user assigns execution, leave tools, permissions and subagents to that native harness, and involve the peer again only when useful or requested.

## Optional foreground discussion

After BOTH Stop-hook associations complete, use `pairroom relay exchange --help` to check the installed CLI. An older binary may not support exchange; retain its documented send/wait path rather than guessing flags. This is a Native-only tool-call loop, not a new Room mode.

Start one participant collecting with `pairroom relay wait --timeout 600`. The other sends focused text and waits in one invocation:

```bash
pairroom relay exchange --id <new-client-id> --text "<question or findings>" --timeout 600
```

Use a fresh ID for each new message; keep that ID and identical content for an uncertain publication. Exchange sends once, then returns the next FIFO input, which may be user steering or an earlier message, NOT necessarily a reply to this send. Read its sender and content. The CLI renews empty waits internally: keep the native tool pending rather than starting model-driven polling or concurrent collectors for the same slot. Respect native cancellation and tool timeouts.

A confirmed-publication timeout means collect with `relay wait`, not resend/exchange. For any transport, output or acknowledgement error, inspect `relay status` and follow recovery guidance; do not automatically replay uncertain work. To finish, use `relay send` for the final peer-facing result and stop without another wait. After explicit send/exchange, omit the final peer handle unless a second full Stop publication is intentional. Ordinary Stop-hook relay remains available; no foreground command wakes an already-idle peer or bypasses hook approval, association or permissions.

## Rules

- Slots are Agent 1 / Agent 2 (`--slot 1|2`), never runtimes; the legacy IDs `claude`/`codex` remain accepted.
- After bind, foreground commands need no `--room`/`--slot`: `send`, `exchange`, `wait`, `status`, `peer`, `park`, `nudge`, `reconcile`.
- If bind reports a missing relay hook, run `pairroom relay install --runtime claude|codex` and have the user approve that exact project hook in the harness; installing never grants native trust and never bypasses approval.
- Never read or print `.pairroom/**/credentials`; the CLI owns all secrets.
- On failure, run the recovery command named in the error (for example `bind --replace`); never repeat `--create` after a created-Room failure.
