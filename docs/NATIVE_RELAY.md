# Native relay: setup, usage and reliability

Native hosting connects two sessions you already run. PairRoom does not launch, configure, or interrupt their processes. This guide owns the user workflow; [CLI reference](CLI_REFERENCE.md#native-relay-commands) owns command options, [Protocol](PROTOCOL.md#native-host-protocol-v8) owns the transport contract, and [session workspace discovery](NATIVE_SESSION_WORKSPACE.md) owns cwd/worktree resolution. The app includes a Native setup guide as well.

## Before starting

Install PairRoom and Git. Open Desktop or run `pairroom service`, but do not start a second Service over the same data directory. In **each Agent's tool shell**, verify `pairroom version` and `git --version`. Opening Desktop alone does not prove that its CLI is on that shell's PATH. Use the CLI from the same PairRoom release as the running Service; see [Installation](INSTALLATION.md). Restart existing shells after changing PATH.

Install and authenticate the harnesses you intend to use: Claude Code, Codex, or Grok Build. Either slot can use any supported Runtime, including the same Runtime twice. Configure Provider, model, effort, tools, and permissions in the original harnesses; Native Room selections do not override those processes.

## One-time project setup

Run `relay install` from any terminal in the Project's worktree; installation itself does not require a native session:

```bash
pairroom relay install --runtime claude,codex
# Select only the runtimes you use; all three are also supported:
pairroom relay install --runtime claude,codex,grok
```

Without `--runtime`, a recognized session supplies its Runtime; an interactive terminal prompts a multi-select, and non-interactive use fails with guidance rather than waiting. `cc` is accepted as a Claude installation alias.

Codex uses `.codex/hooks.json`; Claude Code uses `.claude/settings.json`. Grok Build normally reuses the Claude Code project hook, avoiding two Stop commands. A Grok-only installation without that hook uses `.grok/hooks/pairroom.json`. When Claude hook compatibility is disabled, install Grok with that compatibility off so its own hook is written: PairRoom derives this from `GROK_CLAUDE_HOOKS_ENABLED=false` in the installing environment and never reads Grok's `config.toml`, so set that variable alongside Grok's `[compat.claude] hooks = false` and the two agree. Reinstallation removes only a redundant PairRoom-owned Grok Stop command when the Claude hook covers it, preserving unrelated hooks.

Review and approve the exact installed definitions in the harness: Codex `/hooks`, Claude project hook consent, and Grok hook approval plus folder trust. Follow native trust/restart guidance; PairRoom never grants consent for you.

Before binding, run `pairroom relay preflight` in each Agent session (or any shell in the Project). It checks, without changing anything, that the bare `pairroom` command resolves on that shell's PATH, the Service is reachable and from the same release, and the Stop hook is installed, then prints ordered `next_steps` and exits nonzero until setup is ready. It cannot see approval; the first finished turn's `last_hook_at` in `relay doctor` confirms that.

Installation also writes the relay skill. Skill roots honor `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, and `GROK_HOME`; project hooks stay project-local. The optional `npx skills add sean2077/pairroom` route installs the skill, **not** the hooks or their approval. That route needs Node's package runner; the Go relay CLI does not.

## Create, join, collaborate

Inside the first native session, invoke `/pairroom-relay <topic>` with the skill loaded, or ask its Agent to run:

```bash
pairroom relay bind --create --name "<topic>"
```

Pass the printed `peer_join_local` command to the other session using the same Service, or use `peer_join` with its explicit paths. Ask the second Agent to execute it as a tool call. Do not strip a custom `--service-file` from the generated command. An explicit Room resolves its registered workspace without requiring a preliminary `cd`.

Alternatively, create a **Native** Room in Management and bind the two sessions to it. Do not also run `--create`. When exactly one matching active Room/slot is available, `pairroom relay bind` needs no selectors; otherwise follow the candidate list. Slots are Agent 1/2 (`--slot 1|2`), not vendor names. The creating session becomes Agent 1 by default; existing Rooms are not reordered. Creation uses the detected caller and the Service's selected pair; `--peer-runtime` is available when an explicit peer choice is needed.

Each bind associates immediately from the official tool-call environment: `CLAUDE_CODE_SESSION_ID`, `CODEX_SESSION_ID`, or `GROK_SESSION_ID`. Run it **inside the intended Agent session**, not a detached terminal or Grok's user shell mode (`!`). Missing or conflicting identity fails closed; do not manufacture an environment value. Bind also checks the installed hook. No nonce echo, initial Stop, or status check is needed to unlock confirmed relay. Reuse the existing binding for later reviews.

If creation succeeded but bind failed, finish the printed recovery command for that Room instead of creating another. If the bind response was lost, rerun bind for the same Room/slot without `--create` or a new `--replace`. Explicit replacement is for an intentional session change and cannot stop work in the old harness.

## What is published

Automatic Stop publication and explicit CLI publication have different routing rules:

| Path | Routing and visibility |
|---|---|
| Stop reply containing the exact peer handle | Publish the complete reply to the peer's FIFO; a peer handle takes priority over `@user` |
| Stop reply containing only `@user` | Publish the complete reply for the human in the Room |
| Stop reply with neither routing handle | Record only a sequence/idempotency receipt; **do not copy the private reply body into the Room log or inbox** |
| `relay send` / `exchange` | Use the command's target (peer by default, `send --to @user` for the human); body mentions do not route |

Use the actual handle returned by bind or displayed in the Room. Duplicate runtimes have stable suffixes such as `@codex0` and `@codex1`; an unsuffixed duplicate is ambiguous. Code/URL/email mentions and self-handles do not route. [Protocol](PROTOCOL.md#output-routing) owns exact matching rules.

Publishing and receiving are independent: a Stop that does not publish a body may still park and receive **already queued** input. This does not deliver an unaddressed peer Stop reply. Explicit send can queue an unaddressed body because its target is supplied separately.

Do not publish the same report explicitly and then repeat it in a peer-directed final answer unless a second message is intentional. Neither path deduplicates by body. A confirmed receipt contains IDs/state, not another copy of the report.

## The two receive paths share one mailbox

An approved Stop hook publishes first, then may park for up to 30 seconds within its installed 45-second timeout. It parks only while a peer reply is expected — this session recently addressed its peer and the peer has not answered yet — so an ordinary or `@user` turn ends without an idle half-minute. Claude/Codex can request continuation with an actual envelope, up to eight consecutive blocks. Grok's clipped hook-feedback channel instead returns a bounded readiness instruction: the full input stays queued until foreground `wait` collects it. Grok readiness/recovery hints stop at seven to reserve its final publication gate; other hooks share the vendor budget.

Within that existing hook lifetime, sender publication/reconciliation has an eight-second budget, including at most two seconds for optional transcript metadata confirmation. A slow sender request therefore leaves time for receive-side park and acknowledgement. Timeout retains the original pending publication identity; it does not authorize replay or imply that the Service rejected it.

For an explicit discussion, start the receiver with `pairroom relay wait`, then in the other session run:

```bash
pairroom relay exchange --id review-1 --text "Review this proposal; do not implement"
```

`exchange` sends once and collects the next eligible FIFO input; `wait` only collects. The result may be an earlier queued message or user instruction, not the answer to this particular send. Send plus receive is not atomic and does not prove task completion.

Both commands default to one hour. Finite `--timeout` values allow up to six hours; `--timeout 0` has no PairRoom total deadline. Successful empty HTTP long polls renew inside the CLI, at most 30 seconds per poll, without calling a model. Authentication/transport errors, cancellation, native tool limits, or shutdown still end the wait. A process-owned collector lock prevents another local collector from stealing the same slot's next input; a busy collector does not prevent Stop publication.

A confirmed send followed by an empty wait timeout calls for **receive-only `wait`**, not a new send. Finish with an explicit final send when appropriate, without another ceremonial wait or duplicate peer-directed Stop reply. See the [foreground discussion loop](CLI_REFERENCE.md#foreground-discussion-loop).

## Long reports and file evidence

Keep short decisions inline. For an existing full report, pass it directly instead of asking the model to read and retype it:

```bash
pairroom relay send --id report-01 --text-file "/absolute/task/report.md"
```

`--text-file -` reads stdin. Repeatable `--ref PATH` appends a local path/size/SHA-256 reference **without uploading file contents**; the receiver needs access to those retained bytes. Relative paths resolve from the actual tool-call cwd, not the binding workspace. References are evidence, not permission to read arbitrary paths.

`wait` / `exchange --output-file NEW_PATH` can persist the complete incoming envelope and return a locator. Read the full saved output before acting when a harness shows only a clipped preview. File output does not strengthen `handed_off` into model acceptance. Body size, UTF-8 validation, path semantics, and no-overwrite rules belong in [file-based messages](CLI_REFERENCE.md#file-based-messages-and-evidence).

Grok never publishes clipped Stop text as a complete reply. Send the full text explicitly through a file or stdin instead. Its readiness hint does not claim or acknowledge the inbox body. See [Grok Native](CLI_REFERENCE.md#grok-build-native).

## Runtime boundary and discovery

Relay first resolves the exact native session and its confirmed binding; cwd/project paths are cold-discovery hints, not identity. Disposable locators are revalidated against private binding state. Explicit `--repo`, `--room`, `--slot`, and `--service-file` must agree with the bound session. Ambiguity fails rather than switching Rooms. A Desktop/app-server process may host several sessions, so PID alone is insufficient.

The primary checkout can remain the session entry point while edits/tests/review happen in a task worktree. Communicating does not require changing back or rebinding. Share the exact task path and revision with the peer; PairRoom does not create, merge, or authorize access to worktrees. Native Owner Turn remains advisory, not a writer lock.

For an older confirmed binding without a usable locator, resume it once in the original session with `pairroom relay bind --repo "<original-bound-workspace>"`. Do not replace an otherwise valid binding just because cwd changed. [Workspace discovery and recovery](NATIVE_SESSION_WORKSPACE.md) covers deleted/redirected workspaces, session metadata, and endpoint selection.

## Delivery evidence remains conservative

| Observation | What it proves |
|---|---|
| `queued` | The Service durably accepted the message |
| `delivering` | A delivery claim was persisted before envelope release |
| `handed_off` | The CLI wrote stdout and acknowledged the claim |
| `unknown` | Delivery cannot be settled from the available evidence |
| Wake `accepted` / `submitted` | Codex command acceptance / a complete Claude socket write, respectively |

None proves that a model read, accepted, or finished the work. Collector death or a lost acknowledgement may become `unknown`. The original claimer's receipt-matched acknowledgement can still settle it while no explicit Retry is pending; otherwise inspect history and side effects before Retry creates a new message. Possibly executed effects are never automatically replayed.

The CLI requires an affirmative `handed_off: true` acknowledgement, not merely HTTP success. An empty, malformed, or negative acknowledgement after stdout leaves the local outcome uncertain: inspect the Service's recorded state rather than repeating the message.

A detached background waiter whose output never reaches the model may nevertheless be terminally `handed_off`. Another `wait` cannot re-collect it. Inspect the authorized message/history and actual workspace before deciding a fresh instruction; do not blindly duplicate the original task body.

Same-client-ID recovery requires the same body, target, attachments, quote, and optional review version. Changed content under the same ID fails. A new ID is a new publication, even for identical text. `status` / `reconcile` default to bounded body-free summaries, but **can reconcile a pending Stop publication**; use `history` or `doctor` for read-only inspection. Full history/export is explicit and may contain private material.

## Claude external wake

Update both CLI and Service, then run `pairroom relay bind` once in the existing Claude session; an approved Stop also refreshes the capability. PairRoom captures `CLAUDE_CODE_MESSAGING_SOCKET` and `CLAUDE_CODE_MESSAGING_TOKEN` in that session's tool environment. Do not copy them into prompts or flags. No new hook or longer Stop timeout is required.

A wake-enabled Room (default on; changed through Management at an idle boundary) can nudge an eligible existing Claude inbox or Codex queue with fixed, body-free text. Live collectors take precedence. The minimum interval is per receiving slot, with a shared Room hourly budget. Unattempted rate-limited heads are rechecked when eligible; a reserved or possibly submitted wake is never automatically retried. [Automatic wake](PROTOCOL.md#automatic-idle-peer-wake) owns the exact limits and audit vocabulary.

Claude's inbound policy remains authoritative: `hold`/`refuse`, missing capability, another OS/network namespace, or a stale socket can prevent continuation. A complete write is only `submitted`. Use foreground `wait` or a human nudge when automatic wake is unavailable. [Claude inbox design](design/claude-inbox-wake.md) covers private capability storage and failure categories.

## Verified vendor wake surfaces

These are **repository-recorded experiments from 2026-09-16 and 2026-09-18**, not new runs for the current main or guarantees for future harness versions. [Native efficiency boundaries](design/native-efficiency.md) keeps the dated research context; the [full earlier observations](https://github.com/sean2077/pairroom/blob/0d6e63111beb025f2d00c5dd60afa32b16d661a9/docs/NATIVE_RELAY.md#verified-vendor-wake-surfaces) remain available at their recorded revision.

| Runtime | Recorded observation and current integration boundary |
|---|---|
| Claude Code | Resuming an already-running session created a copy rather than injecting into it. Harness-tracked background `relay wait` completion woke its idle parent. The later Service inbox integration has synthetic transport tests, not a new authenticated acceptance run. |
| Codex (codex-cli 0.154.0) | One controlled `codex queue` experiment woke a deep-idle bound thread; tracked background work remained pollable across turns. Current Service wake uses a fixed nudge, not task text. |
| Grok Build | The owner-authorized 2026-09-18 experiment recorded 4/4 idle-session wakes through harness-owned background wait completion, one model turn per round, without polling output. No Service-initiated Grok wake is integrated. |

Use background wait only where the harness actually surfaces its completion. Human-only Codex wake templates may include vendor session identity in local process arguments; do not copy that identity, task text, or extra shell fragments into wake audits/messages. These observations are not proof that every unattended workflow completes or uses fewer billed tokens.

## Troubleshooting

| Symptom | Action |
|---|---|
| `pairroom` is missing in the Agent's shell | Fix that shell's PATH and verify its CLI version, not just Desktop startup |
| Service is unavailable | Start/reuse the intended Service; a custom data root uses `--service-file <root>/relay-endpoint.json`, never pasted file contents. Up to eight Stop replies given meanwhile (fewer only when they are very long: the saved state stays under about 1.9 MiB) stay saved and publish in order at the next Stop or `relay reconcile`; a reply beyond that is reported on stderr as not retained |
| Hook missing or unapproved | Install for the intended Runtime and review the exact native definition |
| Missing/conflicting session identity | Run bind as the Agent's tool call in the intended session; do not fabricate metadata |
| Binding not found after a directory change | Follow [workspace recovery](NATIVE_SESSION_WORKSPACE.md#upgrade-and-recovery), not another workspace's credentials |
| Slot occupied or bind result lost | Resume the original session/attempt; use replacement only for an intentional new session |
| Native final answer absent from Room | Check whether the Stop reply had the exact peer handle or `@user`; unaddressed bodies stay out of the Room |
| Messages remain queued | Ask the bound receiving Agent to run `wait`; inspect capability/wake observations rather than assuming it was awakened |
| Delivery is `unknown` or output was clipped | Inspect the original message, full saved output, and side effects before any explicit Retry |

## Recovery and review surface

The Native Room uses three columns on wide viewports: **Participants** on the left, conversation in the center, and **Work inspector** on the right. The Participants and Details buttons toggle panels independently. On compact viewports, only one panel opens at a time; close/Escape returns focus without discarding the draft or changing delivery state.

Participant cards show stored Runtime/Provider/model/effort/permission metadata, observed binding state, and last activity. These are display-only and not live presence. The inspector shows per-slot queued/delivering/unknown totals, oldest queued input, last wake observation, Pending items, history, diagnostics, and audit. It provides no process-start or Interrupt control.

Pending items are independent of the recent chat tail. Use `relay history --pending` for oldest-first unresolved work or `relay history --id ID` for one message. Normal history is newest-first; follow returned cursors. Reading never claims, acknowledges, or retries a message. `relay doctor` and the Native diagnostic button inspect the current Room without a model call; CLI doctor also checks local installation observations. Hook approval and model acceptance remain unknown.

Browser refresh restores an unconfirmed immutable original-ID draft and checks its receipt without sending. Explicit same-ID recovery preserves payload; Forget only removes local recovery state, not an accepted/in-flight task. Same-origin localStorage may contain private text and is not encrypted archival. Do not clear it merely to dismiss uncertainty.

Native browser API requests have a 30-second deadline covering both response headers and body reads; SSE is separate. A timed-out publication releases the controls but retains its original recovery record. Check the original receipt before an explicit same-ID retry. Receipt checking and Retry cannot run concurrently in the same page; neither timeout nor reload automatically posts another message.

Cross-window recovery mutations use a short, per-Room Web Lock, never a lock held over network requests. Browser publication requires Web Locks and writable localStorage; unavailable storage fails before publication rather than falling back to an unsafe write. A lock held by another window reports a transient status; retry after that window finishes rather than forgetting the draft. Forget only removes the exact record present when its confirmation opened, so a newer record from another window survives. Reload all open Native pages after upgrading so they use the same locking protocol. Existing outbox records retain their format; do not delete them as an upgrade step.

Optional review evidence can identify the revision actually discussed:

```bash
pairroom relay send --id review-v1 --text "Review this revision; do not implement" \
  --review --review-repo /absolute/task-worktree --review-base main
pairroom relay review --id <published-message-id> --review-repo /absolute/task-worktree
```

An unchanged observation is neither an atomic snapshot nor approval. Use the operator-selected trusted checkout; an incoming anchor path grants no access. Keep edits stable or use immutable commits/artifacts for consequential review. [Review design and historical measurements](design/native-review-closure.md) explain bounds and verification categories.

The browser's tail snapshot is bounded to recent complete messages and audit entries; retained totals still describe full history. It is not a total JSON-byte cap, and full export remains complete. [Protocol](PROTOCOL.md#native-observation-and-review-extensions) and [Storage](STORAGE.md#native-current-work-and-browser-recovery-projections) own the paging/recovery contracts.

Native remains experimental. Authenticated multi-round Claude Code/Codex/Grok acceptance, resume/fork behavior, and comparative billing require separate owner-authorized testing. Synthetic hooks, Mock, browser fixtures, and historical reports do not establish current vendor-model acceptance. Do not publish private transcripts or run paid benchmarks without consent.
