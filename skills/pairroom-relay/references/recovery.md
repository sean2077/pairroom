<!-- pairroom-relay:reference -->
# Relay recovery

Read this on uncertainty, failure, `peer_changed`, or an explicitly requested replacement. Recover through the CLI, not private state; never bypass approvals. Inspection never requeues work or proves model acceptance.

## Uncertain publication

Check status/reconcile only on uncertainty/failure. Keep the original publication ID and bytes; reuse IDs only for identical publication content. Confirmed send plus timeout means receive-only wait, never resend. Before any Retry, inspect the original operation and its effects.

## Changed peer

The original peer generation and body stay local as `peer_changed`. Inspect `pairroom relay peer` first. After an explicit decision, `send/exchange --peer-generation N --id NEW` permits one publication to the new peer. Same-session `--replace` cannot reset automatic context; a new native session can. Do not retag or replay an old automatic reply.

## Replacement

For a local Room, explicit `pairroom relay bind --replace --slot 1|2` expires the old grant and retains existing work. LAN and enterprise replacement/admission use their own references linked from SKILL.md; local replacement is not a fallback for either transport. Cwd changes do not justify rebinding.
