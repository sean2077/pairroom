# Native relay: setup, usage and reliability

Native hosting connects two sessions you already run. PairRoom does not launch,
configure or interrupt them. The same setup guide is available inside the browser
and desktop app, before creating a Room and in every Native Room.
[Protocol](PROTOCOL.md#native-host-protocol-v7) owns the transport contract and
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
waiting. Each selected runtime gets its own project hook and skill directory
(Claude Code `.claude`, Codex `.codex`, Grok `.grok/hooks/pairroom.json`); native
relay supports all three.

```bash
pairroom relay install                      # prompts at a terminal; or:
pairroom relay install --runtime claude,codex,grok
```

Review and approve the exact project hooks in each harness. Codex uses `/hooks`;
Grok uses `/hooks` and its project folder-trust decision;
review changed definitions again. Follow the harness's trust/restart guidance.
PairRoom never grants approval on your behalf. Installation writes the relay
skill as well; a skill-only installation does not install or approve hooks.
The optional `npx skills add sean2077/pairroom` distribution route requires Node
and its package runner, but Node is not a prerequisite for the Go relay CLI.

## Create, join, collaborate

In the first session, invoke `/pairroom-relay <topic>` or ask the agent to run:

```bash
pairroom relay bind --create --name "<topic>"
```

Give the printed `peer_join` command to the second session, and ask that agent
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
that environment bind fails closed rather than guessing. Run `pairroom relay
status` inside each session to verify the binding, then give the agents the task
and intended collaboration in ordinary language. The app displays published
messages and delivery state, not full native history.

For explicit discussion, start the receiver with `pairroom relay wait`, then
use `pairroom relay exchange --id review-1 --text "Review the proposed change"`
in the other session. Exchange defaults to one hour; `--timeout 0` waits until
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
it is not arbitrary idle-session wake-up. The eight-block cap is unchanged; Grok readiness/recovery hints count toward it
without claiming that inbox text reached the model.

Foreground `exchange` sends once, then returns the next eligible FIFO input in
the same tool invocation. `wait` only collects. Their HTTP polls remain at most
30 seconds; a successful explicit empty response renews in the CLI, not the
model. Exchange defaults to one hour, finite totals allow six hours, and `0`
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
Summary counts and bounded recovery IDs are transport observations, never
"working", "done", or "needs user" guesses based on silence. `status --brief`
returns a body-free transport summary; full status/export intentionally retain
history.

## Troubleshooting

| Symptom | Action |
|---|---|
| `pairroom` is not found | Fix PATH in the agent's tool shell and verify `pairroom version`. |
| Service endpoint is unavailable | Open the app/start the Service; for a custom data root, bind with `--service-file <root>/relay-endpoint.json`. Never paste its contents. |
| Hook is missing or not approved | Run `relay install` for the intended runtime and approve the exact definition in that harness. |
| Session identity is missing or differs | Run bind inside the intended session, not a separate terminal. Do not manufacture an environment value. |
| Slot is occupied | Re-run bind in the original session. Only an intentional session change should use `--replace`. |
| A bind response was lost | Retry bind for the same Room and slot without `--create` or `--replace`. It reconciles the original attempt; a new explicit `--replace` intentionally starts another replacement. |
| Messages remain queued | Have the associated receiving agent run `pairroom relay wait`; the app cannot inject into an idle session. |
| Delivery is `unknown` | Inspect the Room and workspace before explicit Retry; it can duplicate work. `handed_off` proves stdout only, not model acceptance. |

Native remains experimental. Synthetic hook, Mock and browser tests are not real
vendor acceptance. Authenticated multi-round Claude Code/Codex/Grok testing,
including resume/fork behavior and comparative billed usage, remains a separate
release gate; do not run paid vendor benchmarks without consent or publish
private transcripts as evidence.
