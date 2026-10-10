# Core concepts

PairRoom coordinates two coding sessions; it is not a model provider, replacement tool loop, or enforced workflow compiler. [Protocol](PROTOCOL.md) owns routing and transport contracts, [Architecture](ARCHITECTURE.md) owns implementation boundaries, and [Project language](../CONTEXT.md) defines canonical terms.

## Tool names and configuration layers

A **harness** is the coding application that runs a session's model/tool loop and owns its native instructions and permissions. The supported Runtimes are [Claude Code](https://code.claude.com/docs/en/overview), [Codex](https://github.com/openai/codex), and [Grok Build](https://docs.x.ai/build/overview). “Grok” in a Runtime selector means Grok Build, not a Grok model. You install and sign in to these applications yourself.

A **model** is the selected model identifier; a **Provider** supplies the account or API endpoint behind it. Picking a Runtime does not pick a model. [CC Switch](https://github.com/farion1231/cc-switch) is an optional external configuration manager: Embedded can read Provider references from it without switching its active Profile, and you need it only if you want per-slot Providers. Native Room selections never reconfigure the original session. See [Configuration](CONFIGURATION.md#cc-switch-provider-references).

A **terminal**, such as [WezTerm](https://wezterm.org/), is where a CLI harness runs; any terminal works. [Orca](https://github.com/stablyai/orca) is a separate Agent workbench compared in [Alternatives](ALTERNATIVES.md); PairRoom does not use it.

A **skill** supplies instructions for the Agent; a **hook** is a callback executed by its harness at a lifecycle event. Installing the `pairroom-relay` skill alone does not install or approve its Stop hooks. A **Service** is PairRoom's local backend; a **daemon** is the Service installed under the operating system's service manager. Neither decides a Room's host mode. [Native relay](NATIVE_RELAY.md) covers hooks and [Operations](OPERATIONS.md) covers the Service.

## Project, Room, and Runtime

A **Project** registers a canonical local Git workspace. A **Room** belongs to a Project and durably records two participant slots, collaboration instructions, Bindings, and messages. Neither registration nor Room creation copies the repository or creates a task worktree.

A participant's **Runtime** is Claude Code, Codex, Grok Build, or Gemini CLI. Either slot may use any supported Runtime, including the same Runtime twice. ActorIDs `slot1` and `slot2` mean Agent 1 and Agent 2, not particular vendors. Copy the displayed mention handle: unique Runtimes use `@claude`, `@codex`, or `@grok`; duplicate Runtimes use stable slot-order suffixes, such as `@codex0` and `@codex1`.

A **Room Runtime** is the active PairRoom component serving a Room, not the participant's harness. Suspending it does not delete the Room. Its process ownership depends on the immutable **host mode**:

| Boundary | Embedded (optional) | Native (default and recommended) |
|---|---|---|
| Vendor sessions | PairRoom starts/resumes supported adapters | Users run their original harness sessions |
| Configuration | Stored selections supply explicit per-process overrides | Stored selections are display-only; configure the original harness |
| Scheduling | One participant owns a native Turn at a time within the Room | Each slot has its own durable FIFO; Owner Turn is advisory |
| Permissions and interruption | Supported adapter controls appear in PairRoom | Approvals, permissions, and interruption stay in the original harness |
| Service capacity | Counts toward the active-runtime limit | Relay-only; exempt from the adapter-capacity budget |
| Colleagues on a LAN | Local only | Explicit new shared Rooms; each colleague keeps a local Service and native session |

New Room creation defaults to Native; Embedded must be selected explicitly. Existing Rooms retain their stored host mode. Host mode and collaboration mode are separate. A Room cannot switch host mode in place. An embedded Service inside the desktop application can serve both kinds of Room: “embedded Service” describes Service ownership, not a Room's host mode.

For [LAN collaboration](LAN_NATIVE.md), one Service owns the shared Room log and the colleague's Service connects to it. The guest has a local transport registration, not a second Room inbox. A new shared Room reserves the other slot as **awaiting teammate**, without guessing their Runtime or session. An exact accepted join receipt fills that slot with the actual Runtime. Each user keeps local filesystem and native permission authority; shared messages and explicitly uploaded evidence are the collaboration boundary.

## Binding and naming

A **Binding** associates a slot with a native session. In Embedded, a new Binding materializes its identity on first accepted native execution; an existing Binding must resume exactly. In Native, `relay bind` runs inside the intended session and associates immediately from its official session-ID environment. The approved Stop hook confirms that identity; it does not create the association. No nonce echo or initial Stop is required.

The Service checks native Runtime/session uniqueness across Rooms, including archived Rooms and duplicate-runtime slots. Archive does not release ownership. Native unbind/replacement revokes a binding generation but cannot undo work already handed to the original harness. Pre-binding vendor transcripts are not imported into the Room.

A Room name is mutable display metadata, not an ID. Renaming preserves the Room and Binding identities. Embedded rename uses a safe suspension boundary and applies supported native title metadata at activation; failed or unsupported title synchronization never substitutes a session. Native rename does not take control of the user's session title. See [API naming](API_REFERENCE.md#room-names-and-native-session-correspondence).

## Collaboration and permissions are separate

`default` lets the addressed Agent finish simple tasks directly. When another perspective helps, Agent 1 leads planning/review and Agent 2 implements, verifies, and contributes feedback. `custom` uses the user's natural-language rules instead. The Room saves this choice at creation; activation preserves its instruction version. Newer task instructions can redirect the current work, but do not rewrite the stored mode.

These responsibilities are not tool grants, mandatory alternating turns, or a plan-approval gate. Review completion alone does not authorize implementation.

**New Embedded Rooms default to YOLO for both participants:** the most permissive supported native permission profile, not a review-only mode. Select narrower supported native policies explicitly. Their independent Permission profiles (`configured`, `read-only`, `yolo`) can change only at an idle boundary with no queued work or pending approval. Runtime, Provider reference, model, and creation-time instructions remain immutable.

**Native permissions belong to the original harness.** A Room's displayed permission, model, or Provider fields do not change that process. Both modes use the chosen live workspace; neither creates a repository lock or isolates other Rooms, editors, or native subagents. Use project-owned worktrees and an agreed writer/review revision when coordinating changes. See [Configuration](CONFIGURATION.md) and [Security](../SECURITY.md).

## One native Turn owner

In **Embedded**, a native **Turn** may contain many model/tool events and several steered inputs. It is not one chat bubble. Cross-Agent work waits for a reliable terminal boundary in the Room-owned FIFO. Quiet output, a diagnostic error, or an HTTP receipt does not release ownership.

In **Native**, PairRoom does not schedule or preempt those Turns. Per-slot collectors share that slot's inbox, while both original sessions may work independently. Agree on a writer rather than assuming a relay receipt locks the workspace.

## Agent relay

An automatic relay requires the peer's exact current handle in the complete visible response. A peer handle wins over `@user`; `@user` alone returns a result or decision to the human. Code, URLs, email addresses, self-handles, and ambiguous duplicate handles do not route. Role names such as `@lead` and `@executor` are not handles.

The two host modes differ in what an unaddressed answer exposes:

| Publication | Result |
|---|---|
| Embedded answer without the peer handle | Visible in the Room; no peer relay |
| Native Stop with the peer handle | Complete addressed response enters the peer's FIFO |
| Native Stop with only `@user` | Complete response is published to the Room for the human |
| Native Stop without either routing handle | No reply body enters the Room log or inbox; only a publication receipt is recorded |
| Native explicit `relay send` / `exchange` | The command's target controls delivery; body mentions are ignored |

Explicit Native publication and automatic Stop publication are separate paths. Sending explicitly and then ending with another peer-directed reply can intentionally produce two messages. A same-ID receipt recovery is not permission to send again with a new ID. See [Native publication rules](NATIVE_RELAY.md#what-is-published).

Relay forwards the complete addressed response and attachments, not an accumulated copy of Room history. Each harness retains its own context. Peer claims are evidence to check, not proof of successful execution or user authorization. There is no automatic relay-count or cost ceiling; omit the peer handle when no useful independent response remains.

## Human controls and receipts

In **Embedded**, `steer` attempts supported same-target native steering; unavailable/rejected steering queues once. Explicit `queue` and cross-Agent messages wait for Turn ownership. Unknown native submission is never submitted again automatically: an unknown steer fails visibly, and an unknown Turn start keeps the participant's Turn until its runtime reports the input or the participant is stopped. Cancel removes waiting work; Interrupt after acceptance may affect the entire native Turn. Answer approvals only with the options and scope actually advertised by the harness.

In **Native**, the browser selects the receiving slot explicitly and queues a message. There is no PairRoom Interrupt or vendor-process start control. Cancel applies only to queued messages; uncertain deliveries require inspection before explicit Retry. Read-only history, diagnostics, and optional Git review versions do not claim, acknowledge, approve, or execute work.

Native `queued` means durable acceptance; `delivering` means a claim was persisted before envelope release; `handed_off` means CLI stdout was written and acknowledged, **not** that the model read or completed it. A Claude wake `submitted` or Codex wake `accepted` is also transport evidence, not model acceptance. See [Native recovery](NATIVE_RELAY.md#recovery-and-review-surface).

## What survives an interruption?

The Event Log retains durable facts; replay does not recreate a running model process.

| Host mode and state | Recovery |
|---|---|
| Embedded input still before native submission | Rebuild the Room FIFO |
| Embedded submission with uncertain native ownership | Fail for explicit inspection and Retry |
| Embedded accepted but unfinished input | Cancel without automatic replay |
| Embedded connection-local pending approval | Expire it |
| Native queued input | Keep it queued for the associated receiver |
| Native interrupted delivery | Recover as `unknown`; a matching original receipt may settle it while no explicit Retry is pending |
| Native `handed_off` input | Do not re-collect it merely because the model's response is missing |

Inspect workspace side effects before retrying. Closing a Room tab only closes a view; archiving a Native Room does not stop its user-owned sessions. Room backups exclude the Git repository, native session stores, and workspace relay credentials. [Storage](STORAGE.md) owns replay details; [Operations](OPERATIONS.md) owns shutdown and backup.
