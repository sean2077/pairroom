# Support scope

[Native setup](docs/NATIVE_RELAY.md) · [LAN collaboration](docs/LAN_NATIVE.md) · [Troubleshooting](docs/TROUBLESHOOTING.md) · [Security](SECURITY.md)

PairRoom is a local-first open-source project with best-effort support through its repository. Identify the **Room host mode** and whether this machine hosts the Room or joins a remote host before reporting. Native is the default and recommended mode. A desktop-owned embedded Service can host either Native or Embedded Rooms; a direct LAN guest needs no local Service.

## Collect a minimal, safe report

```bash
pairroom version --json
```

Choose the next check for the affected path:

| Situation | Check | Scope |
|---|---|---|
| Local Native or an already joined LAN session | `pairroom relay doctor` inside that session | Binding, hooks and delivery without activating a suspended Room; direct LAN does not test the optional observer's wake capability |
| LAN guest before admission | `pairroom relay preflight --join` inside the intended session | CLI, workspace, identity eligibility and hook installation without a local Service |
| Embedded adapter startup | `pairroom doctor --repo /absolute/path/to/repository --json` | Local CLI/environment probes, not an existing Native binding |
| Hosting Service or daemon | `pairroom daemon status` and relevant `pairroom daemon logs -n 200` output | The hosting machine's daemon; a foreground or Desktop-owned Service has a different lifecycle |

Management **Settings → Diagnostics** owns environment checks and the separately consented live test. That test starts a disposable Embedded session; it does not resume or verify an existing Native binding. For LAN reports, include the guest CLI and hosting Service versions separately. Direct LAN preflight checks reachability but does not compare release/build metadata; the bound `relay doctor` report includes the host version. See [LAN recovery](docs/LAN_NATIVE.md) when the host cannot be reached.

For Room integrity, run these on the machine that owns its stored Room directory:

```bash
pairroom verify --data-dir /absolute/path/to/room --json
pairroom diagnostics \
  --data-dir /absolute/path/to/room \
  --output /absolute/path/outside-the-room/pairroom-diagnostics.tar.gz
```

The host owns a LAN Room's Event Log. A guest's private joined-Room state is not a copy of that Room directory. Room diagnostics omit transcript bodies and attachment bytes but may retain paths, versions, structured headers, errors, and environment details. Inspect every archive before sharing. Ordinary CLI JSON is not the same as Management's allowlisted safe report; do not attach a full Event Log by default.

A report should identify OS/architecture, binary path/install/launch method, PairRoom version/commit, host mode, local/host/guest role, selected CLI versions and Provider type, recent upgrade/Binding change, exact reproduction, expected/actual behavior, and relevant IDs/state. For LAN, distinguish pending admission, accepted membership, host unavailability, and local detach. UI reports also need browser/viewport and a safe screenshot/console error. Mention whether Mock reproduces it in a disposable repository.

