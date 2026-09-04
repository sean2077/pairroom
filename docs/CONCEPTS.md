# Core concepts

## Project, Room, and Binding

**Project** is the registration record of a local Git repository in the Management Service. It stores the repository location and the Rooms that can be created there. It is not a copy of the repository and does not own user source.

**Room** is the persistence boundary for collaboration. It holds participant roles, messages, Turn summaries, approvals, Workflow, Binding, and the Event Log. A Room can continue across process starts, but the native process itself is not restored with the Event Log.

**Binding** binds a Room slot participant to a vendor-native session/thread for the selected runtime. Durable ActorIDs `claude` and `codex` identify Agent 1 and Agent 2; `RuntimeKind` selects Claude Code, Codex, or Grok Build. Binding is managed by the Service and cannot be reused freely by another active Room.

## Message and native Turn

A Message is PairRoom's auditable input or output. A native Turn is one complete round actually executed by Claude Code, Codex, or Grok Build. They are not one-to-one:

- one Turn can receive multiple steering messages;
- a queued message may start a new Turn later;
- successful transport delivery does not mean the Turn completed;
- an ordinary diagnostic error does not necessarily mean the Turn terminated.

PairRoom releases the owner only at a reliable boundary, such as native `turn/completed`, a confirmed process exit, or an explicit cancel / abort termination event.

## Deterministic relay

A Room uses the single `turns` policy:

```text
idle
  -> reserve FIFO item
  -> submit to Agent A
  -> Agent A owns the native Turn
  -> reliable terminal boundary
  -> release owner
  -> reserve next FIFO item
```

Invariants:

1. A Room has at most one active native Turn owner at a time;
2. Cross-Agent input can be submitted only after the current Turn ends;
3. A FIFO item is checked for cancellation again before submit, avoiding a ghost Turn;
4. A generic runtime error only updates diagnostics and does not hand off by itself;
5. A newer user instruction takes priority over an older automatic relay.

## Message intent

- `append`: prefer steering the target Agent's active Turn; enter a later boundary when that is unsafe;
- `next_turn`: explicitly request a new native Turn, even if the target is still the current Agent;
- `supersede`: interrupt or replace in-flight input for the target Agent; the actual scope is constrained by the native harness interrupt semantics.

## Handoff and control markers

An explicit `@claude`, `@codex`, or `@peer` in Agent output is a routing instruction to that peer slot (`claude` is Agent 1, `codex` is Agent 2). `@human` or `@user` means the user must decide and automatic relay stops. When a human asks both Agents to interact, the active Agent must write that address; speaking only to the human does not start the other Agent. Implicit automatic handoff without an explicit peer address requires:

```text
[PAIRROOM:HANDOFF]
Goal / Scope / Evidence / Risks / Exact ask
[/PAIRROOM:HANDOFF]
[PAIRROOM:NEXT]
```

Convergence markers:

- `NEXT`: without an explicit peer address, hand to the peer after a valid handoff; an explicit `@peer` address itself requests a peer Turn;
- `DONE`: without a direct peer address, the current chain is complete and returns to the user; a direct peer address still hands off;
- `WAIT`: a user decision is required;
- `BLOCKED`: there is an unresolved external block.

The maximum hop count limits unbounded round-trips. It is not a mechanical A/B rotation count.

## Role and Workspace

- **Driver**: implements in the live workspace;
- **Reviewer**: inspects in an isolated reviewer snapshot and does not modify the Driver's live tree by default;
- **Peer**: an ordinary participant without Driver / Reviewer privilege.

Roles are runtime permission and workspace boundaries, not just prompt labels. Switching roles must happen at a safe Turn boundary.

## Workflow and Approval

A natural-language actor/action sequence can compile into Workflow stages such as plan, review, execute, and audit. Workflow still reuses the same FIFO and single-owner scheduler.

Approval binds to an explicit Agent, native request, and plan revision. After a process restart, the vendor request ID is no longer reliable, so a pending approval expires instead of replaying automatically.

## Restart and fail-closed

The Event Log, Binding, and historical messages are durable state. The native process, current owner, and Room FIFO are process state. On restart:

- in-flight / queued input is marked skipped, cancelled, or failed;
- pending approvals expire;
- input that may already have had side effects is not replayed automatically;
- the user inspects repository state and then retries explicitly.

This is a safety choice against duplicate execution, not a persistent-queue promise.
