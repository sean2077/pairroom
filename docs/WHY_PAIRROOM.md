---
status: active
kind: explanation
reviewed: 2026-09-08
---

# Why PairRoom?

**Choose PairRoom when the recurring problem is coordinating two coding agents on one task, not finding another place to chat with a model.** It puts planning, implementation, peer review, user decisions, and execution state in one local Room while the selected native harnesses still do the coding.

This is a selection guide, not a claim that two agents outperform one. For dated competitor evidence, read [Alternatives](ALTERNATIVES.md). For the shortest runnable path, read [Getting started](GETTING_STARTED.md).

## The problem it addresses

A developer already using two coding CLIs can ask one to plan and the other to implement. The repetitive work is in between: carrying the latest answer across terminals, telling the second agent when the first has finished, checking which session belongs to which task, passing review findings back, and deciding whether a quiet or interrupted process really finished.

PairRoom makes that coordination explicit. A Room binds two native sessions, admits only one participant's native Turn at a time, relays an explicitly addressed complete response at the Turn boundary, and records messages and delivery state. The intended benefit is less **human coordination work** and clearer execution evidence. Whether that benefit exceeds setup time, extra model work, and maintenance is something to measure on your tasks.

## What users get

| Need | PairRoom mechanism | Boundary |
|---|---|---|
| Use a planner and an implementer with different strengths | Two independently selected Runtimes, Providers, models, effort levels, and additional instructions | No automatic model ranking, price optimizer, or demonstrated cost saving |
| Keep existing coding tools | Adapters for native Claude Code, Codex, and Grok Build; either Runtime may occupy either slot, including the same Runtime twice | Supported adapter interfaces, not a promise that every interactive CLI feature is exposed |
| Work through one change together | Default Lead/Executor instructions, or custom natural-language collaboration rules | Instructions guide agents; they are not an enforced phase machine |
| Avoid the two Room participants starting overlapping Turns | One native Turn owner and one Room FIFO | Not a repository-wide lock, process sandbox, or guarantee against a harness's own parallel tools/subagents |
| Inspect and redirect execution | Shared conversation, Turn/tool activity, native approvals, steering, queue, cancel, interrupt, and explicit retry | Native steering support varies; human supervision remains necessary |
| Retain a task's coordination record | Durable Room, native-session Binding, Event Log, attachments, and recovery rules | Does not restore a running process or import all pre-existing native conversation history |

Mechanics are specified in [Concepts](CONCEPTS.md), [Protocol](PROTOCOL.md), [Configuration](CONFIGURATION.md), and [Storage](STORAGE.md). These are implementation-backed capabilities, not uniqueness claims: other tools also have agents, queues, approvals, or persistent sessions.

### Keep the harness, change the coordination layer

A model is not the same thing as its harness. The harness supplies tools, repository instructions, native session handling, configuration, and permission behavior. PairRoom coordinates selected native harnesses rather than implementing another general-purpose model/tool loop.

This matters when the reason for choosing a second agent is not merely its model name, but its existing CLI workflow or native session. It also creates a maintenance cost: upstream protocol changes can break an adapter. Verify the chosen CLI and Provider combination before relying on it; [Support](../SUPPORT.md) describes that boundary.

Native configuration remains the default source for unspecified overrides. Supported CC Switch references are read-only, resolved for creation and activation, and do not change CC Switch's current Profile. A saved reference does not freeze the external Profile's contents. PairRoom is not a Provider manager and does not make an unsupported credential/protocol combination work.

### Make peer review convenient, not ceremonial

The Lead can request implementation, inspect the resulting diff and test evidence, and request a correction only when it changes the outcome. The Executor can challenge an unsupported plan instead of acting as a blind file editor. Both operate on the same live workspace in a modern Room.

A second context can offer another perspective, but agents can share blind spots and repeat one another's mistakes. A review should identify a concrete defect, examine a change, or check evidence; agreement alone is not verification. For a trivial edit, involving the peer may add cost with no useful benefit. Either agent can finish without another relay.

The model-facing relay contains the complete visible response and attachments, not an automatically appended Room transcript. That avoids one source of repeated context, but does **not** establish lower billed tokens: both native sessions have their own context and additional Turns consume work.

### Expose uncertainty instead of silently repeating work

Delivery, execution, and completion are different facts. After restart, PairRoom can restore queued input that never crossed the native submission boundary. Work caught in an uncertain submission window fails for explicit Retry; accepted unfinished work is not automatically replayed. Pending connection-local approvals expire.

This is valuable when duplicate execution would be worse than stopping for inspection. It is not exactly-once execution, automatic rollback, or uninterrupted unattended recovery. Inspect repository side effects before retrying. The Event Log is a coordination record, not a tamper-proof compliance audit or a backup of your Git repository and native session stores.

## A representative use case

Create a **default** Room with the planning/review configuration in Agent 1 and the implementation configuration in Agent 2. Both may use the same Runtime, or different supported Runtimes. A possible first task is:

```text
Find the smallest change that fixes this bug. Delegate implementation and
verification to your peer, then review the actual diff and test evidence.
Challenge unsupported assumptions. Ask me about unresolved product decisions.
Stop when the result is complete; do not exchange acknowledgement-only replies.
```