Never publish tokens, cookies, CSRF/bootstrap URLs, private keys, native session metadata, inbox capabilities, private prompts/replies, source/diffs, approval payloads, or sensitive images. Remove complete startup URLs from foreground logs, and do not upload workspace binding files or the per-user LAN client/identity stores. Use [vulnerability reporting](SECURITY.md#13-vulnerability-reports), not a public exploit report.

## Separate control-plane and vendor evidence

Use an isolated data root and disposable repository:

```bash
pairroom service --mock --data-root /absolute/path/to/isolated-demo-data
```

Select **Embedded** explicitly when creating the Mock Room; Native remains the default. Mock isolates scheduling, persistence, and UI from vendor failures; it does not prove authentication, model quality, LAN membership, or Native session acceptance. Builds, unit tests, in-page fixtures, real-browser Mock HTTP/SSE, synthetic hooks/LAN clients, and authenticated vendor E2E are separate evidence layers. State what actually ran, on which revision/environment.

For native failures, check the selected CLI independently as the same OS user. Preserve sanitized startup/resume/policy/transport errors. An outage is not Store corruption, and quiet output is not a terminal boundary. Paid/live tests require consent and may still invoke native global hooks/MCP.

## Compatibility policy

Embedded adapters track supported documented Claude Code, Codex, Grok, and Gemini interfaces, not every version, interactive feature, or Provider combination. After updating a CLI, check the environment and separately exercise an authorized real read-only task before peer collaboration. For Native, verify the installed/approved hook, exact binding, and addressed exchange in the original sessions rather than spawning an Embedded replacement.

[Configuration](docs/CONFIGURATION.md) owns Embedded native inheritance, supported CC Switch mappings, immutable selections, and failure behavior. Native configuration is display-only and never applies Provider overrides to the original process. [Upgrading](docs/UPGRADING.md) owns format/rollback support. There is no separate roadmap or compatibility promise overriding those contracts.

## Current support boundary

The design supports one Service owner per data root, multiple canonical Git Projects/Rooms, and **two participant slots per Room**. Local Rooms connect one user's sessions. A LAN Room connects its host's session with one admitted colleague; only the host needs a Service. Different sessions on one machine may host local Rooms and join several remote hosts concurrently, retaining each binding's host and identity. Either participant may use any supported Runtime, including duplicates; a LAN peer's Runtime is established at admission. Project unregister and Room deletion have explicit preconditions and never imply deleting the repository.

Embedded limits active adapter-owning Room Runtimes and serializes participant Turns within each Room, not across external writers. Native is exempt from that adapter-capacity budget, uses per-slot FIFO/advisory ownership, and cannot start or interrupt the original sessions. Neither is a repository lock or container-grade sandbox.

Native LAN uses a separately enabled TLS listener on a selected numeric private interface, pinned host identity and owner-approved guest keys. Management, direct Room views and Embedded stay on numeric loopback. See [Security](SECURITY.md) for the boundary. General multi-user administration/RBAC, public-internet hosting, cloud sync, remote process control, more than two slots, arbitrary in-place Room reconfiguration, and an arbitrary-vendor plugin API remain outside scope. Instructions are not enforced workflow phases; there is no automatic relay-count/cost ceiling or guaranteed unattended completion.

## Feature requests

Describe the concrete workflow, why existing behavior is insufficient, the smallest verifiable acceptance criteria, state ownership, recovery, security/privacy, and multi-Room impact. Prefer preserving native capabilities over replacing them. Consult [Alternatives](docs/ALTERNATIVES.md) and [Architecture](docs/ARCHITECTURE.md); an Issue/PR proposal is not a shipped contract. Follow [Contributing](CONTRIBUTING.md).

## Native host verification boundary

[Native setup](docs/NATIVE_RELAY.md) and [LAN collaboration](docs/LAN_NATIVE.md) own installation and operator recovery. Implemented boundaries include official local session association, approved project hooks, durable relay, explicit send/wait/exchange, stdout acknowledgement, and bounded park. A LAN host sees the admitted guest key, not the guest's native session identity. Optional Claude/Codex fixed-nudge wake depends on valid local capability/inbound policy. A direct guest needs a running optional local observer for that wake path; without a collector, hook or observer, messages stay queued at the host. Grok and Gemini have no external wake integration; harness-owned background completion is a separate path.

Record actual CLI/Desktop versions, project trust, and approved hook definitions. Installation alone does not grant approval. Live attach, concurrent resume injection, zero-hook bind, and model-driven idle self-wake are not supported. PairRoom does not replace native permission controls.

`make check`, `make smoke`, and `make browser-check` cover different deterministic/native-fixture/Mock layers, not authenticated model acceptance. Real multi-round testing must separately record bind/join/send/receive, timeout/interruption, host restart, appropriate continuation limits, resume/fork/child identity, and actual usage when available. Use [Contributing's acceptance procedure](CONTRIBUTING.md#owner-authorized-authenticated-acceptance) and the current [Native protocol](docs/PROTOCOL.md#native-host-protocol-v8). A passing historical experiment is not certification of the current release.

For relay uncertainty, start with `relay doctor`. `history` inspects recorded messages without publishing or claiming them, but can activate a suspended Room. `status` or `reconcile` can publish a pending reply by its original sequence. Do not automatically retry unknown delivery; a late original receipt may settle it while no explicit Retry is pending. `handed_off` and wake submission never prove model acceptance. The [CLI reference](docs/CLI_REFERENCE.md#native-relay-commands) owns command effects and recovery details.

Grok clipped Stop text requires explicit complete publication; readiness feedback does not claim inbox text. See [Grok Native](docs/CLI_REFERENCE.md#grok-build-native). Gemini requires approved BeforeTool/AfterAgent hooks and preserves its cumulative-response boundary; Embedded exact resume remains blocked, as documented in [Gemini setup](docs/NATIVE_RELAY.md#gemini-cli). Current authenticated Claude/Codex/Grok/Gemini acceptance and billed-cost comparisons require separately recorded owner-authorized runs.
