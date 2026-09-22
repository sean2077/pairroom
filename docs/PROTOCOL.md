# Agent protocol

This document defines the minimum collaboration contract the model must understand. Scheduling, permissions, persistence, and cancellation are enforced by code, not by prompt self-discipline. The embedded machine-readable contract is `pairroom-protocol/v7` and is printed by:

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

New Rooms use collaboration version 2 unless an explicit supported version is supplied. Current-schema custom instructions remain readable and are injected unchanged on activation; upgrading PairRoom never rewrites an existing Room's policy. Create a new Room to adopt the new default, or give a newer human instruction for the current task. The embedded protocol remains `pairroom-protocol/v7`.

The instructions do not grant tools or force a particular number of Turns. Native permission profiles remain independent; both modern participants use the live workspace. Retired Rooms are rejected; no role-specific instruction fallback is generated. No public role-change operation or role-based addressing remains.

## Authority

```text
user decision
  > repository and native runtime facts
  > durable PairRoom state
  > peer message
  > model inference
```

## Native host protocol v8

`pairroom protocol --host-mode native --json` prints `pairroom-protocol/v8`; embedded mode prints `pairroom-protocol/v7`. The compact native bootstrap plus stored default collaboration stays within 1,800 UTF-8 bytes; the ordinary envelope overhead remains at most 128 bytes. Native session/transcript references are queried with `relay peer`, never included in an envelope. Missing or inaccessible peer history does not block relay.

Association is captured at bind from the official `session_id` the harness exposes to its tool-call environment (Claude Code `CLAUDE_CODE_SESSION_ID`, Codex `CODEX_SESSION_ID`, Grok `GROK_SESSION_ID`); there is no nonce echo, and a bind run outside that environment fails closed. An approved Stop hook then supplies the same official `session_id` and `last_assistant_message` at each response boundary, re-confirming that identity (a mismatch fails closed) and recording the transcript path the environment does not carry; PairRoom does not parse vendor transcripts. Exact current peer handles use the same case-insensitive parser and code/URL exclusions as embedded mode. A peer handle wins over `@user`; only `@user` creates a human escalation; no peer/user handle ends relay without recording the private reply body. Minimal publication receipts still make sequence reconciliation possible. User interruption may produce no Stop and no publication. Claude/Grok StopFailure records only an allowlisted failure category, never the partial reply.

`relay send` is a separate explicit path into the same inbox: default target is the peer, `--to @user` escalates, and body mentions never route. It is the attachment path. Automatic publication is idempotent by `(bind_id, generation, report_seq)`; explicit send uses the client message ID within its binding generation. Neither path deduplicates by body. Same-turn send plus a peer-directed Stop creates two independently auditable messages. The bootstrap instructs the Agent to omit the final peer handle after send unless that second full boundary publication is intentional.

Collection transitions `queued → delivering → handed_off`. `handed_off` asserts only that the CLI wrote stdout, not that the native harness injected it or the model accepted it. Missing acknowledgement or collector death becomes `unknown`. While no explicit Retry is pending, the original claimer's receipt-matched acknowledgement still settles an `unknown` delivery to `handed_off` — the per-claim receipt was issued only to that collector — and a pending Retry blocks that late acknowledgement; otherwise explicit Retry creates a new ID after inspecting history and side effects. Cancellation removes only queued work. A replacement binding invalidates old-generation work and cannot undo a handed-off message.

Runtime draining rejects new publications and claims while allowing valid acknowledgements of already released envelopes to settle. An acknowledgement never activates a suspended Room and still requires the current binding, generation, session and receipt; closure, revocation and uncertain store writes remain fail-closed, and an expired delivery lease settles only through that receipt-matched acknowledgement.

A hook publishes first, then parks up to 30 seconds within a 45-second installed hook timeout, reserving time for stdout and acknowledgement. No claim occurs while waiting. For Claude/Codex, new inbox work returns `{"decision":"block","reason":"<envelope>"}`. At most eight consecutive actual-message blocks are allowed; `stop_hook_active` with no inbox does not spend a block on empty re-arming. There is no idle wake-up promise after timeout, disabled park or the block cap: messages remain queued for the already-associated session's `relay wait`, a human nudge, or — for an eligible Claude/Codex-bound target in a wake-enabled Room — the Service-side automatic wake below. Each continued model turn may cost tokens; no real vendor token measurement is claimed.

### Optional foreground exchange

After association, `relay exchange` composes one explicit peer send with foreground collection in the same CLI invocation. It requires a stable client `--id` before publication. These are two operations, not an atomic request/reply transaction: the returned envelope is the next eligible FIFO input, including earlier messages or user steering, not a correlated answer or completion proof. The compact publication receipt goes to stderr; stdout contains only the incoming envelope. The original send idempotency and stdout-before-ack boundary remain authoritative.

Foreground `wait` and `exchange` default to 3,600 seconds, accept finite totals of 1–21,600 seconds, and treat `--timeout 0` as no PairRoom total deadline. Only successful, explicitly empty responses renew the existing at-most-30-second HTTP long poll inside the CLI. Any authentication, transport, malformed-response, output or acknowledgement error stops collection without retry. No model call, slot-state lock or new durable state is needed to renew a wait. A finite last poll can round up by less than one second and finish its bounded transport/output/ack work; the foreground budget is not a hard task-completion or cost limit. With `0`, caller/native cancellation, transport/auth failure, or process/service shutdown remains authoritative.

