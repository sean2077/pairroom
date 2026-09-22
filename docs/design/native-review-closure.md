# Native review and recovery closure

Implemented for the nine-point review of main `a299c50870f2e1f08d5897ae7a7e9e05752b2036`
(2026-09-22). This remains a two-session relay, not a phase compiler or IDE.
The Event Log, FIFO, generation authentication and uncertain-effect boundaries
remain authoritative. No paid/authenticated vendor acceptance is implied below.

Integrated with the concurrent Native IM update in main `9e079ecafd4f383068f129326f46add007b3aa4e`.
Chat bubbles, reading-position anchoring, participant chips, keyboard focus and the
collapsible inspector are retained. Pending/history/diagnostics share that inspector.
The real Chromium presentation fixture passes in light/dark and at 320-pixel width;
real HTTP/SSE recovery and vendor-model acceptance remain separate test categories.

## User path

Bind normally. Use the native conversation for decisions and readable Markdown/code;
long evidence can be expanded without truncating its content. The **Pending items**
panel remains independent of the recent 300-message chat window. It pages oldest
queued, delivering and unknown items and retains their explicit Cancel/Retry controls.
Inspect an old message by ID or page through history without loading an entire log.
The latest message explicitly addressed to the user and uncertain/wake-failure facts
are surfaced as attention, not guesses that a silent model is stuck.

If a browser response is lost, the original publication ID and payload survive reload
in same-origin localStorage. Reload checks that ID; it never sends automatically.
An explicit retry uses the saved ID/body/target/attachments/review version. A matching
receipt clears the draft. Storage failure blocks publication before POST. Explicit
Forget only removes local recovery state and can lose that safety identity; it does
not cancel an accepted or in-flight message. See [Storage](../STORAGE.md).

On reachability uncertainty, use the Native diagnostic button or `relay doctor` in
the intended session. It distinguishes association, registered collection, queued /
delivering / unknown work, captured capability, last wake outcome, cooldown and next
action. CLI doctor additionally checks its own hook definition/version/workspace.
Hook approval and model acceptance stay unknown. No live vendor request is made and
a suspended runtime is not implicitly activated by doctor. Wake configuration still
belongs to Management; native inbound settings are never changed.

## Deferred wake is not an unsafe retry

The shared Room hourly cap remains ten reservations. The one-minute cooldown is now
per receiving slot, so a reply in the opposite direction is not blocked by the first
wake. An unattempted head suppressed by a budget has a next eligible time. The existing
maintenance tick checks at most two indexed heads and reschedules only unreserved,
still-eligible work. Cancellation, queue-head changes, collector exit and restart are
covered. Deferred rate work retains a runtime lease (separate from HTTP-use evidence).

The durable reservation is still written before vendor submission. Accepted, failed,
submitted and crash-interrupted reserved effects are **never** automatically replayed.
An uncollected reserved head continues to block later heads from producing a second
nudge for the same burst. Inbound hold/refuse and missing/stale capabilities remain
visible fallback situations, not permission to resend the task. Wake workers are
cancelled and joined before the Room event writer closes.

## Optional versioned evidence, not workflow machinery

```sh
pairroom relay send --id review-v1 --text "Review this revision; do not implement" \
  --review --review-repo /absolute/task-worktree --review-base main
pairroom relay history --id <published-message-id>
pairroom relay review --id <published-message-id> --review-repo /absolute/task-worktree
```

A review anchor contains the canonical selected checkout, resolved base/head commits,
and a dirty SHA-256 covering both staged and unstaged diffs plus untracked non-ignored
file bytes/modes (symlinks are hashed as link text). Capture is limited to 15 seconds,
16 MiB of evidence and 1,000 untracked files. External diff/textconv are disabled.
An unborn repository, unavailable evidence or exceeded bound fails capture before
publication. Ignored reports/assets require separately retained `--ref` evidence.

The anchor is immutable send payload and survives explicit Retry/replay. It adds a
short evidence annotation only to opt-in messages. Ordinary routing and envelope
budgets stay unchanged. The Service's comparison uses only its trusted Room project,
never an incoming anchor path; another checkout/namespace is unverified. CLI comparison
accepts an explicit operator-selected task checkout without changing the Room binding.

