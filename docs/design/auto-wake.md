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
small typed runtime policy keeps the transport allowlist and consumption
observation together, so supporting another runtime requires an explicit decision
about both. A missing runtime does not authorize a slot-default transport.

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

Keeping an attempted queue head prevents repeated nudges while that head remains
queued. It does not cover Management cancellation of that head: cancellation is
not evidence that the native inbox was consumed. Outstanding-nudge state is
therefore independent of FIFO membership. Without it, repeated canceled bursts
can spend the shared Room budget and deny the other peer its first wake. Conversely,
keeping that state forever would strand work when a vendor holds, drops or never
reads a nudge. The bounded renewal in the Protocol balances those two failures.

Claude and Codex require different consumption observations. Anthropic's
[message-delivery and inbound-control documentation](https://code.claude.com/docs/en/cross-session-messaging#message-delivery),
reviewed 2026-10-09, permits Claude to read its inbox between tool calls during
an active Turn; a running tool is not interrupted, and inbound `hold` can defer
delivery. This supports observing a later authenticated relay call without
waiting for another Turn. It does not prove consumption at reservation or socket
submission. Applying Codex's Turn-end observation to Claude can instead retain
an already-consumed nudge after same-Turn collection and delay fresh work. The
runtime policy preserves outstanding state for both transports while selecting
the appropriate observation; neither is a vendor acknowledgement.

## Verification

Tests use a real Engine and waker with a controlled clock to cover cancellation
without receiver activity, the shared peer budget, later authenticated activity,
bounded renewal and replay. The complementary same-Turn Claude regression
collects and acknowledges one burst, completes an empty Stop, and allows the next
burst under the existing rate limit. Existing Codex tests retain its stricter
busy-Turn rule. Additional tests cover collector/reservation races, unsupported
runtime identities, redaction, private capability identity, local IPC fixtures
and HTTP-to-inbox-to-FIFO collection.

These tests establish PairRoom behavior. They do not establish real model
acceptance, the native inbound setting, or billed-token improvement. Authenticated
vendor testing remains a separate release gate; the upstream documentation is
evidence for a delivery boundary, not a record of such testing.
