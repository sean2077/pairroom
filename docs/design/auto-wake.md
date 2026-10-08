# Automatic wake for idle native sessions

Status: implemented. Codex queue wake was approved 2026-09-17; Claude inbox
wake was requested 2026-09-21. Current contracts live in
[Protocol](../PROTOCOL.md#automatic-idle-peer-wake). Historical real-vendor
experiments remain in [Native relay](../NATIVE_RELAY.md#verified-vendor-wake-surfaces);
the Claude inbox implementation has separate [setup and evidence boundaries](claude-inbox-wake.md).

## Scope

A queued message is safe in the FIFO but cannot itself make an idle native
session run. The Service may send a fixed nudge to an existing Claude inbox or
Codex thread, subject to per-Room opt-out and native inbound policy. It does not
launch/resume a Claude copy, interrupt either process, replace vendor approvals,
or send task content through a second transport. Grok external wake is not
integrated; its tracked background-collector completion remains available.

## Decisions

- **One Service-side waker.** Both transports share collector checks, a two-second
  grace, one pending decision per target, durable reservations and rate limits.
  Codex uses the operator-configured executable; Claude uses the private inbox
  capability captured by its own confirmed bind/Stop. Mock never contacts a real
  inbox or vendor CLI unless a test explicitly injects a fixture.
- **Per-Room control.** Default on; Management may change it only at an idle Room
  boundary. Delivering/unknown work must be reconciled first. Relay binding
  credentials cannot change wake policy.
- **One authoritative message source.** Wake carries only `nativeWakeNudge`.
  The FIFO owns task text and collection. Native session identity, inbox paths
  and tokens never enter wake events or relay bodies.
- **Bounded effects.** Minimum interval 60 seconds per receiver, shared Room hourly cap 10.
  Reserve by transport message ID before the external effect and never retry
  automatically, including after restart. A submitted nudge may cause a billed
  native turn; a failed or held attempt does not prove a model ran.
- **Conservative evidence.** `accepted` means the Codex queue command succeeded;
  `submitted` means the Claude socket write completed, not vendor acceptance.
  Both have an empty reason. `failed`/`suppressed` require a fixed allowlisted
  reason; raw vendor output and OS errors are discarded. Replay validates the
  same vocabulary.

## Races and recovery

The Native Runtime's final idle-close admission shares the waker mutex with reservation-lease acquisition and HTTP use registration, then checks delivery admission under the Engine lock before setting drain. The lock order is waker then Engine; Engine append observers never reenter the waker. A short lease begins before reservation and ends after outcome audit, including on a rejected reservation. Persistent appends and vendor effects run without holding the waker mutex. This closes both the reservation-to-lease gap and the manager's stale idle observation: an admitted lease refuses idle close, while admitted idle close rejects a later lease without spending a reservation. Grace and capability checks remain outside the lease so unavailable peers do not pin a Runtime.

Shutdown may cancel an already reserved attempt, but its known result can still be appended while draining, after matching the transport message ID and target against the durable reservation. Suppression observations and fresh reservations require normal admission; writer closure or uncertain persistence remains fail-closed. No new event format or automatic retry is introduced.

`WakeCandidate` observes the current queue, binding generation, active collectors
and in-flight delivery. `ReserveWake` rechecks them under the Engine lock before
appending a durable effect reservation. Collection, cancellation, replacement
or policy change before this boundary suppresses the effect without consuming
a reservation. Collection arriving afterward may make one nudge redundant;
the FIFO still prevents a second collection of the same message.

The current oldest queued input owns its wake decision. The existing one-second
maintenance tick inspects at most two indexed queue heads, with one active worker
per receiver. Cancellation, collection, collector exit, binding changes and restart
therefore cannot strand an unattempted successor merely because no new send arrives.
A rate-suppressed head has a next eligible time and keeps the runtime alive across
its idle timeout. At expiry it is revalidated, not blindly submitted. Missing Claude
capabilities are rechecked at a bounded 30-second cadence while the runtime remains
active, without holding the runtime lease on its own. Unbound/unsupported sessions
do not pin a runtime; repeated identical
suppression observations are coalesced instead of appending audit every second.

A durable reservation means a possibly attempted effect, including a crash before
the result could be recorded. Such heads are never automatically retried. A mere
rate suppression has no reservation and may be reconsidered. An attempt can be
accepted or submitted without producing a native turn, so the message queued
right after the newest attempted one owns a new burst once that attempt is
`WakeRenewAfter` (10 minutes) old; before then a busy target is not nudged twice.
Renewal reserves the new message ID under the same limits and never re-reserves
an attempted message.

Collection is not consumption. `codex queue` holds a nudge until the current
native Turn ends and offers no list or dedupe API, so a target that collects its
burst mid-Turn with `relay wait` would otherwise receive a second queued nudge
for its next burst, each later spawning a Turn. The Engine therefore infers an
outstanding nudge from Turn boundaries it already observes: the time of the
target's last authenticated relay call, and the last Turn end — an
authenticated Stop park that releases no envelope, or `StopFailure`. A park that
delivers input continues the Turn and is not a Turn end. At a reservation the
target is idle when its last relay call precedes the last Turn end; an idle
target consumes the nudge with its next relay call, a mid-Turn target only with
a relay call after a later Turn end. `accepted`, `submitted` and outcome-less
reservations count; a definite `failed` does not. While a nudge is outstanding
the new head is suppressed as `nudge_pending` without a reservation, so it stays
an unattempted head that the maintenance tick rechecks. Like a rate deferral it
holds the Runtime lease, for at most `WakeRenewAfter` after the attempt, so a
short idle timeout cannot strand it. Replacement, unbind or session change
clears the state. It is an in-memory projection: reservations and outcomes
replay from existing facts, Turn ends do not, so a replayed nudge takes the
mid-Turn rule. Sessions whose Stop hook is unapproved or skipped (for example a
held foreground collector) produce no Turn end and fall back to the same
10-minute bound. No Event Log field or format changes. The rule applies to both
Claude and Codex targets: verification in October 2026 confirmed that Claude Code's
cross-session inbox behaves like codex queue, holding messages until the current
Turn ends rather than delivering them mid-Turn. Runtime shutdown
cancels and joins wake workers before closing the event writer. Reservations replay into the rate-limit history, so a
Service restart cannot repeat a possibly successful effect. Missing capability,
missing CLI, failed transport or native `hold`/`refuse` leaves the receive-only
`relay wait`/human path available. Do not resend task content to recover wake.
Human-executable Codex templates remain a fallback; no manual Claude socket
command or secret is printed to the agent.

## Verification

Waker tests cover suppression, grace races, burst deduplication, limits, durable
reservation, cancellation, redaction and restart. Claude tests add private
capability identity, actual local IPC fixtures and HTTP-to-inbox-to-FIFO
collection. Mock and synthetic transport tests prove PairRoom behavior, not a
real model turn or billed-token improvement. Real authenticated vendor testing
requires owner authorization and remains a separate release gate.
