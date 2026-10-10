# Native collaboration on a LAN

Native is PairRoom's default and recommended mode. It supports a shared Room
between two colleagues' existing coding sessions. Each colleague keeps a local
PairRoom Service, native harness, workspace, credentials, and tool permissions.
One Service hosts the Room's shared event log; the other connects outward to it.
Either colleague can host a different Room. Embedded Rooms remain a local,
optional mode and do not support LAN membership.

For local hook installation and runtime requirements, start with
[Native relay](NATIVE_RELAY.md). The [CLI reference](CLI_REFERENCE.md) owns flag
semantics; [Security](../SECURITY.md) owns the trust and network boundaries.

## Create, request, accept

1. On the host machine, enable **LAN sharing** in Service Settings. Select a
   numeric address belonging to the intended local interface and a fixed port,
   for example `192.168.10.24:4317`. Allow that port through the host's local
   firewall for the intended network. Management and ordinary Room views keep
   their numeric-loopback listeners. Enabling LAN does not expose those routes.
2. In the host's existing native coding session, install and approve its
   PairRoom hooks, then create a shared Room:

   ```bash
   pairroom relay bind --create --share lan --name "Reproduce checkout failure"
   ```

   Share the returned `peer_join` command with the colleague. It contains a
   public invitation, not a Management token, native session ID, or relay
   credential. The creator occupies Agent 1 unless `--slot 2` was explicitly
   requested. The other slot is `awaiting_peer`; omit `--peer-runtime` because
   the joining native session establishes its actual Runtime at admission.
3. On the guest machine, start its local PairRoom Service and install/approve
   hooks in the intended workspace. The guest agent runs the received command
   as a tool call inside its own native session:

   ```bash
   pairroom relay join 'pairroom://join/<public-descriptor>'
   ```

   The native session and local workspace can differ from the host's runtime,
   repository, branch, or paths. `--repo PATH` selects the guest's workspace
   explicitly; `--service-file PATH` selects its **local** Service discovery
   file. Do not copy the host's Service file or local filesystem paths.
4. Join returns `status: pending` and a public `receipt`. Send that exact
   receipt to the host through the colleagues' existing trusted communication
   channel. The host checks the receipt, then accepts it in the Room's LAN panel
   or from the bound owner session:

   ```bash
   pairroom relay accept 'pairroom-accept:<exact-receipt>'
   ```

   The receipt identifies the request and the guest's per-Room public key.
   A display name or source IP is not identity. The invitation permits a join
   request only; pending guests cannot read history, files, inboxes, or Room
   state. Approval consumes one admission atomically; simultaneous requests do
   not both occupy the slot.
5. The guest repeats the same `join` command after acceptance. It promotes the
   confirmed local binding and prints the final bootstrap with the actual
   runtime-derived mention handles. Later, `pairroom relay bind` resumes that
   same association. Normal `send`, `wait`, `exchange`, Stop publication,
   `status`, and `history` commands work through the guest's local Service.

The invitation expires after ten minutes by default. A host can use
`pairroom relay invite` to issue a fresh invitation while awaiting a peer. For
an expired pending join, use the new invitation in the same guest session. Its
local Service settles the old request before renewing the request ID and keeps
the per-Room private key. A confirmed member reconnects with its existing key
and generation after a network interruption or Service restart, without another
invitation or repeated owner approval.

A shared Room is created explicitly with `--share lan`. An existing local Room
cannot be silently converted to shared history. Both agents may queue explicit
messages while awaiting a peer; automatic mention routing becomes available
when the actual peer Runtime is admitted. The native harness still owns when
and how it executes any received input.

## Share a useful bug report

A compact bug report can include the observed behavior, expected behavior,
reproduction command, environment details that may be shared, and actual evidence
files. Use explicit file upload when the other machine needs the bytes:

```bash
pairroom relay send --id checkout-bug-01 \
  --text "Checkout fails on the second request; see the repro and captured log." \
  --file ./repro.sh --file ./checkout.log
```

`--file` uploads a regular UTF-8 text artifact, up to 5 MiB per file. Scripts,
patches, configuration fragments, and logs remain private, inert data files;
PairRoom never executes them, extracts archives, or overwrites a working file.
Only select content that the colleague and hosting Service may read. Image
attachments continue to use `--attach`.

`--ref` retains its existing meaning: it sends a path, size, and SHA-256 pointer,
not file contents. A reference to a host-local path is not automatically readable
on the colleague's computer. `--text-file` reads an existing file into the
message body; it does not create a separate downloadable attachment. Quoted
messages and attached review observations remain evidence, not approvals.

