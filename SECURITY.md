# PairRoom security policy

> [Architecture](docs/ARCHITECTURE.md) · [Privacy model](docs/PRIVACY.md) · [Operations](docs/OPERATIONS.md) · [Support scope](SUPPORT.md)

## 1. Threat model

PairRoom starts high-privilege local coding Agents. An Agent may read files, modify a repository, run commands, access the network, and invoke user-configured Skills, MCP, Hooks, and plugins. The PairRoom Web UI is a control and observation surface. It does not replace Claude Code, Codex, or Grok Build permissions, sandbox, or organization policy.

PairRoom also stores sensitive discussion, runtime events, and images, so the focus is:

- unauthorized browser access to the Management/Room API;
- DNS rebinding, cross-origin commands, and CSRF;
- Token leakage through URL, history, Web Storage, logs, or screenshots;
- malicious images, forged media types, and resource exhaustion;
- path traversal, symlink escape, and importing files outside the repository;
- implicit access leakage from remote Markdown images;
- unknown high-privilege vendor requests being allowed by mistake;
- UI role disagreeing with actual Runtime permission;
- leftover approvals, locks, or “ghost working” after a crash;
- Registry, Event Log, and Binding ownership divergence;
- treating Reviewer as container-grade isolation.

PairRoom primarily targets a single user, a trusted local machine, and a trusted repository. It does not provide a security boundary against a malicious local same-user process, a compromised kernel, or an untrusted OS.

## 2. Network security defaults

### 2.1 Numeric loopback only

- `pairroom service`, `pairroom serve`, and each Room Runtime accept only numeric loopback addresses;
- Wildcard addresses, LAN/public addresses, hostnames, and `localhost` are rejected before opening repository or Service state;
- Tokenless `serve` still performs loopback Host and same-origin checks;
- PairRoom has no built-in TLS or remote listener;
- Remote access uses only SSH local port forwarding.

A Bearer Token is defense in depth, not a substitute for transport encryption.

### 2.2 Management Shell authentication

When the Service is not given an explicit Token, it generates a random Management Bearer Token and places it in the startup URL fragment.

The browser flow supports two entries:

1. Full Management URL: JavaScript reads the Token from the fragment and immediately removes the fragment from the address bar with `history.replaceState`;
2. Opening the Management origin directly: if there is no recoverable Session, a credential login page accepts the configured Service Token or a complete Management URL containing `#token=...`;
3. Both entries use `Authorization: Bearer <token>` once to call `POST /api/v1/session`;
4. The Service returns a 12-hour sliding-expiry `HttpOnly`, `SameSite=Strict` Session Cookie path-limited to `/api/v1/`, plus a CSRF Token kept only in page memory;
5. The bootstrap/login Token is then cleared from page memory and the input box. Later browser requests use the Session Cookie, and mutations must also provide `X-PairRoom-CSRF`;
6. Explicit logout calls `DELETE /api/v1/session`. When the Session is invalid, the page returns to the login entry.

Management Token, Session ID, and CSRF are not written to `localStorage`/`sessionStorage`. On refresh, the page can recover CSRF from a still-valid Cookie via `GET /api/v1/session`. Service restart, session expiry, explicit logout, or a new browser context requires the Service Token again. CLI/API clients may keep sending a Bearer Header. A query-string token does not authorize the Management API.

### 2.3 Room View authentication

A Service Room Runtime automatically uses an independent Token; compatibility `serve` may set a Token explicitly. When a Token is enabled:

1. Startup credentials appear only in the URL fragment;
2. The browser exchanges them through the bootstrap endpoint for a 12-hour sliding-expiry `HttpOnly`, `SameSite=Strict` Session Cookie;
3. Writes require a per-session CSRF Token;
4. Long-lived Tokens and CSRF do not enter URL query or Web Storage;
5. CLI/API clients may keep using the Authorization Header;
6. A query token does not authorize REST, SSE, or attachment APIs.

Room A's Token, Session, CSRF, SSE cursor, and attachment authorization cannot be used for Room B.

### 2.4 HTTP protections

