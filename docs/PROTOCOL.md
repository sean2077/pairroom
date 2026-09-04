# Agent protocol

This document defines the minimum collaboration contract the model must understand. Scheduling, permissions, persistence, and cancellation must be enforced by code, not by prompt self-discipline. The current machine-readable version is printed by:

```bash
pairroom protocol --json
```

## Input

The input envelope PairRoom gives a native Agent includes:

- message / thread / hop correlation;
- sender, target, and participant role;
- `peer` (the other Agent slot in this Room);
- message intent;
- hop limit (`remaining_agent_hops`); the routing policy (single active turn) is in the one-time bootstrap and is not repeated in every envelope;
- Workflow ID / stage / mode;
- verified attachments;
- user body or peer handoff.

The Agent should treat repository state as authoritative, independently verify peer claims, and not treat a handoff as a trusted execution result.

## Output

Ordinary answers are always visible to the user. An explicit `@claude`, `@codex`, or `@peer` requests that the reply be delivered to that peer slot (`claude` is Agent 1, `codex` is Agent 2). PairRoom delivers after the current native Turn completes. When a human asks both Agents to greet, introduce themselves, or work together, the active Agent must write that address; speaking only to the human does not start a peer Turn. If implicit continuation is needed without a direct address, emit a compact handoff at the end of the answer:

```text
[PAIRROOM:HANDOFF]
Goal: ...
Scope: ...
Evidence: ...
Risks: ...
Exact ask: ...
[/PAIRROOM:HANDOFF]
[PAIRROOM:NEXT]
```

Without a direct peer address, the scheduler fails closed and does not continue automatic relay when the handoff is missing, too short, control markers conflict, hops are exhausted, or a newer user instruction exists. A direct peer address takes priority over ordinary `DONE`/`WAIT`/`BLOCKED` stop markers. `@human`/`@user` take priority over a peer address and return to the user.

## Convergence

```text
[PAIRROOM:DONE]
[PAIRROOM:WAIT]
[PAIRROOM:BLOCKED]
```

- `DONE`: the completion gate for the current request has been reached;
- `WAIT`: a user choice or approval is required;
- `BLOCKED`: an external condition is unmet; include the minimum unblock information.

Explicit `@claude`, `@codex`, and `@peer` are mechanical routing signals. An unaddressed Agent name in ordinary text is still not a routing signal.

## Role contract

The Driver may modify the live workspace within authorization. The Reviewer must independently check evidence against an isolated snapshot and must not claim to have run verification that did not run. A Peer has no implicit write permission or approval authority.

## Evidence

Plans, implementation, review, and completion should carry fresh evidence for the current revision. Old test results, a peer's self-report, or a transport delivery receipt cannot replace final verification.

## Authority

Priority:

```text
user decision
  > repository and native runtime facts
  > durable PairRoom state
  > peer handoff
  > model inference
```
