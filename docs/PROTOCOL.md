# Agent protocol

This document defines the minimum collaboration contract the model must understand. Scheduling, permissions, persistence, and cancellation are enforced by code, not by prompt self-discipline. The embedded machine-readable contract is `pairroom-protocol/v6` and is printed by:

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

In embedded Rooms, ordinary Agent answers are always visible to the user. After the native Turn boundary, PairRoom scans visible output for the exact current `peer_handle`:

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

## Native host protocol v7

`pairroom protocol --host-mode native --json` prints `pairroom-protocol/v7`. The embedded v6 contract and envelope remain unchanged. The compact native bootstrap plus stored default collaboration stays within 1,800 UTF-8 bytes; the ordinary envelope overhead remains at most 128 bytes. Native session/transcript references are queried with `relay peer`, never included in an envelope. Missing or inaccessible peer history does not block relay.

An approved Stop hook supplies its official session identity and last assistant message; Claude/Codex use `session_id` and `last_assistant_message`, while Grok file hooks use `sessionId` and `lastAssistantMessage`. PairRoom does not parse vendor transcripts. Exact current peer handles use the same case-insensitive parser and code/URL exclusions as embedded mode. A peer handle wins over `@user`; only `@user` creates a human escalation; no peer/user handle ends relay without recording the private reply body. Minimal publication receipts still make sequence reconciliation possible. User interruption may produce no Stop and no publication. Claude/Grok StopFailure records only an allowlisted failure category, never the partial reply. Grok handles only main-session `reason:end_turn` Stops; cancellation, subagent and observe-only teardown events do not publish or continue a turn. Grok payloads accidentally invoking the Claude compatibility hook are ignored, not relabelled.

`relay send` is a separate explicit path into the same inbox: default target is the peer, `--to @user` escalates, and body mentions never route. It is the attachment path. Automatic publication is idempotent by `(bind_id, generation, report_seq)`; explicit send uses the client message ID within its binding generation. Neither path deduplicates by body. Same-turn send plus a peer-directed Stop creates two independently auditable messages. The bootstrap instructs the Agent to omit the final peer handle after send unless that second full boundary publication is intentional.

Collection transitions `queued → delivering → handed_off`. `handed_off` asserts only that the CLI wrote stdout, not that the native harness injected it or the model accepted it. Missing acknowledgement or collector death becomes `unknown`; explicit Retry creates a new ID after inspecting history and side effects. Cancellation removes only queued work. A replacement binding invalidates old-generation work and cannot undo a handed-off message.

Runtime draining rejects new publications and claims while allowing valid acknowledgements of already released envelopes to settle. An acknowledgement never activates a suspended Room and still requires the current binding, generation, session and receipt; closure, revocation, expired delivery leases and uncertain store writes remain fail-closed.

A hook publishes first, then parks up to 30 seconds within a 45-second installed hook timeout, reserving time for stdout and acknowledgement. No claim occurs while waiting. New inbox work returns `{"decision":"block","reason":"<envelope>"}`. At most eight consecutive actual-message blocks are allowed; `stop_hook_active` with no inbox does not spend a block on empty re-arming. There is no idle wake-up promise after timeout, disabled park or the block cap: messages remain queued for the already-associated session's `relay wait` or a human nudge. Each continued model turn may cost tokens; no real vendor token measurement is claimed.

### Grok hook adaptation

Grok Native keeps the same v7 binding, FIFO and acknowledgement contract. Its file-hook payload clips assistant replies after 32,768 Unicode scalars and caps feedback at 10,000. A clipped Stop reply is not published; a bounded local hint directs the agent to explicitly send/exchange the complete original if needed, without duplicating a prior explicit publication. Missing text is never reconstructed from a transcript.

For Grok inbox delivery the hook's park probes readiness only. It emits a short `decision:block` instruction to run the existing foreground wait; the peer body remains queued and no delivery receipt or acknowledgement is issued. Foreground collection returns the complete original envelope through stdout. Hints share the existing eight-block cap, respect the collector lock and cannot wake an idle session. Grok's own cancellation/continuation/tool-output limits still apply; a stdout receipt is not proof that its tool displayed every character. Update the CLI, Service and skill together.

### Optional foreground exchange

After association, `relay exchange` composes one explicit peer send with foreground collection in the same CLI invocation. It requires a stable client `--id` before publication. These are two operations, not an atomic request/reply transaction: the returned envelope is the next eligible FIFO input, including earlier messages or user steering, not a correlated answer or completion proof. The compact publication receipt goes to stderr; stdout contains only the incoming envelope. The original send idempotency and stdout-before-ack boundary remain authoritative.

Foreground `wait` keeps a 30-second default, accepts finite totals of 1–21,600 seconds, and treats `--timeout 0` as no PairRoom total deadline. `exchange` defaults to 3,600 seconds and accepts the same finite range or `0`. Only successful, explicitly empty responses renew the existing at-most-30-second HTTP long poll inside the CLI. Any authentication, transport, malformed-response, output or acknowledgement error stops collection without retry. No model call, slot-state lock or new durable state is needed to renew a wait. A finite last poll can round up by less than one second and finish its bounded transport/output/ack work; the foreground budget is not a hard task-completion or cost limit. With `0`, caller/native cancellation, transport/auth failure, or process/service shutdown remains authoritative.

A confirmed publication followed by a finite empty exchange timeout exits nonzero and gives receive-only `relay wait` recovery. It never authorizes sending again. An uncertain send keeps the original client ID for inspection/recovery; uncertain collection is never automatically replayed. The CLI holds a process-owned per-slot collector lock across `wait`/`exchange` and rejects a second collector before it publishes or claims; a Stop hook still publishes but does not take the foreground inbox. Do not enable another coordinator for the same pair, and do not use this loop inside Embedded's single-owner turns. Finish with a final explicit send when appropriate rather than waiting for another ceremonial reply, and omit a final peer handle after explicit exchange unless a second Stop publication is intentional. See the [CLI workflow](CLI_REFERENCE.md#foreground-discussion-loop) for entry and recovery.

PairRoom does not own native processes. Owner Turn is advisory, not a workspace lock. Native provider/model/effort/permission selections are metadata, not applied configuration. Native approval and input interruption remain in the original harness. Authenticated multi-round Claude Code ↔ Codex acceptance remains a release gate, separate from synthetic hook tests. Foreground exchange does not establish that a particular native tool can stay pending for an hour, six hours, or indefinitely, or wake an already-idle peer. This additive CLI composition changes no Room mode, protocol/bootstrap byte budget, store schema or automatic Stop-hook behavior.