A confirmed publication followed by a finite empty exchange timeout exits nonzero and gives receive-only `relay wait` recovery. It never authorizes sending again. An uncertain send keeps the original client ID for inspection/recovery; uncertain collection is never automatically replayed. The CLI holds a process-owned per-slot collector lock across `wait`/`exchange` and rejects a second collector before it publishes or claims; a Stop hook still publishes but does not take the foreground inbox. Do not enable another coordinator for the same pair, and do not use this loop inside Embedded's single-owner turns. Finish with a final explicit send when appropriate rather than waiting for another ceremonial reply, and omit a final peer handle after explicit exchange unless a second Stop publication is intentional. See the [CLI workflow](CLI_REFERENCE.md#foreground-discussion-loop) for entry and recovery.

PairRoom does not own native processes. Owner Turn is advisory, not a workspace lock. Native provider/model/effort/permission selections are metadata, not applied configuration. Native approval and input interruption remain in the original harness. Authenticated multi-round Claude Code/Codex/Grok acceptance remains a release gate, separate from synthetic hook tests. Foreground exchange does not establish that a particular native tool can stay pending for an hour, six hours, or indefinitely, or wake an already-idle peer. This additive CLI composition changes no Room mode, protocol/bootstrap byte budget, store schema or automatic Stop-publication identity.


### Automatic idle-peer wake

A wake-enabled Room (default on; changeable only at an idle Room boundary through Management, never with relay credentials) may nudge an existing Claude Code or Codex session after a durably queued input. One burst produces at most one fixed body-free nudge, rate-limited per Room (minimum interval 60 s per receiver, shared Room hourly cap 10), durably reserved by transport message ID before the effect and never automatically retried. The reservation rechecks the current binding generation, policy, queue and live collectors. A foreground/park collector or in-flight delivery suppresses wake; a collector arriving after reservation may make one nudge redundant. The FIFO alone determines message delivery.

Codex uses `codex queue`. Claude uses the official session inbox captured at confirmed bind/Stop, loaded from a private workspace sidecar matching bind ID, generation and session. Missing, stale or insecure capability means `suppressed/capability_unavailable`; Unix socket/local Windows named-pipe failures mean `failed/socket_failed`, `socket_timeout` or `socket_cancelled`. Successful socket writes mean **`submitted`**, not an acceptance acknowledgement, policy approval, a new model turn, or `handed_off`. Claude `crossSessionInbound` can hold/refuse input; PairRoom never changes it. Both success outcomes (`accepted` for the Codex command, `submitted` for the Claude socket) have no reason; failed/suppressed outcomes require an allowlisted reason. Replay enforces the same vocabulary.

The Event Log contains only `native.wake.updated`, `native.wake.reserved` and `native.wake.attempted` facts with fixed outcomes/reasons, not inbox paths/tokens, vendor output, thread identity or task text. No body enters the vendor wake channel. Failed/unavailable wake leaves input queued for `relay wait` or a human nudge. Grok retains its harness-owned background wait; no external Grok wake is integrated. See [automatic wake](design/auto-wake.md) and [Claude inbox security and setup](design/claude-inbox-wake.md).


### Grok hook boundaries

Grok file hooks use `sessionId`, `lastAssistantMessage` and `stopHookActive`.
Only main-session `Stop` with `reason: end_turn` publishes; cancellation,
subagent and observe-only teardown events are inert, as is the duplicate
invocation through Grok's Claude-compatible hook sources. The hook checks the
identity captured at bind; it cannot create or change a binding.

Grok clips outgoing hook text and Stop feedback. A clipped reply is never
published as complete: reconcile existing pending publication, then require
explicit full-text send/exchange. A Grok hook park probes readiness without
claiming an envelope and returns only a bounded instruction to run foreground
wait. The complete input stays queued until that tool claims, writes and
acknowledges it. These hints share a seven-continuation cap, reserving the last
gate before Grok skips Stop hooks after eight continuations. Other hooks share
the vendor budget; foreground exchange does not consume it. Readiness is not
`handed_off`. No transcript parsing, automatic resend or idle wake-up is added.
See [Grok Native](CLI_REFERENCE.md#grok-build-native) for the upstream limits.

### Native observation and review extensions

`history` is an authenticated read-only operation: optional `id` selects one message;
otherwise `cursor`, `limit` (1–100), `since` (RFC3339) and `pending` select a bounded
page. Normal history is newest-first, pending is oldest-first. Opaque cursors are
publication ordinals, not offsets in a shrinking pending list. The page has a 1 MiB
body/quote budget, retains its first complete message, and inspects at most 5,000
candidates before returning a continuation. Reading never claims, acknowledges or
retries. Receipt values remain private. Full export remains explicit and complete.

Summary counts, current queues, last human-directed message and last wake observation
are replay-built projections of the same Event Log. They are not a second durable
queue. The browser's pending/history views are independent of its recent chat tail.

An optional `review` in an explicit send contains a schema-1 Git observation:
`workspace`, resolved `base`/`head` commits and `dirty_sha256`. It is part of immutable
same-client-ID payload matching and is preserved by explicit Retry. Only explicit
review sends add that metadata to an envelope; ordinary bootstrap/envelope budgets
and routing are unchanged. The workspace in a received anchor is data, not authority
to read another path. Comparison uses the operator-selected trusted checkout.

Unattempted rate-limited wake heads are deferred and rechecked even without a new
publication; reserved wake effects remain no-auto-retry. See [auto-wake](design/auto-wake.md).