An example outcome is:

```text
Human request -> Lead plan -> Executor change and tests
              -> Lead review -> Human receives result
```

This sequence is an example, **not a scheduler guarantee**. The agent must use the peer's exact displayed mention handle to request another Turn. A response without that handle ends Agent relay; responsibilities such as Lead are not routing aliases. A human can redirect the task without editing the saved collaboration mode.

For a discussion-only task, select native read-only permissions for both participants. Saying “plan first” is not equivalent to an enforced human-approval gate. New Rooms default to YOLO for both participants; select narrower policies explicitly. A natural-language instruction to stop is not a budget limit or a sandbox.

## Choose another tool when it fits better

| Your main need | Start with |
|---|---|
| One agent already completes the task reliably | That native CLI; use its subagents when appropriate |
| An occasional second opinion | Two existing sessions and a manual relay |
| A broad assistant, knowledge, document, and Agent workstation | Cherry Studio |
| Architect-model proposals translated into edits without preserving two native harness sessions | Aider's architect/editor mode |
| Many independent tasks, isolated worktrees, multiple repositories, and integrated change review | A workspace-oriented tool such as Vibe Kanban |
| Cloud execution and collaboration across people | A product designed for that deployment, such as Conductor's documented cloud offering |
| Enforced phase gates, delegation budgets, or unattended multi-step business automation | A workflow system with those explicit guarantees, not PairRoom instructions alone |

These are task-fit recommendations, not statements that competitors lack all other capabilities. In particular, Cherry Studio is **not chat-only**, and native coding harnesses already support multi-agent work. See the [dated comparison](ALTERNATIVES.md).

## Costs and limits to accept up front

PairRoom adds a Service, Room state, adapters, and UI to tools you could run directly. It currently has exactly two participant slots per Room. Room creation fixes collaboration and Agent selection; changing only the effective permission profile later does not change the model, Provider reference, or responsibility. It does not implement an arbitrary agent graph or a provider-neutral replacement for every native session feature.

Sequential ownership favors dependent implementation/review work over parallel throughput. Its scope is the two participants in **one Room**. Other Rooms, external tools, and native child processes are not isolated by this rule. Use separate checkouts/worktrees and explicit integration when independent tasks may write concurrently.

There is no automatic relay ceiling or cost circuit breaker. Agents can continue mentioning each other; Cancel, Interrupt, and a newer human instruction are the available controls. With YOLO defaults, lower approval friction is also a larger trust commitment, not a security advantage. Read [Security](../SECURITY.md) before using untrusted code or broad tool access.

The Service is local and loopback-only. That is useful for a single-user local workflow, but not multi-user hosting, remote workers, RBAC, or built-in cloud sync. Cloud-model requests still travel through the selected native CLI to its Provider. Local coordination does not mean offline inference or that code never leaves the machine.

## How to decide whether it pays off

Compare representative tasks from the **same repository revision**, with the same acceptance tests and explicit permission constraints. Include these baselines: one native agent, two native agents with manual relay, and PairRoom. Where relevant, add the native subagent or workspace tool you already use. Record the actual CLI versions, models, effort, Provider, context/session freshness, and task budget; do not attribute a stronger model's improvement to the coordination UI.

Measure completed acceptance criteria, regressions, human interventions, time spent relaying context, elapsed time, review findings that changed the result, and actual Provider usage/cost. Include failed runs and recovery attempts, not only successful demonstrations. Repeat tasks and report variation. Use native/provider accounting where available; do not assume PairRoom has a complete cross-provider billing meter.

Separate two questions: does the UI reduce coordination effort for the **same pair**, and does adding a second agent improve the overall workflow enough to justify its work? Mock tests answer neither question about model quality. They check control-plane behavior, not comparative productivity.

**Adopt PairRoom when it removes recurring coordination work you can observe. Skip it when it only adds another layer to a workflow that already works.**

## Implementation evidence and maintenance

This explanation was checked against PairRoom commit `94dbc6e1add9a7d66eadb89d4779304a7cdc0714`. Current technical documents and executable tests take precedence when behavior changes.

| Claim | Implementation / contract entry |
|---|---|
| Creation-only responsibilities, shared workspace, independent permission profiles | [Collaboration regressions](../internal/room/collaboration_test.go), [permission transitions](../internal/room/permissions.go) |
| Explicit relay, native authority, no ceremonial turns | [Versioned protocol](../internal/protocol/contract.go), [Room Engine](../internal/room/engine.go) |
| Independent Agent selection and native adapters | [Selection model](../internal/model/agent_selection.go), [adapters](../internal/agent/), [CC Switch boundary](../internal/ccswitch/) |
| Persistent state and explicit recovery | [Engine regressions](../internal/room/engine_test.go), [store](../internal/store/), [Storage](STORAGE.md) |
| One desktop host over the same Service | [Desktop host](../desktop/internal/host/host.go), [Operations](OPERATIONS.md#desktop-lifecycle) |

Revisit positioning when a native harness or a close competitor changes its collaboration model. Do not preserve a comparison merely because it once favored PairRoom, and do not turn possible future capabilities into present-tense product claims.
