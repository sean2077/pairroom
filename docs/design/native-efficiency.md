---
status: accepted
updated: 2026-09-22
---

# Native coordination: efficiency and reliability boundaries

Native remains a coordination layer for two user-owned sessions, not a replacement
harness or an enforced multi-stage Workflow. This document records the design
reasoning and dated external research behind the native reliability review.
[Native relay](../NATIVE_RELAY.md) owns setup; [Protocol](../PROTOCOL.md) owns durable
semantics. Test results belong to the CI run for the reviewed commit, not to a
permanent claim that a moving branch or vendor release is verified.

## Keep the current ownership model

The useful split is: the vendor owns inference, tools, approvals, credentials and
session context; PairRoom owns participant identity, durable FIFO, explicit relay,
receipts and audit. The creator becomes Agent 1 by default without coupling the
slot to a runtime. Existing Room identities and saved pair profiles do not move.

For simple tasks, direct execution avoids coordination altogether. For complex
work, a concise explicit exchange lets the agents plan, implement and review
without making every task pass through a fixed pipeline. A new central scheduler,
vendor transcript mirror, terminal injector, or automatic retry of unknown work
would expand authority and failure modes without fixing the concrete defects
found in the native path. None is introduced by this review.

The Stop and foreground paths intentionally coexist. Stop is cheap when the
harness can surface its result directly, but has continuation and output limits.
Foreground `exchange` combines one publication with waiting inside one CLI
invocation. Neither a successful send nor a wake command proves the recipient
model saw the message, and exchange is not a correlated RPC transaction.

## Separate the costs instead of calling all of them tokens

| Boundary | Implemented policy | What it saves; what it does not prove |
|---|---|---|
| Model/tool round trips | Use status/reconcile on uncertainty or failure, not after every confirmed publication. Simple tasks remain direct. | Avoids a mandated diagnostic step; no claim about an exact vendor token bill. |
| Waiting | Renew successful empty transport polls inside the CLI. Keep one collector, and use background waiting only where completion is surfaced without model polling. | Waiting without inference need not create model turns; a vendor's tool timeout still applies. |
| Long native messages | `--text-file` reads existing UTF-8 without a model copy step; `--ref` sends path/size/hash, not contents; `--output-file` persists the envelope and prints a locator. Short replies stay inline. | Avoids read-and-retype and unconditional inlining. Not an upload, snapshot, or billed-token proof. Both sessions need file access. |
| Wake effects | Revalidate queue, binding generation, policy and collectors while reserving the effect durably; preserve rate limits and no automatic retries. | Avoids stale candidates running a vendor command. A collector arriving after reservation can still make one nudge redundant. |
| Browser history | Request `snapshot?tail=1`; copy at most 300 complete messages with a 1 MiB combined body/quote text budget and 80 audit records. Include complete history counts. | Bounds routine copying, JSON transport and DOM work. Metadata adds JSON overhead; this is not a total wire-byte cap. |
| Periodic maintenance | Track in-flight messages in a replay-built index and inspect only them in the one-second reaper and Busy query. | Claim, summary and wake selection now use replay-built current-work queues/counts; see the bounded long-Room benchmarks in native-review-closure.md. |
| Idempotent policy changes | Do not append unchanged park settings. | Avoids redundant Event Log facts and invalidations without hiding real changes. |
| Production dependencies | Keep the shared caller test seam behind a minimal interface, not an import of `testing`. Verify the production import graph. | Keeps test runtime/flag initialization out of the shipped CLI without changing caller detection. |

Full export, the unparameterized snapshot and explicit full-history diagnostics
remain intentionally complete. No log compaction, history deletion or schema
migration is required for these optimizations. History pagination is now read-only and bounded. A future bounded-memory replay
design still needs evidence of pressure and an explicit audit/retention
contract; routine browser windowing is not permission to discard durable facts.

## Recovery must not reinterpret history

Crash recovery and lease expiry use the same `delivering -> unknown` transition,
including the private original receipt. The original authenticated claimer may
settle that receipt after restart when no explicit Retry is pending. Wrong
receipts, revoked generations and conflicting Retry ownership still fail closed.
Public snapshots never expose the receipt.

