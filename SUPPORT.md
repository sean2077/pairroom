# Support scope

[Getting started](docs/GETTING_STARTED.md) · [Troubleshooting](docs/TROUBLESHOOTING.md) · [Security](SECURITY.md) · [Operations](docs/OPERATIONS.md)

PairRoom is a local-first open-source project with best-effort support through its repository. Identify environment, Service/daemon, **Room host mode**, data, browser, Provider, and native Runtime before reporting. A desktop-owned embedded Service is not the same thing as an Embedded Room.

## Collect a minimal, safe report

```bash
pairroom version --json
pairroom doctor --repo /absolute/path/to/repository --json
pairroom daemon status
```

For daemon problems, retain relevant `pairroom daemon logs -n 200` output. Remove complete startup URLs/tokens from foreground logs. Management **Settings → Diagnostics** owns environment checks and the separately consented live test; it does not resume an existing Room session. For an active Native Room, use its diagnostic action or `pairroom relay doctor` inside the intended session instead of assuming a fresh adapter test checks that binding.

For Room integrity:

```bash
pairroom verify --data-dir /absolute/path/to/room --json
pairroom diagnostics \
  --data-dir /absolute/path/to/room \
  --output /absolute/path/outside-the-room/pairroom-diagnostics.tar.gz
```

Room diagnostics omit transcript bodies and attachment bytes but may retain paths, versions, structured headers, errors, and environment details. Inspect every archive before sharing. Ordinary CLI JSON is not the same as Management's allowlisted safe report; do not attach a full Event Log by default.

A report should identify OS/architecture, binary path/install/launch method, PairRoom version/commit, host mode, selected CLI versions and Provider type, recent upgrade/Binding change, exact reproduction, expected/actual behavior, and relevant IDs/state. UI reports also need browser/viewport and a safe screenshot/console error. Mention whether Mock reproduces it in a disposable repository.

Never publish tokens, cookies, CSRF/bootstrap URLs, credentials, inbox capabilities, private prompts/replies, source/diffs, approval payloads, or sensitive images. Use [vulnerability reporting](SECURITY.md#13-vulnerability-reports), not a public exploit report.

## Separate control-plane and vendor evidence

Use an isolated data root and disposable repository:

```bash
pairroom service --mock --data-root /absolute/path/to/isolated-demo-data
```

Mock isolates scheduling, persistence, and UI from vendor failures; it does not prove authentication, model quality, or Native session acceptance. Builds, unit tests, in-page fixtures, real-browser Mock HTTP/SSE, synthetic hooks, and authenticated vendor E2E are separate evidence layers. State what actually ran, on which revision/environment.

For native failures, check the selected CLI independently as the same OS user. Preserve sanitized startup/resume/policy/transport errors. An outage is not Store corruption, and quiet output is not a terminal boundary. Paid/live tests require consent and may still invoke native global hooks/MCP.

## Compatibility policy

Embedded adapters track supported documented Claude Code, Codex, and Grok interfaces, not every version, interactive feature, or Provider combination. After updating a CLI, check the environment and separately exercise an authorized real read-only task before peer collaboration. For Native, verify the installed/approved hook, exact binding, and addressed exchange in the original sessions rather than spawning an Embedded replacement.

[Configuration](docs/CONFIGURATION.md) owns Embedded native inheritance, supported CC Switch mappings, immutable selections, and failure behavior. Native configuration is display-only and never applies Provider overrides to the original process. [Upgrading](docs/UPGRADING.md) owns format/rollback support. There is no separate roadmap or compatibility promise overriding those contracts.

## Current support boundary

The design supports one local Service owner per data root, multiple canonical Git Projects/Rooms, and one human plus **two participant slots per Room**. Either slot may use any supported Runtime, including duplicates. Project unregister and Room deletion have explicit preconditions and never imply deleting the repository.

Embedded limits active adapter-owning Room Runtimes and serializes participant Turns within each Room, not across external writers. Native is exempt from that adapter-capacity budget, uses per-slot FIFO/advisory ownership, and cannot start or interrupt the original sessions. Neither is a repository lock or container-grade sandbox.

Outside scope are multi-user hosting/RBAC, cloud sync, direct LAN/public listeners or built-in TLS, remote workers, more than two slots, arbitrary in-place Room reconfiguration, and a stable arbitrary-vendor plugin API. Instructions are not enforced workflow phases. There is no automatic relay-count/cost ceiling or guaranteed unattended completion. See [Why PairRoom](docs/WHY_PAIRROOM.md).

## Feature requests

Describe the concrete workflow, why existing behavior is insufficient, the smallest verifiable acceptance criteria, state ownership, recovery, security/privacy, and multi-Room impact. Prefer preserving native capabilities over replacing them. Consult [Alternatives](docs/ALTERNATIVES.md) and [Architecture](docs/ARCHITECTURE.md); an Issue/PR proposal is not a shipped contract. Follow [Contributing](CONTRIBUTING.md).

## Native host verification boundary

[Native setup](docs/NATIVE_RELAY.md) owns installation and operator recovery. Native remains experimental. Implemented boundaries include bind-time official session association, approved project hooks, durable relay, explicit send/wait/exchange, stdout acknowledgement, and bounded park. Optional Claude/Codex fixed-nudge wake depends on valid capability/inbound policy. Grok has no Service-initiated wake integration; harness-owned background completion is a separate path.

Record actual CLI/Desktop versions, project trust, and approved hook definitions. Installation alone does not grant approval. Live attach, concurrent resume injection, zero-hook bind, and model-driven idle self-wake are not supported. PairRoom does not replace native permission controls.

`make check`, `make smoke`, and `make browser-check` cover different deterministic/native-fixture/Mock layers, not authenticated model acceptance. Real multi-round testing must separately record bind/send/receive, timeout/interruption, appropriate continuation limits, resume/fork/child identity, and actual usage when available. Claude/Codex allow eight message-bearing hook blocks; Grok reserves its last gate and uses seven readiness/recovery hints. A passing historical experiment is not certification of the current release.

For relay uncertainty, start with read-only `relay doctor` / `history`; `status` or `reconcile` can reconcile a pending publication by its original sequence. Do not automatically retry unknown delivery. A late original receipt may settle it while no explicit Retry is pending. `handed_off` and wake submission never prove model acceptance. Do not share private credentials, endpoint files, pending bodies, or unreviewed logs.

Grok clipped Stop text requires explicit complete publication; readiness feedback does not claim inbox text. See [Grok Native](docs/CLI_REFERENCE.md#grok-build-native). Current authenticated Claude/Codex/Grok acceptance and billed-cost comparison require owner-authorized runs; no such new run is asserted by this documentation refresh.
