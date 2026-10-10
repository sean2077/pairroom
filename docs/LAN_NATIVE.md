# Native collaboration on a LAN

Native is PairRoom's default and recommended mode. It supports a shared Room
between two colleagues' existing coding sessions. **Only the Room's host needs
a PairRoom Service.** The other participant connects directly with the PairRoom
CLI and approved hooks, keeping its native harness, workspace, credentials, and
tool permissions on its own machine. The host owns the shared event log and
inboxes. Joining requires no local Service, inbound listener, or second daemon.

One machine can host its own Rooms and join Rooms on other hosts concurrently
through different native sessions. Each confirmed binding retains its own
host endpoint, public-key pin, Room, slot and generation; there is no global
server switch. Embedded Rooms remain a local, optional mode without LAN
membership.

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
3. On the guest machine, make the CLI available in the Agent's tool shell and
   install/approve hooks for its Runtime in the intended workspace. Check the
   guest setup, then run the received command as a tool call inside that native
   session:

   ```bash
   pairroom relay preflight --join
   pairroom relay join 'pairroom://join/<public-descriptor>'
   ```

   The native session and local workspace can differ from the host's runtime,
   repository, branch, or paths. `--repo PATH` selects the guest's workspace
   explicitly. The invitation supplies the remote endpoint and pin; do not pass
   `--service-file` or copy a Service file from the host. `preflight --join` is
   read-only and does not require a local Service. It checks CLI, workspace,
   session eligibility and hook installation; native hook approval remains the
   user's decision.
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
   `status`, and `history` commands connect directly to the recorded host.
   Ordinary `preflight` now resolves this bound session's remote transport
   without consulting a local Service endpoint.

The invitation expires after ten minutes by default. A host can use
`pairroom relay invite` to recover the current unexpired invitation, or to
issue a fresh one once the previous invitation expired or was consumed. For
an expired pending join, use the new invitation in the same guest session. The
client settles the old request before renewing its request ID and keeps the
per-Room private key. A confirmed member reconnects with its existing key and
generation after a network interruption, CLI restart or host Service restart,
without another invitation or repeated owner approval. Revocation and archive
do not authorize an automatic new join.

A shared Room is created explicitly with `--share lan`. An existing local Room
cannot be silently converted to shared history. The host can queue explicit
messages while its peer slot awaits admission; a pending guest cannot read or
publish Room content. Automatic mention routing becomes available when the
actual peer Runtime is admitted. The native harness still owns when and how it
executes any received input.

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

Shared evidence has two separate storage budgets on the host and in the guest's
download cache:

| Budget | What counts |
|---|---|
| **100 MiB per Room** | Committed attachment content and its manifest metadata. Each message still allows at most eight attachments and 20 MiB of content. |
| **An additional 32 MiB of temporary storage** | Active upload reservations and temporary files left by interrupted uploads or cache writes. Even empty leftover files consume budget, and reopening the Room or restarting the CLI does not reset it. |

