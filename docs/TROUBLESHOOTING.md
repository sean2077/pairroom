# Troubleshooting

## Agent will not start

1. Run `claude --version` / `codex --version` / `grok --version` directly as the same user in the same repository directory, for the runtime selected by that slot;
2. Check executable, Provider, cwd, permission, and sandbox;
3. Inspect the participant `LastError` and Runtime info;
4. If strict session resume is configured, confirm the native session pointed to by the Binding still exists.

Empty `provider`, `model`, `effort`, and `instructions` inherit the selected native CLI's user/global configuration. PairRoom does not replace vendor CLI login.

## Turn has no output for a long time

First check Inspector / the Turn card for a long command, an approval, or ongoing tool activity. A stall notice only means there has been no new event for a while; it does not interrupt the Turn.

If it is truly stuck, choose steering, interrupt, cancel, or restart in risk order. Do not force-submit to the other Agent at the same time; that would break the single-owner boundary.

## Codex shows an error but is still working

A generic Codex `error` can be a mid-Turn diagnostic. PairRoom records it, but only `turn/completed`, an explicit abort / cancellation, or a confirmed process exit releases the owner. If no terminal event follows, treat it as stuck.

## Messages to the peer stay Waiting

This is expected while the current Agent still holds the native Turn. Cross-Agent messages sit in the Room FIFO and wait for a reliable terminal boundary. The “current turn” bar above the timeline shows the owner and queue depth.

Agent relay accepts only the other participant's exact current handle. For a unique runtime that is `@claude`, `@codex`, or `@grok`; when both slots use the same runtime, use the displayed `0/1` handles. An unsuffixed duplicate handle is ambiguous and PairRoom reports both valid choices. `@peer`, `@human`, slot aliases, and old control markers do not route. Use `@user` for a human decision.

If a human said “greet each other” and the current Driver only introduced itself to the user, or also wrote `@user`, its response correctly ended the relay: Agent relay requires the exact peer handle and `@user` overrides every Agent handle in the same reply. Unaddressed human messages start only the Driver. Check the participant card for the exact peer handle and the Inspector envelope for `peer_handle`.

## A message did not continue after restart

Room-owned FIFO entries that never crossed the native submission boundary resume automatically in Event Log order. Input already accepted by a native runtime is not replayed. A message caught inside the native acceptance window is marked failed with explicit Retry guidance because its ownership is unknown and automatic replay could duplicate side effects. Inspect the repository before retrying, and never hand-edit an old Message ID back to pending.

## Cancelling one message affected the whole Turn

Messages in the FIFO can be cancelled precisely. After a native runtime has accepted input, vendor interrupt is often at the whole active Turn. PairRoom keeps unrelated Room FIFO items, but multiple inputs in the same native Turn for the current Agent may terminate together.

## Reviewer does not see the Driver's latest files

The Reviewer uses an isolated snapshot. Confirm that review started at a new boundary after the Driver Turn completed. If a role switch or snapshot refresh failed, inspect the system notice. Do not let Reviewer and Driver write the live workspace at the same time.

## UI refreshes often or the scroll position jumps

Confirm you are on the current build, then check the browser console and SSE reconnects. The page should batch high-frequency telemetry instead of rebuilding the whole DOM per token. If the snapshot sequence repeatedly goes backwards, report the Room ID and event sequence.

## Room cannot be restored

Common causes:

- Event Log corruption;
- the Room uses a Store schema other than `9`;
- the Project path has moved;
- a strict Binding session does not exist;
- the backup is incomplete.

Keep the original data directory. Verify the backup and the first replay error first. Store schema `9` has no migration path from older Rooms; rebuild them or restore a matching old binary with its complete backup.

## Port or token problems

Use `--help` on the current command to check listen / token flags, and confirm another process is not using the port. A non-loopback listener without a token is a configuration error, not something to bypass.

## Desktop, daemon, and service.lock conflicts

The default PairRoom data root allows only one Service owner. After discovering an installed daemon, the desktop host connects to it, starting it if needed. If a daemon installation exists but cannot provide an authenticated Management Shell, the desktop host stops startup and shows the data root, binary, and lock-owner information; it does not start a second embedded Service.

When handling a leftover lock, run `pairroom daemon status` first. Only after confirming the task is stopped and the PID in the lock no longer exists, run `pairroom daemon start --recover-stale-lock`. If the PID is still running, use `pairroom daemon stop` and wait for graceful drain. Desktop quit shuts down only an embedded Service it owns; it does not stop an external daemon.
