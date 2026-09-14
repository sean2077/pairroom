# Native relay: ownership, discovery, reliability and cost

Status: implementation review, 2026-09-14. Baseline: PairRoom
`5a47fdc5ef0e37794cf9a66457b905896d352a0c` (includes #51 and the corrected #52).
This document explains the implementation and the audit fixes accompanying it;
[Protocol](PROTOCOL.md#native-host-protocol-v7) owns the transport contract and
[CLI reference](CLI_REFERENCE.md#native-relay-commands) owns command usage.

## Run inside the native harness

The user starts Claude Code, Codex CLI or Codex Desktop normally. The agent runs
`pairroom relay bind --create` or `pairroom relay bind` through its own command
execution tool. There is no requirement to launch a separate terminal or fill
in every configuration field. "PairRoom does not own the native process" means
it does not spawn, replace or interrupt that process, not that onboarding runs
outside it.

Discovery uses the current Git workspace, recognized harness lineage and native
session metadata (`CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`). Exact session
metadata selects an existing associated Room/slot ahead of PID-only matching:
a Desktop or app-server process can host multiple threads. Repeating `bind` in
that same session resumes its saved Service endpoint and identity without
manual `--continue --session-id`. The Service still owns omitted pair selections.
No provider secrets or guessed model/effort settings are harvested.

A nearest recognized harness scopes inherited outer-harness variables. Ambiguous
or unmatched metadata fails rather than consuming another session's inbox.
A unique pending binding of the same runtime is diagnosable with `status --brief`
without Room/slot flags; multiple pending bindings still need an explicit
candidate. Send, wait and exchange never select a pending nonce. Older
harnesses without session metadata retain lineage and explicit flags.
These are convenience selectors, not proof of identity or a sandbox against
other programs running as the same OS user. Authoritative association still
requires the approved Stop hook, one-time nonce, and official session ID; all
later operations authenticate the binding credential, generation and session.

Create in the first session, join in the other, echo the returned nonce only
for a new binding, then use the installed relay skill. Ambiguous Room or
same-runtime slot choices still require explicit selection. Keep native
approvals and PATH setup in the user's control.

The main repository can remain the session entry point while work happens in
`.worktrees/<task>`. Keep commands pointed at the bound Room workspace and use
an explicit task path for edits/tests/review; Native does not create or merge
worktrees. Changing the shell's directory does not grant a new Room identity.

### Runtime boundary

Native currently supports Claude Code and Codex. Grok Build is recognized so
it cannot accidentally fall through to an outer Claude/Codex binding, but is
not enabled as a Native runtime by this change. Embedded Grok is separate.
Grok's current hook contract uses camelCase session/reply fields, clips the
last assistant message, and caps feedback delivered through a Stop gate. It
cannot honestly inherit the existing full-reply relay guarantee by changing a
runtime allowlist. Future support needs an explicit tested adapter and handling
for those limits, not a fake Claude identity or transcript scraping.

Sources: [Claude session environment](https://code.claude.com/docs/en/env-vars),
[Codex SDK session reference](https://github.com/openai/codex/blob/main/sdk/typescript/README.md).
Grok hook stdin uses camelCase (`sessionId`, `lastAssistantMessage`,
`stopHookActive`); `GROK_SESSION_ID` identifies the session; Stop feedback and
last-assistant text are clipped. Vendor documentation describes interfaces, not
successful authenticated E2E in this repository.

## The two receive paths share one mailbox

A Stop hook publishes the complete routed reply and may park for up to 30 seconds
inside its installed 45-second budget. `decision:block` requests continuation;
it is not arbitrary idle-session wake-up. The eight actual-message block cap is
unchanged.

Foreground `exchange` sends once, then returns the next eligible FIFO input in
the same tool invocation. `wait` only collects. Their HTTP polls remain at most
30 seconds; a successful explicit empty response renews in the CLI, not the
model. Exchange defaults to one hour, finite totals allow six hours, and `0`
means no PairRoom total deadline. Caller cancellation, native tool limits,
revocation and transport errors still end a wait. Neither path owns subagents
or the native model/tool loop.

Send plus receive is not atomic or a correlated request/reply transaction. An
earlier queued message or user instruction may arrive first. Finish by sending
a final result without another wait; after explicit publication do not repeat
a peer-directed final reply unless a second Stop publication is intended.

## Findings and fixes

| Finding | Practical severity | Fix and regression evidence |
| --- | --- | --- |
| PID-only selection cannot distinguish Desktop threads and requires identity flags after resume | High workflow correctness value | Native session-aware selection and same-session bind resume; conflicting metadata, unique pending status inspection, pending nonce and same-PID/different-thread tests |
| Multiple CLI collectors can compete for different FIFO messages | High reliability value | Separate process-owned collector lock across wait/exchange; exchange acquires it before publication; hook publishes first and skips collection if occupied; subprocess/crash-release tests |
| Every active relay request rereads the full Event Log for admission | Medium scaling/performance value, not evidence of data loss | Active Engine authentication, durable pre-authentication for cold activation; effect-boundary reauthentication retained; loader-count and revocation tests |
| SSE clones the full snapshot to read one sequence number | Medium allocation/scaling value | Locked scalar cursor read, zero-allocation assertion; browser/Event Log semantics unchanged |
| Diagnosing one stalled round returns all messages to the model | Medium context/privacy value | Opt-in `status --brief`/`reconcile --brief`, built body-free in the Engine, no full snapshot round trip; authenticated size/leakage and real CLI/HTTP tests |
| Foreground receive inherits the Stop hook's four-second deadline reserve | Medium correctness value | Reserve only in hook mode; a foreground tool can collect queued work within a short caller deadline; cancellation and stdout-before-ack retained |

The collector directory is a kernel-lock anchor, not another journal or an
owner file. It is separate from the short state lock; ordinary send, status,
publication and unbind remain usable during long waits. Process death releases
the kernel lock. There is no timer that steals it from a still-running collector.
After rebinding, the Service rejects an old generation; a local lock does not
authorize it. This coordinates PairRoom CLI instances on one host, not arbitrary
HTTP clients or machines sharing a filesystem.

The performance bugs are history-dependent overhead, not proof of an outage,
security incident or wrong output. Removing them reduces unnecessary work.
`--brief` keeps output size bounded but counts still scan the in-memory messages;
it is not an O(1) indexed-history redesign. Full status/export intentionally
retain history. No persistent auth cache, schema or scheduler is introduced.

## Delivery evidence remains conservative

`queued` means durably accepted; `delivering` precedes envelope release;
`handed_off` means the CLI wrote stdout and acknowledged it, not that the model
read, accepted or completed anything. Lost output/ack and collector death can
become `unknown`. Do not automatically replay possibly executed effects.
Summary counts and bounded recovery IDs are transport observations, never
"working", "done", or "needs user" guesses based on silence.

A warm authentication check uses the actual active Engine, not a separate
credential cache. Cold requests still read durable binding facts before
consuming Runtime capacity. Rebind/revoke races are checked again by the
actual operation; draining still admits valid completion receipts without
reactivating a suspended Room. The log remains the durable authority.

## PairRoom versus Orca: what the evidence supports

The core loops are similar: publish, wait, receive, think, repeat. Orca's
[mailbox contract](https://github.com/stablyai/orca/blob/8e26d516d859e7a27016b5ca42c2d14bafd31052/skill-guides/orchestration/references/messaging-and-gates.md)
uses durable messages, consuming acknowledgements and blocking questions.
Its [worker preamble](https://github.com/stablyai/orca/blob/8e26d516d859e7a27016b5ca42c2d14bafd31052/src/main/runtime/orchestration/preamble.ts)
adds task/attempt lifecycle, explicit completion and worker coordination rules.
It can reuse sessions, wait without model-driven polling, and send summaries;
do not claim it always starts new agents or forwards complete history.

PairRoom's narrower peer relay avoids requiring Run/Task/Dispatch bookkeeping
and preserves user-owned desktop sessions. The shared protocol tests cap the
ordinary envelope overhead at 128 UTF-8 bytes and the native bootstrap plus
default collaboration at 1,800 bytes. Those caps exclude the body, quoted text,
attachments, optional skill, custom instructions and native harness context.
They are neither tokenizer counts nor billing measurements.

Full reply preservation avoids a relay-generated summary losing evidence, but
long replies can cost more than Orca's focused summaries. Explicit exchange
still generates tool calls and results. Native tool backgrounding, interrupted
waits, repeated file reads, cache behavior, retries and implementation rework
can dominate the small envelope difference. Neither lower total cost, higher
quality nor faster completion has been established against Orca.

Borrow clear receipt boundaries, active waiting, precise session targeting and
body-free inspection. Do not add a generic worker fleet or model heartbeat just
to claim parity. Notice/attention improvements should use real lifecycle or
explicit user-escalation events, not additional inference calls.

## Review passes and remaining validation

This change was reviewed in passes for (1) onboarding/ownership, (2) concurrent
collection and cancellation, (3) warm/cold authentication and revocation,
(4) history-dependent I/O, allocation and diagnostic context, and (5) regression
contracts and documentation. The concrete findings above have code and tests;
that is not a guarantee of no undiscovered defects.

Native vendor/tool E2E remains unverified here: real Codex Desktop and Claude
Code tool continuation, approvals, backgrounding, one-hour work, interruption,
process restart and comparative billed usage need an authenticated run. No
synthetic fixture establishes these. A fair cost comparison holds model,
provider, effort, task revision and acceptance criteria constant, then records
quality, elapsed time, user nudges, fresh/cache input and output/reasoning usage
where available, repeated context and recovery work. Do not run paid vendor
benchmarks without consent or publish private transcripts as evidence.
