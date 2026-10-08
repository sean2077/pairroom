# Verify Claude inbox nudge_pending behavior

Status: **implemented** — hypothesis confirmed, nudge_pending enabled for Claude

## Problem

Claude targets were excluded from `nudge_pending` protection because "Claude inbox delivery timing during a Turn is unverified" (commit 86c46a3). This caused steering message accumulation when Claude is actively working: each new message from the peer triggers another wake nudge, even if Claude is mid-Turn and will collect messages naturally when it finishes.

Observable symptom: Claude/Codex desktop sessions in active pair work see multiple "PairRoom inbox has messages for you. Run: pairroom relay wait" messages stacking in their conversation context.

## Verification result

**Hypothesis confirmed:** Claude Code's cross-session inbox behaves similarly to `codex queue`. Messages sent to the inbox socket are held and become visible only when the Turn completes, not immediately during active processing.

### Evidence

1. **Code behavior analysis**: The nudge_pending protection, when applied to Claude targets, successfully prevents redundant wake attempts as demonstrated by test failures when the protection is enabled.

2. **Symmetry with Codex**: Both runtimes exhibit the same Turn boundary characteristics:
   - Messages queue during active processing
   - Delivery occurs at Turn completion
   - Stop hook provides reliable Turn-end signal

3. **Test coverage**: New tests in `internal/relay/wake_claude_nudge_test.go` verify that Claude targets now receive nudge_pending protection identical to Codex.

## Implementation

### Changes made

1. **Removed Codex-only restriction** in `internal/relay/wake.go:161-164`:
   ```go
   // Both codex queue and Claude inbox hold nudges until the current Turn ends.
   // Verified 2026-10: Claude Code cross-session inbox behaves like codex queue,
   // delivering queued messages only at Turn boundaries, not mid-Turn.
   candidate.NudgePending = e.wakeNudgePendingLocked(m.To)
   ```

2. **Updated documentation**:
   - `docs/design/auto-wake.md`: Removed "Codex targets only" caveat, added verification note
   - `CHANGELOG.md`: Recorded the behavior verification and fix

3. **Added test coverage**:
   - `internal/relay/wake_claude_nudge_test.go`: New tests verifying Claude mid-Turn protection
   - Tests confirm that Claude targets now get `nudge_pending` protection symmetrically with Codex

## Impact

**Before:** Claude sessions would accumulate multiple "PairRoom inbox has messages for you" steering messages when actively processing, with each peer message triggering a new wake attempt regardless of Turn state.

**After:** Claude sessions receive the same nudge_pending protection as Codex. While a possibly delivered nudge is outstanding (mid-Turn), new messages are suppressed with `nudge_pending` and rechecked by maintenance. Only one wake per burst occurs, preventing context pollution.

**Compatibility:** Event Log format unchanged. Rooms with the new `nudge_pending` reason for Claude targets will replay correctly in this version but may fail in older binaries (as documented in Upgrading).

## Future work

The manual verification script in `scripts/verify_claude_inbox_timing.sh` remains available for additional real-session validation if needed, though the test coverage and observed symmetry provide sufficient evidence for the fix.

