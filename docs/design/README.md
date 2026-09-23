# Design records

These pages explain decisions and recorded implementation evidence. They do not override current source, authorize new work, or replace the [documentation owners](../README.md). The operator path is [Native relay](../NATIVE_RELAY.md); wire/state contracts belong in [Protocol](../PROTOCOL.md) and [Storage](../STORAGE.md).

| Record | How to use it |
|---|---|
| [Native host mode](native-host-mode.md) | Architecture rationale and acceptance boundaries; use current guides for setup and supported runtimes |
| [Native efficiency boundaries](native-efficiency.md) | Dated external research, bounded-history tradeoffs, and limits on efficiency claims; not a fresh benchmark |
| [Automatic wake](auto-wake.md) | Reservation, budget, and no-retry rationale; current limits are specified by Protocol |
| [Claude inbox wake](claude-inbox-wake.md) | Private capability transport and security rationale; submission is not model acceptance |
| [Native review and recovery closure](native-review-closure.md) | The 2026-09-22 implementation record, acceptance map, and historical microbenchmarks; current UI/workflow lives in Native relay |
| [Participant-slot cutover](slot-actor-migration.md) | Completed historical decision, with a pinned copy of the original plan; not pending phases or a purge instruction |

Preserve dates, source revisions, and measurement caveats. A passing fixture recorded in a design does not certify the current build, and agreement during review does not authorize implementation. New unimplemented proposals normally belong in an Issue/PR; add a durable record only when its rationale is worth maintaining separately.