An explicit send ID is an idempotency key for its accepted payload, not permission
to silently replace text, target, attachments or quoted context. Identical retries
return the original receipt without reopening attachment files; a later missing
file cannot undo acceptance. Actual new publication and collection continue to
validate attachment bytes. Different IDs can intentionally publish identical
text; Stop publication sequences retain their separate recovery contract.

A browser submission stays single-flight, including during upload. If acceptance
is uncertain, keep the original ID and uploaded attachment IDs and prevent edits
from changing the pending request. A failed upload before publication leaves the
draft editable. A fatal Event Log writer ends the event stream instead of leaving
an apparently healthy idle stream or creating a tight error loop.

## Vendor research, checked 2026-09-19

These are primary-source documentation findings, not newly executed vendor tests.
The older owner-authorized experiments in [Native relay](../NATIVE_RELAY.md#verified-vendor-wake-surfaces)
remain historical evidence for their recorded runtime and date.

**Claude Code.** [Channels documentation](https://code.claude.com/docs/en/channels)
describes MCP event push into an already-running session, with explicit per-session
`--channels` opt-in. It is a research preview with authentication, organization
and plugin-allowlist restrictions. PairRoom now uses the separately documented
[cross-session inbox](https://code.claude.com/docs/en/cross-session-messaging#the-sessions-inbox-socket),
not Channels, for local fixed-nudge submission to existing sessions. Capability
capture, inbound-policy limits and synthetic-versus-vendor evidence are documented
in [Claude inbox wake](claude-inbox-wake.md). Neither surface authorizes changing
user permissions or replacing the common FIFO.

**Codex.** [Hooks documentation](https://developers.openai.com/codex/hooks#large-hook-output)
states that oversized model-visible hook output normally spills to a file with a
preview at roughly 2,500 tokens; a spill failure can leave only a clipped preview.
`additionalContextLimit` applies to additional context, not every feedback path.
The onboarding skill therefore tells the agent to read the supplied full-output
file before acting on a preview. Increasing an unrelated limit or silently
replaying an acknowledged message is not a delivery fix. This reinforces the
existing distinction between stdout acknowledgement and complete model context.

**Grok Build.** [Skills, plugins and hooks documentation](https://docs.x.ai/build/features/skills-plugins-marketplaces)
describes project hook trust and reading Claude-compatible hooks and skills
alongside Grok's own configuration. Keep the existing single-owned hook
installation, native config-root selection, readiness-only Stop feedback and
explicit complete publication for clipped replies. A compatible hook is not
permission to install a second publisher or to claim an untested wake surface.

The research does not establish comparative billed cost, model quality, universal
idle wake, or all-version support. Those require owner-authorized vendor tests.
No preview channel or new paid vendor run is enabled here. Claude inbox capture
is an explicit native-binding integration, not a vendor API-key integration.

## Reproduce the engineering checks

The existing `make check` includes full Go tests, race checks, vet, prompt budgets,
skill projection freshness, JavaScript checks and dependency validation. Its
script discovery also runs `scripts/test_native_host_ui.js` against the shipped
client with a deterministic DOM/HTTP fixture. `make smoke` and `make browser-check`
cover Mock collaboration and real browser/HTTP/SSE behavior; neither is vendor E2E.

Focused regression commands:

```bash
go test -count=1 ./internal/relay ./internal/relayclient ./internal/protocol ./internal/service ./cmd/pairroom
node scripts/test_native_host_ui.js
go test ./internal/relay -run '^$' -bench 'BenchmarkNative(HistoryProjection|IdleReap)' -benchmem
```

The regression set covers first/second restart receipt recovery, changed-payload
ID reuse, stale wake authorization at the actual reservation boundary, fatal
stream termination, exact tail totals, complete export, detached/private
projections, in-flight terminal transitions, immutable browser retries and the
production dependency graph. The benchmark reports projection allocation and idle
maintenance cost; it is not a vendor inference benchmark. Preserve these checks
without turning timing measurements into flaky pass/fail thresholds.
