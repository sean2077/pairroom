---
name: pairroom-relay
description: Use when the user invokes /pairroom-relay, asks to create or join a PairRoom native room, bind this session for cross-agent relay, or tell this agent to discuss or review a problem with its peer through PairRoom.
---

# pairroom-relay

Run these commands through THIS native Claude Code / Codex CLI or Desktop session's tools, not a separate terminal or another agent. PairRoom owns the Room and message transport, not your process, provider, model, permissions, worktrees or subagents. Prerequisites: `pairroom` on PATH and one-time human approval of the project hooks installed by `pairroom relay install`.

## Create a room — `/pairroom-relay <topic>`

```bash
pairroom relay bind --create --name "<topic>"
```

Creates the native Room, binds this session, and prints `bind_nonce`, bootstrap instructions and the peer's join command. Give the join command to the OTHER session; do not run it yourself. The current workspace and recognized harness supply defaults; omitted pair settings keep the Service's configured pair, not a guessed provider/model.

## Join or resume

```bash
pairroom relay bind
```

For an already-associated native session, its session metadata selects the existing Room/slot and saved Service endpoint, even when a Desktop process serves multiple sessions. Otherwise resolve the workspace's sole active native Room and its matching runtime slot. Only a real ambiguity requires the exact join command or explicit candidate flags. Never copy credentials or invent session IDs. A Grok caller is identified and rejected explicitly: Native Grok is not implemented; do not impersonate Claude. Embedded Grok support is separate.

## After a new bind, once

1. Echo `bind_nonce` verbatim in your visible final reply; the approved Stop hook associates the official session ID. Environment/PID discovery alone never associates it.
2. Adopt the returned bootstrap/collaboration instructions. A resumed association needs no new nonce echo or process restart. Before association, `pairroom relay status --brief` can inspect this pending binding without inbox access; do not wait or exchange until the nonce is echoed.

## Joint review

Reuse the associated Room and exact workspace/revision. Challenge material assumptions with evidence and counterexamples; exchange new findings and necessary context, not repeated transcripts. Do not create a worktree or subagent hierarchy just to exchange opinions. Stop when known material objections are resolved or require a user decision; agreement is not proof. Review completion does not authorize implementation. After the user assigns execution, leave tools, permissions and subagents to that native harness, and involve the peer only when useful or requested.

## Optional foreground discussion

After BOTH Stop-hook associations complete, inspect `pairroom relay exchange --help` for supported flags. Update binary and skill together; older binaries may lack exchange, long waits or `--brief`.

Start one participant with `pairroom relay wait --timeout 0` when the native tool can safely stay pending. The other sends focused text and waits in one call (default one hour):

```bash
pairroom relay exchange --id <new-client-id> --text "<question or findings>"
```

`--timeout 0` removes PairRoom's total deadline; finite waits allow up to 21600 seconds. Plain `wait` defaults to 30 seconds. Each NEW message needs a fresh ID; an uncertain publication retains its original ID and content. Exchange returns the next FIFO input, possibly earlier input or user steering, not necessarily a correlated reply. Read the sender and body. The CLI renews empty HTTP waits internally without model polling. Keep the existing tool pending; a second collector is rejected before exchange publishes. Never start a competing collector when a native tool is backgrounded. Respect native cancellation and tool limits.

For diagnosis, start with `pairroom relay status --brief`; full `status` returns history and is only needed for targeted investigation. A finite confirmed-publication timeout requires `relay wait`, not another send/exchange. Transport, output or acknowledgement errors require inspection before recovery; never automatically replay uncertain work. Finish with `relay send`, not another ceremonial wait. After explicit send/exchange, omit the final peer handle unless a second Stop publication is intended. Stop hooks still publish while a foreground collector is active, but do not steal its input. No command wakes an already-idle peer or bypasses native approval.

## Rules

- Slots are Agent 1 / Agent 2 (`--slot 1|2`), never runtimes; durable IDs `claude`/`codex` remain accepted.
- Associated foreground commands normally need no Room/slot flags. A unique pending binding is diagnosable with `status --brief` the same way; send/wait/exchange still require the nonce. Session metadata wins over shared-PID inference; conflicting/unmatched metadata must not select a different session.
- Missing hooks: run `pairroom relay install` in the intended session and have the user approve the exact project hook. Installing never grants trust. Explicit `--runtime claude|codex` can prepare peer hooks; never use it to relabel the session being bound.
- Never read or print `.pairroom/**/credentials`; the CLI owns secrets.
- Follow the recovery named in the error; never repeat `--create` after a created-Room failure.