- The Management API accepts a direct Bearer or a valid browser session. Session-authenticated mutations require CSRF, and all mutations also check `Sec-Fetch-Site`/Origin;
- The Room API performs Host and same-origin checks; when a browser session is enabled, mutations also perform CSRF checks;
- The Room API rate-limits by client with a fixed window to reduce local abuse and accidental loops;
- Both Web surfaces enable CSP, `frame-ancestors 'none'`, no-referrer, and no-sniff response headers. The Management same-origin Room surface changes only its own responses to `frame-ancestors 'self'`; a direct Runtime URL still forbids framing;
- Room attachment responses require authentication and use `nosniff`, ETag, and inline disposition;
- The startup fragment is not sent with HTTP requests or Referer, but it can still leak through screen sharing, log copy, or browser extensions.

## 3. Project, Room, and Binding

- A Project accepts only an absolute path the user entered explicitly;
- The server resolves symlinks, the Git worktree root, and canonicalizes;
- It does not scan common development directories and does not provide a server filesystem browser;
- Room provisioning completes in a hidden directory and is published atomically after full success;
- `(agent, vendor_session_id)` is globally unique inside the Service; archive does not release ownership;
- An Existing Binding must resume exactly;
- A deferred New Binding materializes only after the first real input is accepted;
- Event append, ownership checkpoint, or uniqueness failure interrupts execution and fails closed;
- PairRoom does not import the vendor transcript from before the binding.

`agent` in Binding identity is the durable slot (`claude` / `codex`), not the selected runtime.

## 4. Attachment safety

- Only PNG, JPEG, GIF, and WebP are accepted;
- SVG, HTML, scripts, and arbitrary binaries are rejected;
- Real content signatures are checked; file extension and client MIME are not trusted;
- Single-image size, per-message total size, count, edge length, and total pixels are limited;
- Files and manifests use random opaque IDs and conservative permissions;
- Every Resolve rechecks size, regular file, non-symlink, dimensions, and SHA-256;
- Message/API/export do not include the attachment's local absolute path;
- Repository image import goes through canonical path and symlink boundary checks;
- Remote URLs do not enter the automatic import flow;
- Attachments already in the durable transcript cannot be removed through a DELETE API;
- Object URLs exist only in the current page and are not persistent public links.

Images can still contain secrets, customer information, or other window contents that are visible to the eye. Format validation does not replace a human check before sending.

## 5. Runtime and approvals

### 5.1 Claude

- Startup must complete native control initialize;
- Unknown control requests return error;
- `can_use_tool`/`AskUserQuestion` enter the durable approval lifecycle;
- Reviewer uses plan permission mode and blocks write tools;
- The control layer fail-closes again on write requests that still arrive.

### 5.2 Codex

- Unknown app-server requests fail closed;
- Reviewer Turns use a read-only sandbox;
- Additional permissions can be granted only as a subset of the original request;
- command/file/additional-permission requests enter the unified approval lifecycle.

### 5.3 Grok Build

- Empty `provider`, `model`, `effort`, permission, and sandbox overrides are omitted so the native CLI user/global configuration is inherited;
- Prompt and instruction text are written to a prompt file and must not appear in process argv;
- Unknown high-privilege requests still fail closed; native approvals that the adapter cannot represent are rejected rather than auto-allowed.

### 5.4 Approval lifecycle

Interrupt, stop, restart, Runtime error/exit, role switch, and PairRoom restart expire pending approvals that cannot be reused safely. The UI must not replay an old decision onto a new vendor request.

Role changes follow “apply Adapter policy first, then persist the Room role”, so the UI cannot show Reviewer while the underlying process still runs with Driver permission.

## 6. Reviewer workspace boundary

The Reviewer runs in an independent Git snapshot by default:

- includes HEAD;
- applies staged + unstaged tracked diff;
- copies untracked regular files;
- rejects unsafe symlinks and out-of-bound references;
- records source HEAD, dirty, and snapshot digest;
- removes the write bit on POSIX;
- then layers Claude plan/disallowed tools or Codex read-only sandbox.

