# Native setup and usage

Native hosting connects two sessions you already run. PairRoom does not launch,
configure or interrupt them. The same setup guide is available inside the
browser and desktop app, before creating a Room and in every Native Room.

## Before starting

Install PairRoom and Git. Open the desktop app or run `pairroom service`, but do
not start a second Service over the same data directory. In **each agent's tool
shell**, verify `pairroom version` and `git --version`: launching the desktop app
alone does not prove that its CLI is on that shell's PATH. Use the CLI from the
same PairRoom release as the running app/Service. Source installations use
`make install`; packaged installations follow the [installation guide](../README.md).
Restart an existing shell after changing PATH.

Install and sign in to the selected harnesses, and open both sessions in the
same Git project. Current Native support is Claude Code and Codex; Grok Build
remains embedded-only. Selecting Provider/model/effort/permissions in a Native
Room does not reconfigure your existing sessions.

## One-time project setup

Run `pairroom relay install` from each intended session. It chooses that
session's runtime; when identification is unavailable, use the matching explicit
command below. Install once per selected runtime, not once per Room or round.

```bash
pairroom relay install --runtime claude
pairroom relay install --runtime codex
```

Review and approve the exact project hooks in each harness. Codex uses `/hooks`;
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
relay. Run `pairroom relay status` inside each session to verify the binding,
then give the agents the task and intended collaboration in ordinary language.
The app displays published messages and delivery state, not full native history.

For explicit discussion, start the receiver with `pairroom relay wait`, then
use `pairroom relay exchange --id review-1 --text "Review the proposed change"`
in the other session. Exchange defaults to one hour; `--timeout 0` waits until
cancellation when the harness permits a pending tool. A confirmed send followed
by a wait timeout calls for `wait`, not a new send. See the
[CLI reference](CLI_REFERENCE.md) for detailed recovery commands.

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

Native remains experimental. Synthetic hook, Mock and browser tests are not
real vendor acceptance. Authenticated multi-round Claude Code ↔ Codex testing,
including resume/fork behavior, remains a separate release gate.
