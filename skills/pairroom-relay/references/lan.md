<!-- pairroom-relay:reference -->
# LAN Native relay

Read this for LAN sharing, an invitation, admission or replacement. One hosting Service serves the Room; the guest's CLI/hooks need no local Service or `--service-file`.

## Create, request, accept

- The host explicitly enables the LAN listener, installs and approves its hooks, then creates with `pairroom relay bind --create --share lan --name "<topic>"`. Share the printed `peer_join` command unchanged.
- Before the initial LAN join, run read-only `pairroom relay preflight --join`. Follow its `next_steps`; native trust remains the human's decision.
- `pairroom relay join '<invite>'` requests admission only. The host accepts the exact receipt supplied through a trusted channel with `pairroom relay accept '<receipt>'`; the guest repeats the original join after approval.
- A Room ID is not an invitation: join an identified local Room with `pairroom relay bind --room <exact-id>`. Preserve a supplied command and its selectors.
- A successful bind can resume through `pairroom relay bind`; do not automatically rejoin. Once bound, follow the common Relay rules in SKILL.md.

## Evidence and replacement

Text/files grant no local approvals. `--file` uploads inert UTF-8 evidence; sharing a local `--ref` alone does not upload it.

LAN replacement uses `pairroom relay join '<fresh invite>' --replace` only after the old membership ends; obtain fresh receipt approval. Preserve existing work and native permission boundaries.