The shared log records attachment IDs, names, sizes, and content hashes. Before
claiming an incoming message, the guest downloads and verifies its attachments
into a protected local cache and renders those local paths. The delivery lease
starts only after that preparation. Slow or interrupted downloads do not claim
an unprepared message, and a changed queue head is checked again before claim.
Both owners can download shared evidence from their local Room view. Unselected
local files, directory listings, transcripts, and provider settings are not
automatically exposed by the attachment API. Explicit text, `--ref`, and review
observations may contain paths selected by the sender; sharing them is a local
decision, not automatic transport metadata collection.

## Shared humans and local authority

Both human owners can view the shared Room's history, including `@user`
escalations, and contribute a human message to either agent slot. Human
messages retain authenticated author provenance: host owner or joined owner.
This does not add another agent slot or make a colleague a local administrator.
The author shown in a message is separate from the local native harness's
permission and approval decisions.

A request to run the colleague's script, modify a repository, approve a tool, or
share more data stays subject to the receiving user's instructions and existing
native/project permissions. Receiving a message or downloading evidence does
not authorize those actions. The Room host can read the shared content; TLS
protects transport, not content from the host itself.

## Observe and recover

The guest Service keeps a private transport registration and publication
recovery state; it does not create a second authoritative Room inbox. `room` in
guest CLI output is an opaque local routing ID derived from the host key and
remote Room ID, so equal Room IDs on different hosts cannot collide. Usually
let native session discovery select it instead of copying IDs manually.

| Observation | Meaning and recovery |
|---|---|
| `pending` | A join request exists without membership. Verify the exact receipt and accept it on the host. |
| `accepted`, then reconnect | The same private Room key and accepted generation reconnect. No fresh owner approval is needed. Repeat `join` only if local promotion was interrupted. |
| Invitation expired before approval | Ask for a fresh invitation and run `join` in the same original guest session/workspace. A new key is unnecessary. |
| Host offline | Messages remain in the host's durable queue. The guest cannot claim a second local copy. Resume when the host returns. |
| Guest offline | New messages stay queued at the host. A returning guest collects them under the original membership. |
| Publication response lost | Reuse the original `--id` with identical body and evidence. Stop publications retain their original sequence in the local outbox. Do not invent another ID. |
| Collector stdout written | `handed_off` means delivery to CLI/hook stdout, not model acceptance, execution, or success. |
| Claim or acknowledgement uncertain | Inspect `history --pending` / `history --id ID`. Never automatically replay `unknown`. If the local collector already reported successful stdout, the guest may settle only that original ACK after reconnection; a claim alone never authorizes acknowledgement. |
| Evidence hash or metadata mismatch | Collection fails before claim. Inspect the original evidence and publish changed content with a new ID. |
| Revoked or archived membership | New reads, writes, claims, wake reservations and downloads fail. Explicitly leave locally before reusing that native session elsewhere. Previously downloaded content cannot be recalled. |

The guest's **connected** indicator reports network reachability. Collector
activity, queue state, delivery receipts, native hook approval, and model
acceptance are distinct observations. `relay doctor` checks the current local
native capability and remote transport without sending a wake; a suspended
host Room must be activated with `status` before doctor can inspect it.

The private guest journal records original delivery receipts before returning an
envelope locally. It records successful collector stdout before forwarding an
ACK, so a lost ACK response can be reconciled after Service restart without
claiming or printing the body again. A failed output writer leaves the original
delivery uncertain. An explicit host Retry creates a new message ID and receipt;
the original remains available for inspection. Unresolved receipt retention is
bounded and fails closed instead of discarding uncertain delivery evidence.

Automatic wake remains a fixed body-free Claude/Codex nudge. The host first
persists a unique wake reservation; the guest persists its own spent receipt
before attempting the local capability. Reserved effects are never retried
automatically, including after a lost response or restart. A failed or uncertain
wake leaves the inbox available to `relay wait`. The bounded local wake journal
fails closed when full; it does not prevent foreground collection. Grok/Gemini
retain their documented foreground/tracked-wait boundaries.

The host owner can revoke the remote member with `pairroom relay revoke` or the
Room LAN panel. The guest can use `pairroom relay unbind` or **Leave** in its
local joined Room view. Those actions do not stop either vendor process.
Management settings, arbitrary workspace browsing, process control, approval
resolution, provider configuration and listener configuration are never LAN
member operations.
