# Support scope

[Getting started](docs/GETTING_STARTED.md) · [Troubleshooting](docs/TROUBLESHOOTING.md) · [Security](SECURITY.md) · [Operations](docs/OPERATIONS.md)

PairRoom is a local-first open-source project with best-effort support through its GitHub repository. Identify whether a problem belongs to the environment, Service/daemon, Room data, browser, Provider configuration, or native Runtime before reporting it.

## Collect a minimal, safe report

```bash
pairroom version --json
pairroom doctor --repo /absolute/path/to/repository --json
pairroom daemon status
```

For daemon problems, include relevant output from `pairroom daemon logs -n 200`. For a foreground Service, retain startup/error output but remove complete Management/Room URLs and tokens. Management's Service diagnostics help with Project registration, capacity, and Registry problems; they do not replace Room diagnostics.

For a specific Room:

```bash
pairroom verify --data-dir /absolute/path/to/room --json
pairroom diagnostics \
  --data-dir /absolute/path/to/room \
  --output /absolute/path/outside-the-room/pairroom-diagnostics.tar.gz
```

Diagnostics are designed to omit transcript bodies and attachment bytes, but can still contain paths, versions, structured event headers, errors, and environment details. Inspect every archive before sharing it. Do not attach the full Event Log as a default troubleshooting step.

A useful report states the OS/architecture, actual binary path and install/launch method, PairRoom version/commit, selected native CLI versions, selected Runtime/Provider type, and the most recent upgrade or Binding change. Add exact steps, expected/actual result, relevant Room/Message/Turn IDs and state, and whether it reproduces in a non-sensitive repository with Mock. UI reports also need browser/viewport details and a safe screenshot or console error.

Do not publicly submit tokens, cookies, CSRF values, complete startup URLs, credentials, private prompts/replies, source/diffs, real approval payloads, or sensitive images. Use [Security reporting](SECURITY.md#13-vulnerability-reports) for vulnerabilities rather than a public exploit report.

## Separate control-plane and vendor evidence

Use a disposable repository and an isolated data root for a Mock reproduction:

```bash
pairroom service --mock --data-root /absolute/path/to/isolated-demo-data
```

Mock helps isolate PairRoom scheduling, persistence, and UI behavior from a vendor CLI or Provider failure. It does not prove authenticated native sessions or model quality work. A successful build, unit test, browser fixture, real-browser Mock Service test, and real vendor E2E are different evidence layers; report which one you actually ran.

For a native failure, first try the selected CLI independently as the same user in the same repository. Include sanitized executable/configuration and exact native resume/permission/transport errors. Do not interpret a vendor outage as Store corruption, or a quiet native Turn as a confirmed process exit.

## Compatibility policy

Adapters track the documented native interfaces of Claude Code, Codex, and Grok Build. This is not certification of every release, interactive feature, or third-party Provider combination. There is no permanent support matrix for obsolete CLIs. After updating a native CLI, run `doctor` and a real read-only single-Agent smoke followed by an explicitly addressed peer Turn on a non-critical repository.

[Configuration](docs/CONFIGURATION.md) owns native inheritance, supported CC Switch schema/Profile mappings, immutable selections, and failure behavior. The catalog's unsupported reason is meaningful; PairRoom must not silently substitute another Provider or permission policy. [Upgrading](docs/UPGRADING.md) owns Store/provisioning compatibility and rollback. There is no separate compatibility page or product roadmap that overrides those contracts.

## Current support boundary

Supported design: one local Service owner per data root, multiple canonical Git Projects and durable Rooms, bounded active Room Runtimes, and one human plus **two participant slots per Room**. Either slot may select Claude Code, Codex, or Grok Build, including the same Runtime twice. Project unregistration and archived Room deletion have explicit preconditions and do not imply deleting the user's repository.

Outside that contract: multi-user hosting/RBAC, cloud sync, direct LAN/public listeners or built-in TLS, remote workers, more than two slots in one Room, arbitrary Runtime reconfiguration inside an existing Room, container-grade isolation from responsibility labels, and a stable plugin API for arbitrary vendors.

The two participants' native Turns are serialized within a Room, not across all external writers. Creation-time rules are instructions, not enforced workflow stages. No automatic relay-count/cost ceiling or guaranteed unattended completion is provided. See [Why PairRoom](docs/WHY_PAIRROOM.md) before choosing it for a different problem.

## Feature requests

Describe a concrete workflow, why the existing Room/collaboration/permission model is insufficient, and the smallest verifiable acceptance criteria. Explain state ownership, failure recovery, migration, security/privacy, and multi-Room impact. Say how the proposal preserves the native harness rather than replacing its capabilities.

Read [Why PairRoom](docs/WHY_PAIRROOM.md), [Alternatives](docs/ALTERNATIVES.md), and [Architecture](docs/ARCHITECTURE.md) first. A proposal in an Issue or PR is not a current feature contract. Contributions should follow [Contributing](CONTRIBUTING.md).
