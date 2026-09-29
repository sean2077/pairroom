---
status: active
kind: explanation
reviewed: 2026-09-29
---

# Why PairRoom?

**Two independent coding agents, one problem. Keep the harness; add a second opinion only when it earns its cost.** PairRoom is for repeated cross-review between two supported native sessions, not for replacing their execution engines with another agent framework.

A useful outcome can be just a reviewed plan. Once assumptions and material objections are resolved, the user can let either native agent execute with its own tools, skills, permissions, and subagents. PairRoom need not manage every implementation step. Review and implementation can also stay in the Room when that is useful; neither path is a mandatory pipeline.

This is a selection guide, not evidence that two agents are always more accurate or cheaper. [Alternatives](ALTERNATIVES.md) compares the Agent workbench [Orca](https://github.com/stablyai/orca), the native harnesses, and other products using dated primary sources. [Getting started](GETTING_STARTED.md#review-first-execute-where-it-fits) has reusable prompts.

## The problem it addresses

The recurring work is between two agents: carrying a proposal to the other session, returning a concrete objection, checking the revision against repository evidence, and knowing when another opinion is no longer useful. The point is not to maximize agent count or divide every task into parallel jobs. It is to improve one decision without making the human a message courier.

Two is a deliberate limit. Two participants can cover each other's gaps over the shortest communication path; more agents add coordination and token overhead that this job rarely repays. Each participant is an official harness ([Claude Code](https://code.claude.com/docs/en/overview), [Codex](https://github.com/openai/codex), or [Grok Build](https://docs.x.ai/build/overview)) with its own tools and subagents; PairRoom adds no model/tool loop of its own.

Automatic relay publishes an explicitly addressed complete response at the native Turn boundary, keeps the sessions' identities, and records delivery state. It does not make agents agree, prove a plan correct, or replace human product decisions. An independent review must add evidence, a counterexample, or a meaningful correction; agreement alone is not verification.

## Keep the harness, and choose the interaction surface

Keeping a native harness and keeping its original desktop/terminal UI are different promises. Choose the Room's immutable host mode accordingly:

| Need | Embedded Room | Native Room (recommended for daily work; experimental) |
|---|---|---|
| Where you interact | PairRoom's conversation and controls; adapters drive the supported native harness interfaces | Your own Claude Code / Codex / Grok Build sessions, including the intended Codex Desktop workflow; approved hooks bind them to the relay |
| Who owns execution | PairRoom schedules the two participants' Turns; each harness still runs its own tools and subagents | The original harness owns its process, tools, permissions, input and interruption; PairRoom does not launch or interrupt it |
| Provider / model / effort | Each slot independently selects supported overrides or inherits native configuration | Configured in each original harness; Room selection fields are metadata, not applied overrides |
| Delivery and control | Single Room Turn owner, FIFO, supported steering, queue, cancel, interrupt and explicit retry | Durable per-slot FIFO and binding audit; advisory Turn ownership, no process lock or Interrupt control |
| Important limit | Native tool execution does not expose every interactive vendor feature or preserve an independent Desktop UI | Automatic continuation is bounded by park; authenticated multi-round vendor E2E remains a release gate |

Native is the recommended mode for daily work: you keep interacting in your usual terminal or client, each side's work stays visible there, and you can stop or correct it directly. It remains experimental under the limits below. Embedded is the quickest first trial, including Mock, and the mode for independent per-slot Provider selection. In both modes the exchange between the Agents is visible in the Room.

A requirement to keep **Codex Desktop** is a reason to evaluate Native, not to claim Embedded is a transparent attachment to that application. Native supports Claude Code, Codex and Grok Build. Grok uses bounded Hook readiness hints and foreground collection to avoid clipped inputs; clipped Stop replies require explicit full-text publication. See [Grok Native](CLI_REFERENCE.md#grok-build-native). Hook parking and continuation have Runtime-specific bounds; beyond them, messages remain queued for a supported collector, an eligible wake, or human intervention. Neither `handed_off` nor synthetic hook tests prove model acceptance. See [Native continuation limits](NATIVE_RELAY.md#long-unattended-runs-by-runtime), [Protocol](PROTOCOL.md#native-host-protocol-v8), and [Support](../SUPPORT.md).

### Independent configuration without a Provider manager

PairRoom is not itself a Provider manager. Embedded selections can optionally reference supported [CC Switch](https://github.com/farion1231/cc-switch) Profiles without changing that external configuration manager's current Profile. CC Switch is not required when inheriting native configuration. References are read-only and revalidated; they do not freeze the external Profile or make unsupported authentication work. Save a usual pair as an [Agent pair profile](CONFIGURATION.md#agent-pair-profiles).

Native preserves the configuration chosen in each original session rather than injecting child-process overrides. Do not present Embedded Provider selection as a Native feature. PairRoom is not a credential store or a universal Provider marketplace. [Configuration](CONFIGURATION.md) owns the exact support boundary.

## Review together; leave execution to the chosen agent

The default Lead/Executor responsibilities are flexible instructions, not mandatory ranks or phases. Both participants can be high-capability reviewers. A user may ask them to challenge a plan, stop at a decision, and only later assign execution to either one. A custom Room can express that preference without introducing another mode.

```text
One problem -> proposal <-> evidence-based objections and revisions
            -> reviewed plan + remaining uncertainty -> user chooses execution
```

This is an example interaction, not a state machine. Relay requires the peer's exact displayed handle; `@user` without a peer handle returns the decision to the human, and no peer handle ends Agent relay. Do not mention the peer merely to acknowledge, agree, or ceremonially return a Turn.

Codex and Claude Code already provide native delegation/subagent capabilities; Claude also documents agent teams. PairRoom's reason to exist is not to recreate those mechanisms. It connects the two top-level sessions the user chose, while leaving native decomposition, tool use and subagent decisions to the executing harness. If native delegation already supplies the required second opinion, use it directly. See the [native-harness comparison](ALTERNATIVES.md#native-harnesses-the-default-alternative-to-adding-infrastructure).

After review, Native users can continue in the chosen original session without another peer relay unless requested. Embedded users can direct one participant in the Room. Moving that same Embedded session to an external harness requires ending/draining PairRoom's ownership first and verifying native resumption support; it is not automatic live attachment or mode conversion. Never operate the same session from two owners at once.

For discussion-only work, choose native read-only restrictions where supported. A prompt saying “do not implement yet” is not an enforced approval gate. Embedded new Rooms default to YOLO, the most permissive supported native permission profile; Native permissions remain controlled by the original harness.

## Keep the project's workflow and worktrees

PairRoom does not require a new worktree per agent or install its own task-branch manager into user projects. Keep repository instructions, existing scaffold and skills, tests, and PR/MR delivery policy authoritative; no particular scaffold is a PairRoom prerequisite. A Project registration is not a migration to another editor or directory layout.

A valid workflow starts both sessions at the primary checkout while edits and tests target one explicitly named directory such as `.worktrees/log-upload`. Opening a session at the primary checkout is not permission to edit it. Share the exact task path and revision with both agents so a reviewer does not inspect the primary checkout's older files by mistake. Tool permissions and hooks must permit the intended access; PairRoom does not bypass them.

Assign one writer when sharing a task worktree. Embedded's single-Turn ownership covers only its two participants, not other Rooms, native subagents, or external processes. Native has no enforced writer lock. Worktree creation, merge and cleanup should have one owner, following the project's existing rules. Do not run a cleanup helper that also merges/pushes as though it only deletes a directory.

## The native conversation loop

The Native-mode highlight is not “two agents can talk” but the shape of the loop: your addressed visible reply can be the transport. The approved Stop hook publishes the complete addressed reply into the Room's durable FIFO, mention handles route it, and no retelling, summary or acknowledgement turn is required on top. An unaddressed Stop reply remains private; explicit `send`/`exchange` follows its command target instead. The protocol's byte budgets still apply to what the receiving side injects. See [publication rules](NATIVE_RELAY.md#what-is-published).

Reachability across turn boundaries is layered: a bounded Stop-hook park collects immediate answers; outside it, a wake-enabled Room can use an available Claude inbox or Codex queue for a fixed body-free nudge. Foreground collection remains available, and a harness-owned background `relay wait` is useful only when the harness surfaces its completion to the model. Grok has no Service wake. PairRoom never starts or interrupts agent sessions, and queued messages survive restarts instead of being lost. [Native relay](NATIVE_RELAY.md#long-unattended-runs-by-runtime) owns the Runtime-specific limits, recovery, and [dated vendor observations](NATIVE_RELAY.md#verified-vendor-wake-surfaces).

`exchange`/`wait` poll and renew their lease inside the CLI process, so **waiting costs no model calls**. What a delivered message costs is another matter: wakes, continuations, native context, and the work the model then does all count, and there is no fixed per-message turn or token cost. Full peer replies, code reads, reasoning, and retries add up. Keep messages focused, and measure real usage from the Provider rather than inferring it from delivery receipts.

Dated working-session evidence (2026-09-16/17, Windows; Claude Code 2.1.273 + codex-cli 0.154.0, both authenticated): two native sessions ran a full delivery loop overnight without human relaying of message content — task delegation, four adversarial design-review rounds against a schema-migration draft, implementation of the approved design, independent line-level review, and merge — with zero message loss, and conservative peer refusals correctly gating two over-broad authorization interpretations. Working-session evidence; it does not replace the release-gate vendor E2E.

## What is actually lightweight?

The [protocol](PROTOCOL.md) keeps fixed identity/routing guidance in a compact bootstrap and sends sender, body, attachments and explicit user-quoted context in a dynamic envelope. It does **not** automatically append accumulated Room history, summarize the peer's response, or require a Task/Dispatch acknowledgement in every model reply.

Static tests cap the ordinary envelope overhead at **128 UTF-8 bytes** and bootstrap plus default collaboration at **1,800 bytes**, excluding the documented body/media/quote/custom-instruction costs. These are byte budgets, not token counts, cache-hit guarantees or billing measurements. Optional onboarding skill text is separate and also consumes context when loaded.

Both sessions retain their native context. Full peer replies, repeated code reads, native compaction, reasoning, tool results and retries still cost work. Keep follow-ups focused on changed assumptions, new findings and necessary evidence rather than repeating the entire plan. PairRoom does not truncate the reply for you. A more economical model or a smaller relay envelope does not guarantee a cheaper completed task.

The target is **accuracy, efficiency and acceptable total cost together**. Better convenience alone does not justify materially worse results or an unacceptable bill. There is no automatic relay-count or cost ceiling; choose a simpler single-agent path when peer review does not earn its overhead.

## Orca is a useful workbench, not an imaginary non-collaborator

In the [dated comparison](ALTERNATIVES.md#research-scope), Orca combines terminals, workspaces, notifications, review tools and an experimental structured orchestration layer. Its explicit messages, blocking ask/reply and existing-terminal reuse can support repeated review of the **same** problem, not just independent parallel jobs. It also supports externally created worktrees. Calling it “parallel only” would be incorrect. [Alternatives](ALTERNATIVES.md#orca-workbench-and-supervised-coordination-versus-a-pair-relay) documents the evidence and Provider distinction.

The fit question is whether you want that workbench and supervised task lifecycle. If you need two original sessions to review one proposal and then let a native harness execute normally, another Run/Task/Dispatch layer may add little value. PairRoom relays directly between the two Agents with no coordinating model, so the models see less coordination text, although no token or cost saving has been measured. This is a workflow preference, not evidence that Orca is slow, token-heavy, or unable to collaborate.

Adopting Orca's terminal/notification surface does not require adopting its orchestration or moving worktree ownership. Conversely, PairRoom Embedded sessions do not automatically become Orca-controlled terminal panes. Native can be evaluated with a compatible terminal host, but hook coexistence and recovery need real testing. Do not let two coordinators drive the same pair simultaneously.

## Choose another tool when it fits better

| Main need | Start with |
|---|---|
| One harness already finishes reliably, including its own subagents | That native harness |
| Only an occasional independent second opinion | Two existing sessions and manual relay |
| Many sessions, integrated terminals, attention management, workspaces and supervised tasks | [Orca](https://github.com/stablyai/orca) or another workspace-oriented workbench |
| A broad assistant, knowledge and Agent workstation | [Cherry Studio](https://github.com/CherryHQ/cherry-studio) |
| An architect/editor model split without two preserved native sessions | [Aider](https://aider.chat/docs/usage/modes.html) |
| Cloud/team execution or enforced workflow budgets and gates | A product with those explicit deployment and control guarantees |

PairRoom adds its own Service, bindings, storage and compatibility maintenance. It is not zero setup, a generic agent graph, a full editor, multi-user hosting, cloud sync, or a replacement for every native session feature. Its web listeners are local and loopback-only; the selected models may still receive code through their Providers. See [Security](../SECURITY.md).

## Expose uncertainty instead of silently repeating work

Delivery, execution and completion are different facts. Recovery preserves safe queued work and requires explicit decisions for uncertain delivery; it does not promise exactly-once execution or automatic rollback. Embedded and Native have different submission/collection boundaries, documented in [Protocol](PROTOCOL.md) and [Storage](STORAGE.md). Inspect side effects before Retry. The Event Log is a coordination record, not a tamper-proof compliance audit or a repository backup.

## How to decide whether it pays off

Compare the same repository revision, acceptance criteria, permissions, model/effort/Provider combination and comparable session freshness. Include one native agent, native subagents when relevant, manual two-session relay, PairRoom, and Orca when it fits. Evaluate Embedded and Native separately rather than pooling their different continuation behavior.

Record defects found and resolved, regressions, unresolved assumptions, human interventions, context-relay work, elapsed time, actual cached/uncached input and output usage, and Provider charges. Include failed runs and recovery. Repeat tasks and report variation; do not attribute a stronger model to the coordination tool. Native/provider accounting is authoritative where available, not a presumed complete PairRoom billing meter.

Separate “does this surface make the same pair easier to operate?” from “does a second agent improve the result enough to justify its work?” Mock tests establish neither comparative quality nor cost. No head-to-head Orca/PairRoom benchmark was performed for this document.

**Adopt PairRoom when an observable improvement in cross-review justifies the coordination layer. Keep execution and tooling choices where they already work.**

## Implementation evidence and maintenance

The PairRoom statements here were checked against [commit `42827c959c4760c4306e91d2c9b7d1c96d0bb283`](https://github.com/sean2077/pairroom/tree/42827c959c4760c4306e91d2c9b7d1c96d0bb283) by reading source and documentation; no new vendor or billing test was run. External comparisons keep the dates and revisions listed in [Alternatives](ALTERNATIVES.md#research-scope). Where this page and the technical references disagree, the references win.

| Claim | Implementation / contract entry |
|---|---|
| Flexible responsibilities, not a phase compiler | [Protocol](PROTOCOL.md), [collaboration regressions](../internal/room/collaboration_test.go) |
| Exact-handle relay and byte budgets | [Versioned protocol](../internal/protocol/contract.go), [Room Engine](../internal/room/engine.go) |
| Independent Embedded selections | [Selection model](../internal/model/agent_selection.go), [CC Switch boundary](../internal/ccswitch/) |
| Native process/configuration boundary and bounded continuation | [Native protocol](PROTOCOL.md#native-host-protocol-v8), [relay client](../internal/relayclient/) |
| Persistent state and explicit recovery | [Engine regressions](../internal/room/engine_test.go), [Storage](STORAGE.md) |

Revisit positioning when native harnesses or close competitors change. Product breadth, stars and marketing claims do not establish superiority; no single ingredient here is claimed exclusive to PairRoom.
