# Native host mode

Status: implemented, experimental. Current contracts live in
[Architecture](../ARCHITECTURE.md), [Protocol](../PROTOCOL.md#native-host-protocol-v8)
and [Storage](../STORAGE.md#native-relay-state). User setup is documented in
[Native setup and usage](../NATIVE_RELAY.md).

## Decisions

Native Rooms relay between user-owned sessions; PairRoom never owns their model
loop or launches/interrupts those processes. Host mode is immutable and separate
from collaboration mode. Provider/model/effort/permissions are display-only.

Binding is an explicit in-session command. It captures the official identity
from the selected harness's tool environment and associates immediately. There
is no automatic SessionStart association or alternate association path. Missing
or invalid identity, an incompatible creator slot, or missing local hooks fails
before Project/Room creation. Service defaults are read and pinned before that
preflight; full creation-time validation remains on the Service.

The approved Stop hook publishes a complete finished response and may park
for incoming relay. It confirms the binding identity and opportunistically
records the transcript path. A known bound harness with a different identity
fails visibly without publication or collection; an unrelated unbound session
is a no-op. Identity/lineage observations used for diagnostics are not authority.

Claude Code documents `CLAUDE_CODE_SESSION_ID` for tool subprocesses and its
agreement with hook `session_id` in the [official environment reference](https://code.claude.com/docs/en/env-vars).
Codex injects `CODEX_SESSION_ID` in its [execution environment](https://github.com/openai/codex/blob/main/codex-rs/core/src/exec_env.rs).
Grok exposes `GROK_SESSION_ID` to tools; its file hooks use camelCase identity
and reply fields. Hook feedback carries only readiness for foreground collection;
clipped outgoing replies require explicit full-text publication. See
[Grok Native](../CLI_REFERENCE.md#grok-build-native) for the pinned contract.
These references do not replace testing installed versions, resume/fork/child
sessions or actual response boundaries. Missing or divergent identity fails
closed; the same-user threat boundary does not resist intentional environment
overrides or arbitrary private-file reads by that OS user.

## Durable boundaries

Global `(runtime, session_id)` ownership includes duplicate-runtime slots,
embedded/native collisions and archived Rooms. Archive retains ownership.
Same-session bind recovery preserves generation; explicit replacement/unbind
revokes old credentials and queued work without claiming to stop native work.

Unconfirmed local bind attempts stay in a private write-ahead file, outside
active hook/foreground discovery. Failed attempts never overwrite the committed
local credentials or state. Retrying an uncertain result reuses its original
bind identity; interrupted promotion must not reset publication watermarks.
Only confirmed binding state enters normal discovery. Secrets remain in
owner-only files, never stdout, argv, model context or Event Log.

The per-slot inbox is durable FIFO; Owner Turn is advisory. The Event Log is
append-only. Persist `delivering` before releasing an envelope and acknowledge
only after stdout; ambiguous delivery becomes `unknown` and requires explicit
Retry. Report sequence and pending body are one atomic local write and recovery
reconciles the original identity. Publishing and receive-side park are independent.
Automatic Stop publication and explicit send are independently auditable paths;
same-turn use can intentionally duplicate a message. No transcript parsing,
concurrent resume injection or scheduled idle self-wake is introduced.

## Release acceptance

Bind-to-send/wait/exchange, global ownership, replacement/unbind, visible hook
identity mismatch, pre-creation failures, FIFO/ack/restart recovery and skill
projection freshness require regressions. Embedded protocol v7 and native protocol v8, their prompt budgets, Go 1.25 and the approved dependency closure remain current. Registry checkpoint schema 3 has strict canonical slot keys and retains host mode.

Mock and synthetic hook/HTTP/SSE/browser results must be labeled separately.
Real authenticated Claude Code/Codex/Grok multi-round acceptance remains an unmet
release gate until the actual installed harnesses are exercised and reported.
