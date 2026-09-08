# Core concepts

PairRoom coordinates native coding sessions. It is not a model provider, a replacement tool loop, or an enforced workflow compiler. This page defines the user-facing behavior; [Protocol](PROTOCOL.md) owns exact model-facing routing rules, and [Architecture](ARCHITECTURE.md) owns implementation invariants. Canonical terminology is in [Project language](../CONTEXT.md).

## Project, Room, and Runtime

A **Project** registers a canonical local Git repository. A **Room** is a durable collaboration context belonging to a Project, containing two participant slots, messages, session Bindings, and saved collaboration instructions. A **Room Runtime** is the active set of processes and HTTP surfaces serving that Room. Suspending a Runtime does not delete the Room.

A participant's **Runtime** selects Claude Code, Codex, or Grok Build. Either slot may use any supported Runtime, including the same Runtime twice. Historical ActorIDs `claude` and `codex` mean Agent 1 and Agent 2 in persisted data; they do not require those vendors to occupy the slots.

Public display names and mention handles come from the selected Runtimes. Unique Runtimes use `@claude`, `@codex`, or `@grok`; duplicates use stable slot-order suffixes such as `@codex0` and `@codex1`. Copy the handle shown in the Room rather than inferring it from an ActorID or responsibility.

## Binding and naming

A **Binding** associates a participant slot with a native session. New Bindings materialize when native execution creates the session; existing Bindings must resume their exact native identity. The Service owns Binding uniqueness, including archived Rooms. Native login/configuration and the pre-binding transcript remain owned by the native CLI; PairRoom does not import the earlier transcript.

A Room name is mutable display metadata, not an ID. A missing creation name is generated once. Rename waits for a safe boundary, suspends the Runtime without interrupting the Turn, and records the new name. The next activation applies a derived native session title where supported. A desired name or failed title synchronization does not replace the native session ID. See [API reference](API_REFERENCE.md#room-names-and-native-session-correspondence).

## Collaboration and permissions are separate

| Creation choice | Responsibility |
|---|---|
| `default` | The addressed Agent completes simple tasks directly. When collaboration helps, Agent 1 leads planning and review; Agent 2 handles implementation, verification, and technical feedback. |
| `custom` | User-supplied natural-language rules replace the default responsibility instructions. |

The Room saves this choice at creation and restores it on activation. These instructions do not enforce a particular number or order of Turns, establish a plan-approval gate, or grant tools. A user can redirect the current task, but cannot edit the stored mode or immutable Agent selection inside an existing Room.

Modern participants both use the live workspace. New Rooms default to **YOLO** for both; select narrower native policies explicitly. The independent Permission profile can be `configured`, `read-only`, or `yolo`, changed only when both participants are idle, the FIFO is empty, and no approval is pending. It does not switch responsibility, model, Provider reference, or native identity.

Legacy Rooms retain their old role/workspace boundaries, including the legacy Reviewer snapshot where applicable. They are not silently converted to YOLO. Public role switching and role-based addressing are removed. See [Configuration](CONFIGURATION.md), [Security](../SECURITY.md), and [Upgrading](UPGRADING.md).

## One native Turn owner

A native **Turn** can contain many model/tool events; it is not one chat bubble or one tool call. PairRoom permits one participant's native Turn at a time **within a Room**. Cross-Agent input waits in the Room's FIFO until a reliable terminal boundary releases ownership.

This is not automatic A/B/A/B rotation. The current Agent may finish all useful work and answer the user. A diagnostic error, lack of recent output, or HTTP submission receipt does not prove its native Turn has ended.

The ownership rule does not create a per-Room Git worktree, lock the repository against external writers, isolate other Rooms, or disable native tool/subagent concurrency. Arrange separate checkouts/worktrees and explicit integration for independent writing tasks. Native permissions and host isolation remain separate concerns.

## Agent relay

After the current native Turn ends, an Agent's complete visible reply is relayed only when it contains the other participant's exact current handle. Code, URLs, email addresses, ambiguous duplicate handles, and self-handles do not create a peer relay under the [Protocol](PROTOCOL.md#output-routing) rules.

No exact peer handle means Agent relay ends. `@user` alone returns the decision to the human; an Agent handle in the same reply wins over `@user`. Neither `@lead`/`@executor` nor historical role aliases are routing handles. An unaddressed human message starts Agent 1 in a modern Room.

Only the current complete peer response and its attachments are forwarded, not an appended copy of all Room history. Each native session retains its own context. A peer's claim is input to verify, not proof that a command succeeded or that the user approved an action.

There is no automatic relay-count ceiling. Agents are instructed not to continue for acknowledgement, thanks, or ceremonial turn return, but the human remains the active circuit breaker. Do not confuse these instructions with a cost budget or an unattended-completion guarantee.

## Human controls and receipts

| Action or state | Meaning |
|---|---|
| Send / `steer` | Default intent. Same-target input may join the active Turn if the adapter confirms native steering. |
| `queue` / cross-Agent input | Wait for ownership in the Room FIFO. No hidden competing adapter queue. |
| Steering unavailable/rejected | Queue the same input once. An unknown submission result instead fails visibly for explicit recovery. |
| Cancel while waiting | Remove that FIFO item without clearing unrelated queued input. |
| Interrupt after native acceptance | May stop the entire active native Turn, including multiple inputs absorbed into it. |
| Retry | Create a new auditable Message for an unsuccessful terminal target; do not mutate or blindly resubmit the old one. |
| Native approval | Answer the actual advertised request and scope; it is not an approval for an arbitrary PairRoom plan. |

Support for steering differs by Runtime. Codex uses native Turn steering, Claude's current adapter reports it unavailable, and Grok uses its supported interjection extension. See [Architecture](ARCHITECTURE.md#native-adapters) for adapter boundaries.

A newer human instruction can cancel stale not-yet-started Agent relays. A send receipt means input was recorded/admitted, not that a change was made or a Turn completed. Inspect delivery/processing state, Turn summary, and actual repository/test evidence. Concurrent pending retries of the same source/participant are rejected; this is not an exactly-once guarantee. Wire details belong in [API reference](API_REFERENCE.md).

## What survives an interruption?

The Event Log retains Room facts and auditable state. Current process connections, transient token deltas, native request IDs, and the active owner are not durable processes you can resume by replaying the UI.

| State at restart | Recovery |
|---|---|
| Room-owned input that never crossed native submission | Rebuild in FIFO order |
| Input caught in `submitting` with unknown native ownership | Fail for explicit Retry after inspection |
| Unfinished input already accepted by the native Runtime | Cancel without automatic replay |
| Connection-local pending approval | Expire rather than reusing the response |

Inspect repository side effects before retrying uncertain work. A backup of Room data does not include the repository or all native session stores. [Storage](STORAGE.md) is the authority for replay/schema details; [Operations](OPERATIONS.md) covers backup and shutdown.
