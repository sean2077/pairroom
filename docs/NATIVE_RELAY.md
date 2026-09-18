# Native relay: setup, usage and reliability

Native hosting connects two sessions you already run. PairRoom does not launch,
configure or interrupt them. The same setup guide is available inside the browser
and desktop app, before creating a Room and in every Native Room.
[Protocol](PROTOCOL.md#native-host-protocol-v8) owns the transport contract and
the [CLI reference](CLI_REFERENCE.md#native-relay-commands) owns command flags.

## Before starting

Install PairRoom and Git. Open the desktop app or run `pairroom service`, but do
not start a second Service over the same data directory. In **each agent's tool
shell**, verify `pairroom version` and `git --version`: launching the desktop app
alone does not prove that its CLI is on that shell's PATH. Use the CLI from the
same PairRoom release as the running app/Service. Source installations use
`make install`; packaged installations follow the [installation guide](../README.md).
Restart an existing shell after changing PATH.

Install and sign in to the selected harnesses, and open both sessions in the
same Git project. Current Native support is Claude Code, Codex and Grok Build. Selecting Provider/model/effort/permissions in a Native
Room does not reconfigure your existing sessions.

## One-time project setup

Run `pairroom relay install` once per Project, from any terminal in its worktree
(it does not need a native session). Pass `--runtime` with a comma-separated list
(`cc|claude`, `codex`, `grok`); inside a recognized session it infers the harness,
and at an interactive terminal without `--runtime` it prompts a multi-select.
Non-interactive use without `--runtime` fails with the options listed rather than
waiting. Codex keeps `.codex/hooks.json`. Claude Code keeps `.claude/settings.json`.
Grok Build's Claude Code compatibility layer runs those Claude Code project hooks
by default, so installing Claude Code and Grok together writes only the Claude Code
hook; a Grok-only install still writes `.grok/hooks/pairroom.json`. Native relay
supports all three. Each selected runtime still gets its skill directory.

```bash
pairroom relay install                      # prompts at a terminal; or:
pairroom relay install --runtime claude,codex,grok
```

Review and approve the exact project hooks in each harness. Codex uses `/hooks`;
Claude Code uses project hook consent; Grok uses `/hooks` (press `r` to reload)
and its project folder-trust decision — when it is reusing the Claude Code hook,
that is the definition to review. Follow the harness's trust/restart guidance.
If Claude-hook compatibility is disabled (`[compat.claude] hooks = false`),
install Grok with that compatibility off so PairRoom writes the Grok hook file.
Installing Claude Code when a PairRoom Grok hook file already exists leaves that
file in place unless you confirm removal at an interactive prompt.
PairRoom never grants approval on your behalf. Installation writes the relay
skill as well; a skill-only installation does not install or approve hooks.
Skill installation honors `CLAUDE_CONFIG_DIR`, `CODEX_HOME` and `GROK_HOME`
for the respective runtime; project hook paths remain project-local.
The optional `npx skills add sean2077/pairroom` distribution route requires Node
and its package runner, but Node is not a prerequisite for the Go relay CLI.

## Create, join, collaborate

In the first session, invoke `/pairroom-relay <topic>` or ask the agent to run:

```bash
pairroom relay bind --create --name "<topic>"
```

Give the printed `peer_join_local` command to a second session in the same
workspace, or `peer_join` with explicit paths, and ask that agent
to execute it through its tools. With one matching active Room and slot, it can
instead run `pairroom relay bind`. Slots are Agent 1/2 (`--slot 1|2`), not vendor
names. Follow the candidate list rather than guessing when selection is ambiguous.

Alternatively, create a **Native** Room in the app and bind both sessions to that
Room; do not also run `--create`. Each successful bind is immediately ready for
relay: bind reads the official session id the harness exposes to its tool-call
environment (Claude Code `CLAUDE_CODE_SESSION_ID`, Codex `CODEX_SESSION_ID`,
Grok `GROK_SESSION_ID`) and
associates at once, so there is no nonce to echo and nothing to wait for. Run
bind as a tool call inside the intended session, not a detached terminal; without
that environment bind fails closed rather than guessing. Give the agents the task
and intended collaboration in ordinary language; no status check or initial Stop
is needed to unlock relay. `pairroom relay status` is available for diagnosis. The app displays published
messages and delivery state, not full native history.

For explicit discussion, start the receiver with `pairroom relay wait`, then
use `pairroom relay exchange --id review-1 --text "Review the proposed change"`
in the other session. Wait and exchange default to one hour; `--timeout 0` waits until
cancellation when the harness permits a pending tool. A confirmed send followed
by a wait timeout calls for `wait`, not a new send. See the
[CLI reference](CLI_REFERENCE.md#foreground-discussion-loop) for detailed recovery
commands.

The main repository can remain the session entry point while work happens in
`.worktrees/<task>`. Keep commands pointed at the bound Room workspace and use an
explicit task path for edits/tests/review; Native does not create or merge
worktrees. Changing the shell's directory does not grant a new Room identity.

## Runtime boundary and discovery

Native supports Claude Code, Codex and Grok Build, including two sessions of
one runtime. Slots remain Agent 1/2. For a Grok creator with an explicit Codex
peer, use `pairroom relay bind --create --peer-runtime codex`; no own-runtime or
slot flag is needed. Without overrides, the Service's configured pair is kept.

Grok's hook text is clipped by the harness. PairRoom never forwards a clipped
reply as complete and never puts a full inbox body into Grok's Stop feedback.
Instead, a short readiness instruction asks the agent to collect the still-queued
input with foreground `wait`. For long outgoing replies use full-text
`send/exchange`. See [Grok Native](CLI_REFERENCE.md#grok-build-native) for limits,
owned hook/skill locations, compatibility filtering and validation boundaries.

Discovery uses the current Git workspace, recognized harness lineage and native
session metadata (`CLAUDE_CODE_SESSION_ID`, `CODEX_SESSION_ID`, `GROK_SESSION_ID`). Exact session
metadata selects an existing associated Room/slot ahead of PID-only matching: a
Desktop or app-server process can host multiple sessions. Repeating `bind` in the
same session resumes its saved Service endpoint and identity without manual
flags. The Service still owns omitted pair selections, read and pinned before any
Project/Room creation; no provider secrets or guessed model/effort settings are
harvested. A nearest recognized harness scopes inherited outer-harness variables;
ambiguous or unmatched metadata fails rather than consuming another session's
inbox. These are convenience selectors, not proof of identity or a sandbox
against other programs running as the same OS user. The approved Stop hook
re-confirms the official session id captured at bind and fails closed on
divergence; all later operations authenticate the binding credential, generation
and session.

## The two receive paths share one mailbox

A Stop hook publishes the complete routed reply and may park for up to 30 seconds
inside its installed 45-second budget. `decision:block` requests continuation;
it is not arbitrary idle-session wake-up. The park collects the next eligible
FIFO input, which may be a peer's published reply even when that reply carried
no routing handle: ending the relay stops the continuation chain, it does not
suppress delivery. Claude/Codex keep eight blocks. Grok readiness/recovery requests stop at seven
to reserve a final publication before the vendor skips its eighth continuation
gate; these hints never claim that inbox text reached the model. Other hooks
can also consume the vendor budget, so this is not an unconditional final-delivery guarantee.

Foreground `exchange` sends once, then returns the next eligible FIFO input in
the same tool invocation. `wait` only collects. Their HTTP polls remain at most
30 seconds; a successful explicit empty response renews in the CLI, not the
model. Both default to one hour, finite totals allow six hours, and `0`
means no PairRoom total deadline. Caller cancellation, native tool limits,
revocation and transport errors still end a wait. Neither path owns subagents
or the native model/tool loop.

Send plus receive is not atomic or a correlated request/reply transaction. An
earlier queued message or user instruction may arrive first. Finish by sending a
final result without another wait; after explicit publication do not repeat a
peer-directed final reply unless a second Stop publication is intended. A single
process-owned collector lock coordinates concurrent CLI collectors on one host;
process death releases it.

## Delivery evidence remains conservative

`queued` means durably accepted; `delivering` precedes envelope release;
`handed_off` means the CLI wrote stdout and acknowledged it, not that the model
read, accepted or completed anything. Lost output/ack and collector death can
become `unknown`. Do not automatically replay possibly executed effects.
A body that reached a collector's stdout but not the model — for example a
detached background waiter whose output the harness never surfaced — stays
terminally `handed_off`; no later `relay wait` can re-collect it. Recovery is
diagnostic, not replay: inspect the authorized history (`status --brief=false`)
and ask the sender for a fresh instruction that does not duplicate the original
task body, whose side effects may already have executed.
Summary counts and bounded recovery IDs are transport observations, never
"working", "done", or "needs user" guesses based on silence. `status` and
`reconcile` default to bounded body-free summaries; `--brief=false` and export
intentionally retain full history. Send receipts contain IDs and transport state,
not another copy of the outgoing body. Optional wake-metadata lookup has its own
short deadline and cannot hold up collection for the full transport timeout.

## Verified vendor wake surfaces

Two vendor-sanctioned surfaces exist around the idle-wake boundary, verified
on real CLIs (2026-09-16; Grok Build 2026-09-18):

- **Claude Code**: no external command injects into an existing session, and
  resuming a running session starts a copy instead. The harness does wake an
  idle session when harness-tracked background work completes, so an
  agent-owned background `relay wait` keeps that session reachable by design
  (see the `pairroom-relay` skill). This is model-initiated, never
  PairRoom-initiated.
- **Codex (codex-cli 0.154.0)**: `codex queue --thread <session UUID>
  --message <text>` woke a deep-idle bound thread in a controlled one-time
  experiment (accepted queue exit, then an autonomous relay report with no
  human interaction). Unified-exec background tasks survive turn end and stay
  pollable cross-turn. A wake nudge must stay body-free; thread identity may
  be visible to local process observers and vendor/CLI diagnostics, and must
  never be written to the Event Log, files, or relay bodies.
- **Grok Build (grok 1.0.34)**: native binding, bounded Stop readiness, and
  authenticated multi-round acceptance in both directions are verified on
  real CLIs (2026-09-18 owner-authorized controlled experiment; sanitized
  fixed categories: 4/4 rounds woke an idle session when an agent-owned
  background `relay wait` completed, one model turn per round, no polling
  output). As with Claude Code, this wake is model-initiated — the official
  Grok background-task completion wakes the idle parent session — never
  PairRoom-initiated. Service-initiated deep-idle wake has no
  vendor-sanctioned surface for Grok and stays fail-closed.

In a wake-enabled Room (per-Room configuration, default on; opt-out only at
an idle Room boundary through the Management surface) the Service
automatically executes the Codex queue wake for a durably queued
peer-directed message to a deep-idle Codex-bound session: fixed body-free
nudge, at most one per burst, rate-limited, durably reserved before the
command, audited through a fixed outcome/reason vocabulary, and never
automatically retried. See [design/auto-wake.md](design/auto-wake.md) and
[PROTOCOL.md](PROTOCOL.md#automatic-idle-peer-wake). Claude Code and Grok Build
sessions have no external injection surface and stay reachable through the
agent-owned background `relay wait`. For disabled Rooms, non-Codex targets,
or wake failures, the CLI still prints a human-executable wake template as
the fallback; the human decides and runs it.

## Troubleshooting

| Symptom | Action |
|---|---|
| `pairroom` is not found | Fix PATH in the agent's tool shell and verify `pairroom version`. |
| Service endpoint is unavailable | Open the app/start the Service; for a custom data root, bind with `--service-file <root>/relay-endpoint.json`. Never paste its contents. |
| Hook is missing or not approved | Run `relay install` for the intended runtime and approve the exact definition in that harness. |
| Session identity is missing or differs | Run bind inside the intended session, not a separate terminal. Do not manufacture an environment value. |
| Slot is occupied | Re-run bind in the original session. Only an intentional session change should use `--replace`. |
| A bind response was lost | Retry bind for the same Room and slot without `--create` or `--replace`. It reconciles the original attempt; a new explicit `--replace` intentionally starts another replacement. |
| Messages remain queued | Have the associated receiving agent run `pairroom relay wait`. A wake-enabled Room automatically wakes an idle Codex-bound target; otherwise, for an idle Codex peer a human may run the printed vendor `codex queue` wake template. |
| Delivery is `unknown` | Inspect the Room and workspace before explicit Retry; it can duplicate work. `handed_off` proves stdout only, not model acceptance. |

Native remains experimental. Synthetic hook, Mock and browser tests are not real
vendor acceptance. Authenticated multi-round Claude Code/Codex/Grok testing,
including resume/fork behavior and comparative billed usage, remains a separate
release gate; do not run paid vendor benchmarks without consent or publish
private transcripts as evidence.
