# Agent protocol

This document defines the minimum collaboration contract the model must understand. Scheduling, permissions, persistence, and cancellation are enforced by code, not by prompt self-discipline. The current machine-readable contract is `pairroom-protocol/v6` and is printed by:

```bash
pairroom protocol --json
```

## Bootstrap

Each native session receives a compact stable bootstrap plus the Room's stored, versioned collaboration instructions and any per-Agent additional instructions. Default mode provides flexible Lead / Executor responsibilities; custom mode inserts the supplied prose instead, without default responsibilities. It identifies the Agent's current public display name and exact mention handle, explains single-Turn ownership, and asks the Agent to mention its peer only when another response is genuinely necessary. Claude Code and Codex use their native instruction layers. A new Grok ACP session receives the rules through `_meta.rules`; an exactly loaded Grok session receives the current bootstrap once in its first PairRoom prompt instead of replacing its native system prompt.

## Input envelope

Every native Turn or steer receives a dynamic envelope, for example:

```text
[PairRoom message]
from: @codex

Implemented the change; tests passed. @claude Please review the diff.
```

When attachments are present, a compact `attachments:` list between `from` and the body carries quoted filename, media type, and adapter-only local path. Binary image parts continue through native transport when supported. When Grok ACP does not advertise image input, PairRoom still validates the attachments and delivers the complete text envelope with local paths, plus an explicit notice that no visual content was sent. Grok may use a permitted native image-reading tool or ask `@user` for missing details; the fallback does not claim image understanding. The complete body remains unchanged, including its whitespace; relay never summarizes a peer response or appends accumulated Room history.

When the current user message explicitly replies to another message in the same Room, a `quoted_message:` block follows any attachments and precedes the body. It carries the quoted sender handle and complete original text, both Go-quoted so embedded newlines cannot form new envelope fields. Quoted images are merged into the current `attachments:` list by canonical ID without duplicating files already on the input. Agent `ReplyTo` values remain correlation links and are not expanded. Unknown quoted IDs fail closed before persistence. The quoted text is never summarized.

Message ID, Thread ID, ReplyTo, native request/session IDs, delivery intent, and protocol version remain available to transport, Event Log, and diagnostics where applicable. They are not repeated as model-facing envelope fields. Self/peer identity and collaboration responsibility live at the instruction layer, not in `self_handle`, `peer_handle`, or `current_role` per turn. Static contract checks cap the ordinary envelope overhead at 128 bytes (excluding body, media, and quoted-message text) and the bootstrap plus default collaboration at 1,800 bytes. Custom instructions have a separate 16 KiB UTF-8 input limit; these byte budgets are not token-billing claims.

The Agent should treat repository state as authoritative and independently verify peer claims. A transport receipt or another Agent's assertion is not execution evidence.

## Output routing

Ordinary Agent answers are always visible to the user. After the native Turn boundary, PairRoom scans visible output for the exact current `peer_handle`:

- unique runtime: `@claude`, `@codex`, or `@grok`;
- duplicated runtime: stable slot-order handles such as `@claude0` and `@claude1`.

Matching is case-insensitive. An unsuffixed duplicated-runtime handle is ambiguous, produces a visible warning, and does not route. Mentions inside fenced code, inline code, URLs, and email addresses are ignored. A self-handle does not route.

An exact Agent handle in the same response wins over `@user`. `@user` alone returns the decision to the human. Without the exact peer handle, Agent relay ends and either Agent's answer may be the final result.

The removed aliases `@driver`, `@reviewer`, `@lead`, `@executor`, `@peer`, `@human`, `@all`, `@agent1`, and `@agent2` have no routing meaning. In Agent output they remain ordinary visible text; an otherwise unaddressed user send that relies on one is rejected instead of silently falling back to Agent 1. Removed `PAIRROOM:HANDOFF`, `PAIRROOM:NEXT`, `PAIRROOM:DONE`, `PAIRROOM:WAIT`, and `PAIRROOM:BLOCKED` markers are ordinary visible text. No fixed handoff format is accepted or required.

## Convergence

There is no PairRoom relay counter or automatic circuit breaker. Agents must omit the peer handle after delivering a complete answer, and must not mention the peer merely to acknowledge, agree, thank, or return the Turn ceremonially. A continued relay should exist only because an independent response can materially change or complete the result.

The user remains the active circuit breaker: Cancel removes queued work, Interrupt stops the current native Turn, and a newer instruction cancels stale not-yet-started Agent relays.

## Creation-time collaboration contract

`default` uses the least coordination needed for an accurate result. For simple, low-risk tasks, the addressed Agent executes, verifies, and answers directly, without delegation or peer review. Otherwise, Lead (Agent 1) focuses on planning, decisions, and review; Executor (Agent 2) implements, verifies, and contributes technical feedback. Complexity, uncertainty, or risk can justify involving the peer; the responsibilities are defaults, not a mandatory sequence. Newer human instructions take precedence. `custom` uses the human's natural-language rules without adding these defaults.

New Rooms use collaboration version 2 unless an explicit supported version is supplied. Version-1 and custom instructions remain readable and are injected unchanged on activation; upgrading PairRoom never rewrites an existing Room's policy. Create a new Room to adopt the new default, or give a newer human instruction for the current task. The relay protocol remains `pairroom-protocol/v6`.

The instructions do not grant tools or force a particular number of Turns. Native permission profiles remain independent; both modern participants use the live workspace. Retired Rooms are rejected; no role-specific instruction fallback is generated. No public role-change operation or role-based addressing remains.

## Authority

```text
user decision
  > repository and native runtime facts
  > durable PairRoom state
  > peer message
  > model inference
```
