# Opt-in automatic wake for idle native sessions

Status: draft — owner decision required; nothing here is implemented.
Updated: 2026-09-16. Evidence base: the verified vendor wake surfaces in
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

Goal: an optional, audited, rate-limited automatic wake of an idle
Codex-bound session through the vendor queue surface, so a publishing agent
can reach a deep-idle peer without human presence.

Non-goals: waking Claude Code sessions (no external injection surface exists;
resume of a running session forks a copy), waking Grok (unverified, binding
unimplemented), replacing vendor approval semantics, delivering message
bodies through vendor argv, or any always-on default.

## Decision points

1. **Who executes the wake.** Service-side waker vs sender-side CLI vs
   human-only (status quo). Service-side gives one choke point for rate
   limits and audit, and the binding already holds the vendor session id; it
   requires the `codex` CLI on the Service host. Sender-side needs no new
   Service authority but makes injection agent-triggered and requires the
   vendor CLI on every agent host. Human-only keeps the current template.
2. **Configuration surface.** Service-wide default off, per-Room opt-in
   changeable only at an idle Room boundary (mirrors the permission-profile
   rule), vs per-binding opt-in.
3. **Nudge contract.** Fixed body-free text constant, never Room content;
   dedupe so a burst of queued messages produces at most one wake per quiet
   period; thread identity never written to the Event Log, files, or relay
   bodies, and redacted from diagnostics.
4. **Rate and cost limits.** Minimum interval per Room (suggested ≥ 60 s) and
   hourly cap (suggested ≤ 10); every wake is a billed model turn under the
   receiver's native approval/tool policy.
5. **Audit.** Event Log records wake attempts and outcomes
   (`accepted` / `failed` / `suppressed`) without thread identity or bodies.
6. **Failure modes.** Thread exited, not found, vendor version drift, or
   non-zero exit fail closed to today's queued-to-idle behavior; no automatic
   retry — a second attempt requires a fresh explicit decision (the no-retry
   rule proven in the 2026-09-16 experiment).
7. **Boundary amendment.** The CLAUDE.md native host boundary gains one
   sentence: an opt-in wake uses only the vendor-sanctioned queue surface,
   body-free, rate-limited, and audited. This is a durable-invariant change
   and requires owner approval on its own.

## Verification plan (if approved)

Unit: waker scheduling, dedupe, limits, audit shape, redaction. Integration:
Mock cannot fake a vendor wake, so real E2E on codex-cli follows the same
consent protocol as the 2026-09-16 experiment (verbatim owner authorization,
visible cancel window, sanitized fixed-category reporting). Release gates keep
the "no synthetic E2E claims" invariant.

## Open questions

- Park interaction: a wake landing inside the receiver's 30-second Stop park
  could double-deliver the same input; FIFO claim is single-owner, but the
  turn-shape needs a real observation before implementation.
- Should `queued_inbox_hints` and the human wake template remain visible when
  auto-wake is enabled (current answer: yes, as human fallback)?
- Does a wake attempt belong in Room history UI, or diagnostics only?