This is not a container, VM, read-only mount, or malware sandbox. External MCP, a vendor Runtime bug, Windows permission semantics, or user-custom configuration can widen access. For untrusted tasks, use a controlled container/VM/independent checkout.

The Driver is the only writer by default. A Reviewer snapshot should not be a parallel implementation branch. If two writers are required, use a human-managed independent Git worktree/branch and an explicit merge.

## 7. Persistence and recovery

- The data root uses private directory permissions. Events, prompts, images, and manifests use conservative file permissions when the platform supports it;
- Append-only events are synced before publish;
- Only a damaged final half-line is repaired;
- Mid-file corruption, sequence forks, or a future schema are rejected;
- A Registry checkpoint can be rebuilt from default Room Event Logs;
- If checkpoint write fails and consistency cannot be proven, later mutations are blocked;
- One data root allows only one Service writer;
- A stale lock is not guessed automatically; recover explicitly after confirming the old process has exited;
- Backup/restore rejects traversal, links, duplicates, undeclared files, size, and hash anomalies;
- Ordinary transcript export does not include the verbose Inspector event tail;
- Diagnostics are designed to omit transcript body and attachment bytes, but still need a human check.

Do not hand-edit Event sequence, Store schema, attachment manifest, or Binding Identity.

## 8. Runtime capacity and shutdown

- An active Turn is not interrupted for capacity reclaim;
- A queued Runtime can be cancelled;
- active+idle Runtimes can drain safely;
- busy, starting/stopping conflict, or cleanup-uncertain failed is not pretended to be suspended;
- A Runtime whose cleanup is uncertain continues to occupy capacity;
- Shutdown first stops Management mutation, then waits for in-flight management requests and Room Turns, then releases the lock.

Force-killing the process can leave a stale lock, pending approvals, or Processing state that needs replay to close. Prefer the normal daemon/Service lifecycle.

## 9. Sensitive local data

`events.jsonl` may contain:

- user prompts and Agent answers;
- filenames, diffs, tool arguments;
- command output, errors, and local paths;
- model/runtime diagnostics;
- approval details and Session/Thread IDs.

`attachments/` may contain error screenshots, product UI, architecture diagrams, data charts, and customer material. Treat the entire Room data, logs, backups, and exports as private code assets.

## 10. Vendor data path and custom configuration

When using a cloud model, code, images, and tool results may be sent to the corresponding vendor. PairRoom does not proxy, encrypt, or change that path.

Official CLIs still load user/project configuration, Skills, MCP, Hooks, and plugins. Malicious or overly broad configuration can widen read/write, network, and external-service access. PairRoom does not audit those configurations.

Empty PairRoom `provider`/`model`/`effort`/`instructions` inherit that native CLI global configuration. Add only explicit overrides.

## 11. Remote resources

PairRoom does not automatically load remote Markdown images. After the user actively opens an ordinary `https` link, the browser visits the target site directly; the target site sees a normal network request.

## 12. Recommended practice

1. Run only on trusted repositories;
2. Keep secrets where the Agent does not need to read them;
3. Default to one Driver and one Reviewer;
4. Start from conservative vendor permission/sandbox;
5. Review commands, paths, and permission scope carefully;
6. Check visible sensitive information before sending images;
7. Keep the listener on numeric loopback; use only SSH forwarding remotely;
8. After upgrading a vendor CLI, run `pairroom doctor` and a real smoke on a non-critical repository;
9. Verify/backup regularly and protect Room data according to sensitivity;
10. Use a container/VM/independent checkout when strong isolation is required;
11. Do not put complete Management/Room startup URLs in public logs or Issues;
12. Inspect diagnostics by hand before sharing.

## 13. Vulnerability reports

Do not attach secrets, private repository contents, real attachments, authentication Tokens, Cookies, complete startup URLs, or directly exploitable sensitive payloads to a public Issue.

Prefer the repository's private security reporting channel. Provide only a minimal reproduction, affected versions, platform, threat assumptions, and expected boundary. If a private channel is unavailable, first file a public Issue without exploit details asking maintainers to establish a security communication channel.
