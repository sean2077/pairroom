# Agent protocol

This document defines the minimum collaboration contract the model must understand. Scheduling, permissions, persistence, and cancellation are enforced by code, not by prompt self-discipline. The current machine-readable contract is `pairroom-protocol/v5` and is printed by:

```bash
pairroom protocol --json
```

## Bootstrap

Each native session receives a compact stable bootstrap. It identifies the Agent's current public display name and exact mention handle, explains single-Turn ownership, and asks the Agent to mention its peer only when another response is genuinely necessary. Claude Code and Codex use their native instruction layers. A new Grok ACP session receives the rules through `_meta.rules`; an exactly loaded Grok session receives the current bootstrap once in its first PairRoom prompt instead of replacing its native system prompt.

## Input envelope

Every native Turn or steer receives one dynamic `[PairRoom message]` envelope containing:

- protocol, Message ID, and Thread ID;
- `from_handle`, `self_handle`, and `peer_handle`;
- optional `reply_to`;
- `current_role`;
- verified attachment metadata and adapter-only local paths;
- the complete current message body.

Hop counters, remaining-turn budgets, Workflow fields, delivery intent, and redundant transport metadata are not part of the envelope. Agent-to-Agent delivery contains the complete visible peer response and attachments, not a summary or accumulated Room history.

The Agent should treat repository state as authoritative and independently verify peer claims. A transport receipt or another Agent's assertion is not execution evidence.

## Output routing

Ordinary Agent answers are always visible to the user. After the native Turn boundary, PairRoom scans visible output for the exact current `peer_handle`:

- unique runtime: `@claude`, `@codex`, or `@grok`;
- duplicated runtime: stable slot-order handles such as `@claude0` and `@claude1`.

Matching is case-insensitive. An unsuffixed duplicated-runtime handle is ambiguous, produces a visible warning, and does not route. Mentions inside fenced code, inline code, URLs, and email addresses are ignored. A self-handle does not route.

`@user` overrides every Agent handle in the same response. Without `@user` or the exact peer handle, Agent relay ends and either Agent's answer may be the final result.

The removed aliases `@peer`, `@human`, `@all`, `@agent1`, and `@agent2` have no routing meaning. In Agent output they remain ordinary visible text; an otherwise unaddressed user send that relies on one is rejected instead of silently falling back to the Driver. Removed `PAIRROOM:HANDOFF`, `PAIRROOM:NEXT`, `PAIRROOM:DONE`, `PAIRROOM:WAIT`, and `PAIRROOM:BLOCKED` markers are ordinary visible text. No fixed handoff format is accepted or required.

## Convergence

There is no PairRoom relay counter or automatic circuit breaker. Agents must omit the peer handle after delivering a complete answer, and must not mention the peer merely to acknowledge, agree, thank, or return the Turn ceremonially. A continued relay should exist only because an independent response can materially change or complete the result.

The user remains the active circuit breaker: Cancel removes queued work, Interrupt stops the current native Turn, and a newer instruction cancels stale not-yet-started Agent relays.

## Role contract

The Driver may modify the live workspace within authorization. The Reviewer independently checks evidence against an isolated snapshot and must not claim verification that did not run. A Peer is an equal collaborator but gains no implicit write permission or approval authority.

## Authority

```text
user decision
  > repository and native runtime facts
  > durable PairRoom state
  > peer message
  > model inference
```
