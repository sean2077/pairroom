# Automatic wake for idle native sessions

Status: implemented. Codex queue wake was approved 2026-09-17; Claude inbox
wake was requested 2026-09-21. The sole detailed wake contract is
[Protocol: Automatic idle-peer wake](../PROTOCOL.md#automatic-idle-peer-wake).
This document records the rationale and evidence behind it. Historical real-vendor
experiments remain in [Native relay](../NATIVE_RELAY.md#verified-vendor-wake-surfaces);
Claude inbox [setup and security](claude-inbox-wake.md) have a separate owner.

## Scope and decisions

A queued message is safe in the FIFO but cannot itself make an idle native
session run. The Service can nudge an existing session through an available
vendor transport. This avoids launching a competing session or making the model
poll for work. Per-Room control and vendor inbound policy preserve the operator's
choice about automatic wake.

One Service-side waker shares effect admission across supported transports. A
small typed runtime policy keeps the transport allowlist and the observation
that releases an outstanding wake together, so supporting another runtime
requires an explicit decision about both. A missing runtime does not authorize
a slot-default transport.

Task text has one authoritative source: the relay FIFO. The external wake carries
only a fixed instruction to collect it. This keeps vendor wake transport from
becoming a second task-delivery channel and keeps native session identity and
inbox capabilities out of wake audit facts. Reservations identify possible
external effects; delivery receipts identify FIFO hand-off. Those are different
facts and cannot substitute for each other.

## Races and recovery

A point-in-time wake candidate cannot authorize a later effect: collection,
cancellation or replacement may win before reservation. Likewise, observing an
idle Runtime separately from admitting a wake leaves a gap in which suspension
can cancel an already authorized effect. The shared admission boundary and short
effect lease close those races. Grace and capability checks hold no possible
external effect, so making them pin a Runtime would keep unavailable peers alive.
The [Protocol](../PROTOCOL.md#automatic-idle-peer-wake) owns the exact rechecks,
lock order, maintenance, timing and shutdown rules.

Queue membership and outstanding-wake state answer different questions. An
attempted message can remain queued after a failed wake, and Management can
cancel that message while a possibly delivered nudge remains outstanding.
Neither case may hide unattempted input from maintenance. The next candidate
therefore stays visible while the outstanding-wake projection decides whether
to defer it. This preserves audit and lease coverage without automatically
retrying an attempted ID. The Protocol's bounded suppression also prevents
repeated canceled bursts from exhausting the other peer's shared Room budget
when a vendor holds, drops or never reads a nudge.

Claude and Codex expose different useful observations. Anthropic's
[message-delivery and inbound-control documentation](https://code.claude.com/docs/en/cross-session-messaging#message-delivery),
reviewed 2026-10-09, permits Claude to read its inbox between tool calls during
an active Turn; a running tool is not interrupted, and inbound `hold` can defer
delivery. The session's own status or publication calls can continue while a
nudge is held, so ordinary authenticated activity proves no inbox progress.
Claude's acknowledged FIFO handoff is a stronger observable boundary: the
collector received useful input, making the earlier nudge unnecessary even
without waiting for another Turn. It does not prove that the vendor delivered
or removed the nudge. Codex retains its existing Turn-based inference. The
runtime policy and durable handoff projection keep these distinctions explicit;
the exact observations belong to the Protocol, not a vendor-acceptance claim.

## Verification

Tests use a real Engine and waker with a controlled clock to cover authenticated
activity without collection, the shared peer budget, receipt-matched handoff,
queued successors after failure or progress, bounded renewal and replay. The
same-Turn Claude regression acknowledges one burst and allows the next under
the existing rate limit; existing Codex tests retain its stricter busy-Turn
rule. Additional tests cover collector/reservation races, unsupported runtime
identities, redaction, private capability identity, local IPC fixtures and
HTTP-to-inbox-to-FIFO collection. Service restart tests separately cover
Grok/Gemini waiting-input notification without an external wake transport.

The [2026-10-08 Claude verification record](claude-nudge-pending-verification.md)
is retained with its original assertions marked superseded. Its code analysis
and fixture symmetry did not verify real vendor timing, and the original text
must not be used as current acceptance evidence.

These tests establish PairRoom behavior. They do not establish real model
acceptance, the native inbound setting, or billed-token improvement. Authenticated
vendor testing remains a separate release gate; the upstream documentation is
evidence for a delivery boundary, not a record of such testing.
