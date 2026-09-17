# Automatic wake for idle native sessions

Status: landed — owner approved and implemented 2026-09-17
(`internal/relay/wake.go`, `internal/service/native_waker.go`).
Updated: 2026-09-17. Evidence base: the verified vendor wake surfaces in
[NATIVE_RELAY.md](../NATIVE_RELAY.md) (Claude Code background-task wake;
codex-cli 0.154.0 `queue` wake of a deep-idle thread, one-time controlled
experiment).

## Problem

Native collaboration stalls when the receiving session is deep-idle: the FIFO
keeps every message safe, but no model turn starts until a human interacts or
the receiver happens to end a turn inside its Stop-park window. Claude-side
background waits and the human-executed Codex wake template reduce friction,
but both still require one participant to act first.

## Goals / Non-goals

Goal: a per-Room configurable (default on), audited, rate-limited automatic
wake of an idle Codex-bound session through the vendor queue surface, so a
publishing agent can reach a deep-idle peer without human presence.

Non-goals: waking Claude Code sessions (no external injection surface exists;
resume of a running session forks a copy), waking Grok (unverified, binding
unimplemented), replacing vendor approval semantics, delivering message
bodies through vendor argv, or any Service-wide always-on switch without
per-Room opt-out.

## Decision points

Owner decisions recorded 2026-09-17.

1. **Who executes the wake.** Decided: Service-side waker. One choke point
   for rate limits and audit; the binding already holds the vendor session
   id. Requires the `codex` CLI on the Service host; fail closed to the
   queued-to-idle behavior with a diagnostic when it is absent.
2. **Configuration surface.** Decided: per-Room, default on; changeable only
   at an idle Room boundary (mirrors the permission-profile rule). No
   Service-wide always-on switch without per-Room opt-out.
3. **Nudge contract.** Fixed body-free text constant, never Room content;
   dedupe so a burst of queued messages produces at most one wake per quiet
   period; thread identity never written to the Event Log, files, or relay
   bodies, and redacted from diagnostics.
4. **Rate and cost limits.** Decided: draft defaults — minimum interval per
   Room ≥ 60 s and hourly cap ≤ 10; every wake is a billed model turn under
   the receiver's native approval/tool policy.
5. **Audit.** Event Log records wake attempts and outcomes
   (`accepted` / `failed` / `suppressed`) without thread identity or bodies.
6. **Failure modes.** Thread exited, not found, vendor version drift, or
   non-zero exit fail closed to today's queued-to-idle behavior; no automatic
   retry — a second attempt requires a fresh explicit decision (the no-retry
   rule proven in the 2026-09-16 experiment).
7. **Boundary amendment.** Decided: approved by the owner 2026-09-17. The
   CLAUDE.md native host boundary gains one sentence: an enabled wake uses
   only the vendor-sanctioned queue surface, body-free, rate-limited, and
   audited. This is a durable-invariant change.

## Implementation notes (2026-09-17)

- Landed Engine contract in `internal/relay/wake.go`: `WakeEnabled`,
  `SetWakeEnabled` (rejects `ErrWakeRoomBusy` while delivering/unknown
  deliveries are unresolved), `ReserveWake` (duplicate-rejecting,
  `ErrWakeReserved`), `RecordWake` (fixed outcome/reason vocabulary;
  `accepted` carries no reason, `failed`/`suppressed` require one; replay
  enforces the same rules fail-closed), `WakeReservations` (replay-restored
  history injected at waker activation), and the atomic `WakeCandidate`
  query (`QueueStart`/`WaiterActive`/`Delivering` facts; `Claim` long-polls
  register blocked waiters per slot as transient process state).
- Waker core in `internal/service/native_waker.go` (Agent 2 session):
  2-second grace with one pending grace per target; a queue that empties
  during grace records `suppressed/collected`; extra enqueues in the same
  burst merge without per-message audit; the reason vocabulary gained
  `waiter_active` and `collected` on top of the original set.
- Management surface: `POST /api/v1/rooms/{room}/wake-config`
  (Management-authenticated, user-owned; relay binding credentials can
  never reach it). `relay status` summaries expose `wake_enabled`.
- Park-interaction open question: converged. Waiter facts plus the grace
  keep wakes away from live park/foreground collectors; single-owner FIFO
  bounds the residual claim race to at most one redundant vendor turn.
- Human wake template and queued-inbox hints remain visible when auto-wake
  is enabled, as the fallback for disabled Rooms, non-Codex targets, and
  wake failures.



## Verification plan

Unit: waker scheduling, dedupe, limits, audit shape, redaction. Integration:
Mock cannot fake a vendor wake, so real E2E on codex-cli follows the same
consent protocol as the 2026-09-16 experiment (verbatim owner authorization,
visible cancel window, sanitized fixed-category reporting). Release gates keep
the "no synthetic E2E claims" invariant.

## Open questions

- Park interaction: resolved 2026-09-17 — `WakeCandidate` reports blocked
  Claim waiters and in-flight deliveries atomically, and the waker's grace
  re-checks the queue before executing; single-owner FIFO bounds the residual
  race to at most one redundant vendor turn.
- `queued_inbox_hints` and the human wake template remain visible when
  auto-wake is enabled, as the human fallback (resolved 2026-09-17).
- Does a wake attempt belong in Room history UI, or diagnostics only?
  (Still open; browser UI projection deferred.)
- Misrouted-publication noise (observed 2026-09-17): resolved by team
  agreement 2026-09-17 — the waker applies identical semantics to every
  peer-directed queued input, including `source: stop` publications; a
  legitimate peer-directed Stop relay deserves the same wake. Misrouting is
  prevented at the agent-prose layer (visible replies avoid quoting exact
  peer handles unless routing is intended), never by waker-layer damping or
  silent suppression.
