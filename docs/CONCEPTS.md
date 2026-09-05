# Core concepts

## Project, Room, and Binding

**Project** is the registration record of a local Git repository in the Management Service. It stores the repository location and the Rooms that can be created there. It is not a copy of the repository and does not own user source.

**Room** is the persistence boundary for collaboration. It holds participant roles, messages, Turn summaries, approvals, Bindings, attachments, and the Event Log. A Room can continue across process starts, but the native process itself is not restored with the Event Log.

**Binding** binds a stable Room participant slot to a vendor-native session or thread. Durable ActorIDs `claude` and `codex` still identify Agent 1 and Agent 2 internally; `RuntimeKind` independently selects Claude Code, Codex, or Grok Build. A Binding is managed by the Service and cannot be reused freely by another active Room.

## Public identity and mention handles

Public identity follows the runtime currently assigned to each slot. When a runtime occurs once, its display name and handle are `Claude Code` / `@claude`, `Codex` / `@codex`, or `Grok Build` / `@grok`. When both slots use the same runtime, stable slot order adds zero-based suffixes everywhere: for example `Codex 0` / `@codex0` for Agent 1 and `Codex 1` / `@codex1` for Agent 2. Changing a runtime may therefore change public identity without changing the durable ActorID or Binding owner.

An unsuffixed handle for a duplicated runtime is ambiguous and does not route. PairRoom reports the two exact handles to use. Handle matching is case-insensitive. Mentions inside fenced code, inline code, URLs, and email addresses are ignored.

## Message and native Turn

A Message is PairRoom's auditable input or output. A native Turn is one complete round actually executed by Claude Code, Codex, or Grok Build. They are not one-to-one:

- one Turn can receive multiple accepted steering messages;
- a queued message may start a new Turn later;
- successful transport delivery does not mean the Turn completed;
- an ordinary diagnostic error does not necessarily mean the Turn terminated.

PairRoom releases the owner only at a reliable boundary, such as native `turn/completed`, a confirmed process exit, or an explicit cancel or abort termination event.

## FIFO and single ownership

```text
idle
  -> reserve one FIFO item
  -> start one native Turn
  -> optionally accept same-Turn steer for that same Agent
  -> reliable terminal boundary
  -> release owner
  -> reserve the next FIFO item
```

Invariants:

1. A Room has at most one active native Turn owner;
2. Cross-Agent input starts only after the current Turn ends;
3. A queued item is checked for cancellation again before the native submission boundary;
4. an adapter never owns a hidden second queue; the Room FIFO is the sole queue;
5. a generic runtime error updates diagnostics but does not release ownership by itself;
6. a newer user instruction cancels an older not-yet-started Agent relay without implicitly interrupting the active native Turn.

## Message intent

- `steer` is the default. When the target already owns the active Turn, PairRoom asks its adapter to steer that Turn. `accepted` enters the Turn; `unavailable` or `rejected` moves the same Message into the Room FIFO exactly once; `unknown` fails visibly and requires an explicit Retry so PairRoom cannot duplicate uncertain work.
- `queue` always enters the Room FIFO when another Turn is active. When the Room is idle, it starts immediately.

Input to the other Agent is always queued until the active Turn ends, regardless of intent. A user who wants to stop current work uses Cancel or Interrupt explicitly.

## Agent relay and convergence

Only the exact current handle of the other Agent in visible Agent output requests a relay. PairRoom stores and forwards that complete visible response and its attachments after the current native Turn completes. It does not truncate the response, append Room history, or require a structured handoff packet.

A response without the peer's exact handle ends Agent relay. Either Agent may deliver the final result. An exact Agent handle wins over `@user` in the same response; `@user` alone leaves the Room waiting for the user; a self-mention never routes. Former aliases such as `@peer`, `@human`, `@all`, `@agent1`, and `@agent2` do not route; an otherwise unaddressed user send that relies on one is rejected. Former `PAIRROOM` control markers are ordinary text.

PairRoom applies no hop or Turn ceiling to explicit relays. The bootstrap tells each Agent to mention the peer only when another independent response can materially complete the user's request, not to acknowledge, agree, thank, or ceremonially return a Turn. If Agents deliberately keep naming one another, the user can Cancel, Interrupt, or send a newer instruction.

## Role and Workspace

- **Driver** implements in the live workspace;
- **Reviewer** inspects in an isolated reviewer snapshot and does not modify the Driver's live tree by default;
- **Peer** is an equal collaborator operating within its native permissions and current workspace boundary.

Roles are runtime permission and workspace boundaries, not just prompt labels. Switching roles must happen at a safe Turn boundary.

## Approval and human questions

Native permission requests remain visible Room approvals and retain the vendor's available decisions. A request ID belongs to one live native connection, so unresolved approvals expire across process restart.

When a headless runtime cannot continue an interactive question in place, the generic human-input bridge emits the question visibly with `@user`, interrupts that native Turn, and lets the user's reply start a new Turn. PairRoom does not hide a native prompt or reintroduce a workflow layer.

## Restart and recovery

The Event Log, Binding, Messages, and Room-owned FIFO states are durable. On restart:

- FIFO entries that never crossed the native submission boundary are restored in original order;
- an entry caught in the native submission acceptance window is marked failed with explicit Retry guidance, because automatic replay could duplicate execution;
- input already accepted by a native runtime is not replayed and its unfinished processing is cancelled;
- pending approvals expire;
- the user inspects repository state before retrying any uncertain or accepted work.

This distinction preserves queued work while failing closed wherever native ownership cannot be proven.