Temporary capacity is reserved before reading an upload. A successful upload
moves its content into committed storage; normal cancellation or failure
cleanup releases temporary capacity. If interrupted writes exhaust the budget,
further staging is refused with a request to inspect the leftover uploads.
Reaching a limit does not
silently delete valid attachments, cached evidence, or uncertain delivery
records to make space. See [attachment storage](STORAGE.md#attachment) for
the existing reclamation rule for host uploads that no message references.
Uploads to local-only Rooms do not inherit these LAN storage quotas.

The guest's copies live in that joined client's `evidence` directory under the
user configuration directory's `pairroom/lan-clients/<lan-id>`. Freeing this
cache is a deliberate operator action. Stop collection and any optional local
Service observer, then move the **complete `evidence` subdirectory**, including
content and manifest files together, to a backup outside the client directory.
Keep the parent client directory, its private identity and all delivery and
publication journals intact. Do not remove individual content files while
leaving their manifests. Retain the backup for evidence still in use or involved
in an uncertain delivery; deleting it is an explicit choice after inspection.
The next access fetches and verifies a fresh copy only while the host still
authorizes that membership and retains the evidence.

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
The host Room view and the guest's optional **Joined Rooms** dashboard can also
download shared evidence. Unselected local files, directory listings, transcripts,
and provider settings are not automatically exposed by the attachment API.
Explicit text, `--ref`, and review
observations may contain paths selected by the sender; sharing them is a local
decision, not automatic transport metadata collection.

## Shared humans and local authority

Both human owners can inspect the shared Room's history, including `@user`
escalations. The host Room view and the guest's optional local dashboard support
human messages to either agent slot. Human messages retain authenticated author
provenance: host owner or joined owner.
This does not add another agent slot or make a colleague a local administrator.
The author shown in a message is separate from the local native harness's
permission and approval decisions.

A request to run the colleague's script, modify a repository, approve a tool, or
share more data stays subject to the receiving user's instructions and existing
native/project permissions. Receiving a message or downloading evidence does
not authorize those actions. The Room host can read the shared content; TLS
protects transport, not content from the host itself.

## Observe and recover

The guest keeps its certificate, admission and transport recovery journal in a
private per-user client store, separate from Service data roots. Its exact
native session association and publication state remain local. There is no
second authoritative Room inbox. `room` in guest CLI output is an opaque local
routing ID derived from the host key and remote Room ID, so equal Room IDs on
different hosts cannot collide. Let native session discovery select it instead
of copying IDs manually. Changing cwd or a local Service configuration never
retargets that binding.

For a direct LAN binding, `pairroom relay status --brief=false` returns a recent
window of at most **300 complete messages and 80 audit entries**. Encoded JSON
size limits can shorten that window so the response stays below the LAN
protocol's **8 MiB** response cap. Included message bodies, quotes and evidence
metadata stay complete. `total_messages` and `total_audit` retain the exact
counts for the Room's retained history; omitted entries have not been deleted.
Use paginated `pairroom relay history` with its returned cursors, or
`history --id ID`, to inspect older evidence. As with brief status, this command
can reconcile pending Stop publications; history and doctor are the read-only
inspection paths. A local binding's `status --brief=false` retains its full
snapshot behavior.

| Observation | Meaning and recovery |
|---|---|
| `pending` | A join request exists without membership. Verify the exact receipt and accept it on the host. |
| `accepted`, then reconnect | The same private Room key and accepted generation reconnect. No fresh owner approval is needed. Repeat `join` only if local promotion was interrupted. |
| Invitation expired before approval | Ask for a fresh invitation and run `join` in the same original guest session/workspace. A new key is unnecessary. |
| Host offline | Messages remain in the host's durable queue. The guest cannot claim a second local copy. Resume when the host returns. |
| Guest CLI/hooks are not running | New messages stay queued at the host. A returning collector uses the original membership; opening a new native session does not inherit it. |
| Optional local Service stopped | Direct `join`, `send`, `wait`, `exchange` and hooks remain usable. The local dashboard and its optional wake observation are unavailable. |
| Publication response lost | Reuse the original `--id` with identical body and evidence. Stop publications retain their original sequence in the local outbox. Do not invent another ID. |
| Collector stdout written | `handed_off` means delivery to CLI/hook stdout, not model acceptance, execution, or success. |
| Claim or acknowledgement uncertain | Inspect `history --pending` / `history --id ID`. Never automatically replay `unknown`. If the local collector already reported successful stdout, a later bind/resume or receive opportunity can settle only that original ACK after reconnection; a claim alone never authorizes acknowledgement. |
| Evidence hash or metadata mismatch | Collection fails before claim. Inspect the original evidence and publish changed content with a new ID. |
| Evidence cache at its bound | This machine's verified copies reached 100 MiB. Stop collection and any optional observer, then move the complete joined-client `evidence` subdirectory, content and manifests together, to a backup. Preserve the parent private client record and all delivery/publication journals. A later access downloads and verifies a fresh copy only if the host still authorizes access and retains it. Keep the backup for needed or uncertain-delivery evidence; no automatic removal occurs. |
| Revoked or archived membership | New reads, writes, claims, wake reservations and downloads fail. Explicitly leave locally before reusing that native session elsewhere. Previously downloaded content cannot be recalled. |

The optional dashboard reports its latest contact with the host. An HTTP
response over the pinned connection counts as successful contact even when
the host denies the requested operation because membership was revoked or the
Room was archived. Contact and membership status therefore describe separate
facts: a reachable host can refuse access. An active collector, native hook
approval and model acceptance are also separate observations. A contact
timestamp does not prove continuous reachability or that either Agent is
online or working. `relay doctor` checks the current local native capability
and remote transport without sending a
wake; a suspended host Room must be activated with `status` before doctor can
inspect it.

The private guest journal records original delivery receipts before returning an
envelope locally. It records successful collector stdout before forwarding an
ACK, so a later bind/resume, collector or optional observer can reconcile a lost ACK
response without claiming or printing the body again. A failed output writer
leaves the original delivery uncertain. An explicit host Retry creates a new
message ID and receipt; the original remains available for inspection.
Unresolved receipt retention is bounded and fails closed instead of discarding
uncertain delivery evidence.

Foreground `wait` / `exchange`, a tracked background `wait` whose completion the
harness actually surfaces, and bounded Stop park provide direct receive paths.
Keep one collector per slot. No running collector, hook, or optional local wake
observer means new messages remain queued at the host until the next receive
opportunity. The remote host cannot wake a native process on the guest machine
by itself.

To abandon a pending attempt or detach while its host is offline, run this
inside the original native session, using the local Room ID printed by join:

```bash
pairroom relay unbind --local-only --room <local-room-id>
```

This makes the local association inactive and releases its local session
reservation without contacting the host. It does not cancel a possibly
accepted remote admission; ask the host owner to revoke it. The optional
dashboard offers the same explicit **Detach locally** action. Neither path
reconnects that association automatically. The original session can omit
`--room` when its client catalog resolves a single association, including a
pending admission after changing directories.

Changing the native session for the **same** remote Room requires a fresh
invitation and an explicit replacement after the host confirms the old
membership is revoked, left, or expired:

```bash
pairroom relay join '<fresh-invitation>' --replace
```

Use the original workspace and host route. This starts a new request, whose
exact receipt must be accepted again by the host. The old client record and
receipts remain at `lan-clients/<lan-id>/retired/<local-bind-id>/client.json`;
workspace publication state, credentials and the original join attempt remain
under `.pairroom/retired/<local-bind-id>/`. These private archives are never
replayed into the new binding. A plain `join` or `bind` only resumes
its original association; it cannot replace retired membership. Renewing a
never-admitted expired request in its original session/key remains the
ordinary pending-request renewal described above. An archived Room must be
restored or replaced by its host before it can admit anyone.

An optional local Service can observe client bindings and perform supported
Claude/Codex wake with a fixed body-free nudge. The host first persists a unique
wake reservation; the local observer persists its spent receipt before using
the local capability. Reserved effects are never retried automatically,
including after a lost response or restart. Failed or uncertain wake leaves
input available to `relay wait`. The bounded wake journal fails closed when
full without preventing foreground collection. Grok/Gemini retain their
documented foreground/tracked-wait boundaries.

## Optional local dashboard

If PairRoom Desktop or a local Service is already running, **Joined Rooms**
projects bindings from the bounded client catalog belonging to that OS user.
It appears alongside locally hosted Rooms and offers shared history, human
messages, receipt checks, evidence downloads, Leave and **Detach locally**. Private keys stay in
the client store and never enter the browser. The dashboard and CLI use the
same client identity and recovery state, not a duplicate Room log or another
binding. A different local Service data root does not change the remote host.

Preserve both per-user stores, `pairroom/lan-clients` and
`pairroom/native-identities`, with their matching workspace state when backing
up this machine. The latter coordinates exact native-session ownership with
locally hosted Rooms. See [Storage](STORAGE.md#direct-lan-client-state) for
their authority and recovery boundaries.

You may start a local Service for this dashboard or to host your own Rooms.
It remains optional for participating in other Rooms: joining and normal Agent
communication never wait for it, start it automatically, or require a daemon.

The host owner can revoke the remote member with `pairroom relay revoke` or the
Room LAN panel. The guest can use `pairroom relay unbind` directly or **Leave**
in its optional joined Room view. Those actions do not stop either vendor process.
A revoked member frees the shared slot: a successor admitted from a fresh
invitation selects its own Runtime, and an expired invitation window keeps
answering its newest attempt before that request is forgotten. Revoked
admissions stay durable for audit; revoking never rewrites the Room Event Log.
Management settings, arbitrary workspace browsing, process control, approval
resolution, provider configuration and listener configuration are never LAN
member operations.
