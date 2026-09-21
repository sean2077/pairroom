# Claude Code external inbox wake

Implemented against the documentation checked on 2026-09-21. Synthetic local
transport and binding tests are not authenticated Claude acceptance evidence.

## Use

Update the Service and CLI together. In the existing Claude Code session, run
`pairroom relay bind` once. A later approved Stop hook also captures/refreshes
its inbox. No new flags, hooks, Channels setup, Claude process, or increased Stop
timeout are needed. The Room's existing automatic-wake switch controls this
path as well as Codex queue wake.

A usable session exposes both `CLAUDE_CODE_MESSAGING_SOCKET` and
`CLAUDE_CODE_MESSAGING_TOKEN` to its tools/hooks. PairRoom detects those
capabilities rather than assuming a CLI version guarantees them. Missing or
invalid environment clears the older capability; it does not prevent normal
binding, publication or collection. Never copy either value into a prompt.

Claude's `crossSessionInbound` policy still applies. `hold` can need human
release; `refuse` can discard the nudge. PairRoom does not change permissions,
forge a sender's permission mode, or promise that the Service is an own-child
sender merely because it holds the token. Keep background `relay wait` where
its harness surfaces completion, or use a human nudge when external wake is
unavailable. A failed/held attempt is not automatically retried.

## Transport and identity

Confirmed bind and identity-checked Stop capture a capability in
`.pairroom/rooms/<room>/slots/<slot>/claude-inbox.json`. It includes the bind ID,
generation, native session ID, inbox address and token. This is private local
state, not a Room event, API field or vendor session registry. The Service
re-reads the exact bound workspace/slot and rejects a mismatched generation.
Grok's compatible Claude hooks and Codex never capture inherited Claude values.

The existing waker applies its collector check, grace, burst deduplication,
per-Room limits and durable reservation before the external write. It sends
only the fixed `nativeWakeNudge`; the actual task stays exclusively in the
PairRoom FIFO. The receiving session must collect it through `relay wait`.
Neither socket submission nor a redundant nudge claims the task.

The stream contains an auth JSON line followed by a user-message JSON line.
No sender metadata, task body, session-resume command or permission override is
sent. There is no documented acceptance acknowledgement in this socket
contract: a completed write is recorded as **`submitted`**, never `accepted`,
`handed_off`, or proof that a model turn started. A native inbound policy or
vendor regression may prevent continuation after a submitted write.

## Local security and failure boundaries

Unix uses an owned Unix-domain socket, rejects symlink/regular-file endpoints
and group/world-writable sockets, and requires owner-only capability files and
slot-directory ancestry. Windows accepts only local `\\.\pipe\...` paths,
checks the pipe server belongs to the same OS user, uses anonymous security
quality-of-service to prevent server impersonation,
and creates/verifies a protected owner-only file DACL before storing the token.
Writes use a bounded deadline; overlapped Windows writes cancel and drain
before freeing their buffers/handles. No external networking or new dependency
is added.

The capability file is bounded to 16 KiB. Atomic refresh avoids partial reads;
existing slot cleanup removes crash-left temporary files; unbind removes the
capability file. Do not export either
the sidecar or its `.claude-inbox-*` temporaries. These checks protect accidental
cross-binding and stale identity, not hostile code running as the same OS user.
The Service and Claude must share a compatible local OS/user/filesystem/IPC
namespace; a native Windows Service cannot use a WSL Unix socket by guessing a
translation, and a remote/container session is not implicitly reachable.

Missing/insecure/stale capabilities produce `suppressed/capability_unavailable`
without consuming a reservation. Failed writes produce fixed
`socket_failed`, `socket_timeout` or `socket_cancelled` audit reasons. Tokens,
addresses, native session identity and raw socket errors never enter wake
audit, public projections or command arguments. All failures retain FIFO input;
rebind in the intended live session or use receive-only `relay wait`. Do not
resend a task merely because wake did not visibly start a turn.

## Sources and verification

- [Anthropic cross-session messaging](https://code.claude.com/docs/en/cross-session-messaging#the-sessions-inbox-socket): official inbox environment, transports, authentication and inbound policy.
- [Claude Code issue #93720](https://github.com/anthropics/claude-code/issues/93720): dated v2.1.268 reproduction of the auth-line + `type:user`/`message:{role,content}` wire shape and idle delivery. This issue reproduction is not a version-independent protocol guarantee.
- [Windows CreateFile](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilew) and [CancelIoEx](https://learn.microsoft.com/en-us/windows/win32/api/ioapiset/nf-ioapiset-cancelioex): local pipe access, SQOS and overlapped cancellation lifetime.

Deterministic tests cover actual Unix sockets/Windows named pipes, private-file
validation, token refresh, generation mismatch, cancellation, no-ACK submission,
redacted failure, no retry after restart, and HTTP bind/send/collect with a fake
inbox. Windows/macOS CI runs the new transport package. None of these tests runs
a paid Claude model. Owner-authorized real sessions, including `accept`/`hold`/
`refuse`, Windows and Unix, remain a separate acceptance gate.
