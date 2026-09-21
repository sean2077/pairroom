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
- **Bounded effects.** Minimum interval 60 seconds per Room, hourly cap 10.
  Reserve by transport message ID before the external effect and never retry
  automatically, including after restart. A submitted nudge may cause a billed
  native turn; a failed or held attempt does not prove a model ran.
- **Conservative evidence.** `accepted` means the Codex queue command succeeded;
  `submitted` means the Claude socket write completed, not vendor acceptance.
  Both have an empty reason. `failed`/`suppressed` require a fixed allowlisted
  reason; raw vendor output and OS errors are discarded. Replay validates the
  same vocabulary.

## Races and recovery

`WakeCandidate` observes the current queue, binding generation, active collectors
and in-flight delivery. `ReserveWake` rechecks them under the Engine lock before
appending a durable effect reservation. Collection, cancellation, replacement
or policy change before this boundary suppresses the effect without consuming
a reservation. Collection arriving afterward may make one nudge redundant;
the FIFO still prevents a second collection of the same message.

A burst's oldest queued input owns its wake decision; later inputs do not create
additional attempts. Reservations replay into the rate-limit history, so a
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
