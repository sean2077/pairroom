# PairRoom support scope

> [Getting started](docs/GETTING_STARTED.md) · [Troubleshooting](docs/TROUBLESHOOTING.md) · [Security policy](SECURITY.md) · [Operations](docs/OPERATIONS.md)

PairRoom is a local-first open-source project. Support is best-effort through the GitHub repository. Before filing an issue, decide whether it belongs to environment, Service/daemon, Room data, browser UI, or Vendor Runtime.

## 1. Before filing an Issue

### 1.1 Version and environment

```bash
pairroom version --json
pairroom doctor --repo /absolute/path/to/repository --json
```

Record the operating system, architecture, install method, PairRoom binary path, and the current official Claude Code / Codex / Grok Build versions for the selected runtimes.

### 1.2 Service / daemon

```bash
pairroom daemon status
pairroom daemon logs -n 200
```

Foreground mode keeps startup output, but remove complete Management/Room URLs, Tokens, and sensitive paths before sharing.

Export Service diagnostics from the Management Shell for Project/Room/Runtime/capacity/Registry problems. That file does not replace Room diagnostics.

### 1.3 Room data

```bash
pairroom verify --data-dir /absolute/path/to/room --json
pairroom diagnostics \
  --data-dir /absolute/path/to/room \
  --output pairroom-diagnostics.tar.gz
```

Diagnostics are designed to omit transcript body and attachment bytes, but they may still contain versions, structured event headers, errors, and environment paths. Read [SECURITY.md](SECURITY.md) and inspect the archive by hand before sharing.

### 1.4 Mock comparison

Try reproducing in a minimal test repository:

```bash
pairroom service --mock
# or
pairroom serve --repo /absolute/path/to/test-repo --mock
```

Mock helps distinguish PairRoom control-plane/Room-state problems from vendor CLI problems. Mock success does not prove a real vendor will work.

## 2. A bug report should include

- operating system and architecture;
- PairRoom version/commit/build date;
- actual binary path and launch entry;
- minimal relevant Service/Room parameters, with Tokens redacted;
- current official Claude Code / Codex / Grok Build versions for the selected runtimes;
- Project/Room/Runtime phase and visible Delivery/Processing state;
- exact steps, expected result, and actual result;
- whether it reproduces with `--mock`;
- whether it reproduces in a minimal non-sensitive Git repository;
- redacted `verify`/`doctor` results;
- the most recent upgrade, daemon reinstall, Binding, or data migration before the problem.

For UI issues, add browser version, viewport, console error, and a minimal screenshot that can be made public.

## 3. Do not submit publicly

- API/Management/Room Tokens;
- Cookies, CSRF, or a complete startup URL;
- private prompts, Agent answers, or Event Log;
- source code, diffs, command output, or approval payloads;
- customer/product screenshots and attachments;
- vendor credentials or organization information;
- directly exploitable details of an unpublished security vulnerability.

Handle security vulnerabilities through the private reporting path in [SECURITY.md](SECURITY.md).

## 4. Issue classification

### Service / daemon

Typical symptoms: cannot install/start, stale lock, log rotation, different CWD opens different data, Runtime capacity/queue, Registry unhealthy.

### Project / Room lifecycle

Typical symptoms: Project unavailable, duplicate canonical root, Room provisioning, Existing Binding conflict, Legacy pending, archive/restore.

### Room data

Typical symptoms: Event sequence, future schema, attachment hash, backup/restore, state that does not close after restart.

### Browser

Typical symptoms: Management refresh 401, Room session/CSRF, SSE disconnect, history paging, image preview, mobile overflow.

### Vendor Runtime

Typical symptoms: `doctor` probe, Claude control initialize, Codex app-server request, Grok streaming-json turn, Session/Thread resume, permission/sandbox, a real Turn stuck.

More accurate classification makes it easier not to treat a vendor service outage as a PairRoom Store bug, or a tab Token loss as a daemon failure.

## 5. Compatibility policy

PairRoom follows the current stable public Claude Code / Codex / Grok Build interfaces. It does not maintain a permanent compatibility matrix for obsolete CLIs. After updating any vendor CLI:

```bash
pairroom doctor --repo /absolute/path/to/safe-test-repo
```

and complete a real smoke on a non-critical repository. See [Runtime compatibility](docs/RUNTIME_COMPATIBILITY.md) for the detailed policy.

## 6. Current support boundary

Currently supported:

```text
one local Service per data root
multiple canonical Git Projects
multiple durable Rooms
bounded active Room Runtimes
one human + two Agent slots per Room
each slot: Claude Code, Codex, or Grok Build (same runtime allowed twice)
```

Not in the current support contract:

- multi-user hosting, team RBAC, cloud sync;
- a direct LAN/public listener or built-in TLS;
- remote workers;
- more than two Agent slots in one Room;
- Project removal / permanent Room deletion;
- full Runtime policy hot modification;
- Reviewer container-grade security guarantees;
- a stable plugin API for additional vendors.

## 7. Feature requests

A feature request should explain:

- a concrete user workflow, not only “support tool X”;
- why the existing Service/Room/Driver/Reviewer model is insufficient;
- fact sources, failure recovery, and migration needs;
- security, privacy, and multi-Room identity/capacity impact;
- whether it would weaken official harness native capability;
- the smallest verifiable acceptance criteria.

Read [Product plan](docs/PRODUCT_PLAN.md) and [Architecture](docs/ARCHITECTURE.md) first, and avoid requesting documented non-goals.