`unchanged_observation` means observed bytes/revision match, **not** an atomic snapshot,
a review verdict or execution approval. Files may change during/after observation;
finish edits or use an immutable commit/artifact before relying on a consequential
review. `stale` marks prior conclusions as targeting an older version. `different_workspace`
and `unverified` must not be interpreted as passing review.

For material findings, an ordinary short Markdown table is sufficient: finding,
evidence (revision/path/line or retained artifact), impact and disposition / unresolved
question. This is optional prose, not a required message schema or extra approval step.
When independent analysis is useful, give both sessions the same version and question
before sharing initial conclusions. Then exchange new evidence and disagreements;
return unresolved decisions to the user instead of forcing consensus. Do not create
a Run/Task/DAG or automatically authorize implementation when review finishes.

## Long-Room projection benchmark

Command: `GOMAXPROCS=4 go test ./internal/relay -run '^$' -bench BenchmarkNativeLongRoom -benchtime=100ms`.
Go 1.25.0, Linux amd64, AMD EPYC 9V74. Baseline: the audited main plus the identical
benchmark fixture. After: this change, median of three runs. Terminal history is
populated without disk I/O, with one queued message. These are projection microbenchmarks,
not end-to-end latencies, throughput promises, fsync measurements or model/token savings.
Baseline has one measurement; no statistical confidence claim is made.

| Terminal messages | Summary before | Summary after | Wake candidate before | Wake candidate after |
|---:|---:|---:|---:|---:|
| 1,000 | 48.2 µs | 1.00 µs | 22.0 µs | 0.103 µs |
| 10,000 | 579 µs | 1.22 µs | 293 µs | 0.109 µs |
| 100,000 | 9.22 ms | 1.22 µs | 5.28 ms | 0.111 µs |

Summary now includes additional diagnostic facts and uses 1,128 B / eight allocations
versus 784 B / six in the baseline; candidate lookup remains allocation-free. Runtime
queues/counters and publication ordinal indexes are replay-built, not separately
persisted. Claim, readiness, summary and wake use current work rather than terminal
history. Sorted pending-index updates may still cost proportional to active unresolved
work. Full retained messages/replay remain memory/history-sized; this is not log compaction.

## Acceptance map

| Review item | Implementation and deterministic evidence |
|---|---|
| 1. Wake liveness | Fake-clock two-way wake, deferred cooldown/hourly budget, no-new-traffic recheck, restart reservation, head cancellation, collector exit, runtime lease and no-retry tests. |
| 2. Hidden pending work | Old queued message after 301 later messages remains paged independently; cursor remains valid while earlier pending items settle. Native UI has independent pending controls. |
| 3. Refresh-safe send | Production JavaScript executes with fresh JS contexts and shared persistent storage; ID/payload recovery, storage failure, receipt matching and no automatic resend. Browser fault injection covers accepted-but-lost response. |
| 4. Native diagnosis | Authenticated Service and CLI tests; no model calls, credentials, session identity or task bodies in reports. Capability/approval evidence remains conservative. |
| 5. Targeted recovery | Message-ID, time and stable-cursor pages; auth, malformed query, text budget, receipt redaction and read-only sequence tests. |
| 6. Guidance consistency | README/skill/CLI/UI use actual runtime handles, conditional capability guidance and non-exact token claims; shared skill freshness and route/asset contracts. |
| 7. Current-work indexing | Deterministic randomized state transitions versus full-history oracle; replay, baseline/after benchmark. |
| 8. Review version | Git dirty/staged/new/symlink/cancellation/bounds tests, immutable same-ID payload, envelope/replay/Retry preservation and CLI comparison. |
| 9. Review/attention UI | One shared safe Markdown renderer, bounded/collapsible evidence, pending/history/diagnostic controls, explicit transport/human attention. Real browser checks remain separate from model acceptance. |

Keep four measurement categories separate: transport correctness, browser behavior,
actual vendor acceptance and paid quality/cost. The first two use deterministic
fixtures. The latter two require owner-authorized runs with pinned CLI/model/config,
same task/revision, raw usage when available, and counts of valid findings, false
positives, wall time and human interventions. Missing usage is unknown, never inferred
from envelope size. No new real-vendor or billed-cost benchmark was executed here.
