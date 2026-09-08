# PairRoom security policy

[Architecture](docs/ARCHITECTURE.md) · [Data and privacy](#9-sensitive-local-data) · [Operations](docs/OPERATIONS.md) · [Support](SUPPORT.md)

## 1. Threat model

PairRoom starts high-privilege local coding Agents. Native tools, Skills, MCP, Hooks, plugins, and subprocesses may read/modify files, run commands, and access the network. PairRoom's UI and Room scheduler do not replace native permission, sandbox, or organization policy.

The intended environment is one user, a trusted local machine, and a trusted repository. There is no security boundary against a malicious same-user local process, compromised OS, or kernel. The control plane addresses unauthorized browser/API access, CSRF/DNS rebinding, credential exposure, unsafe attachments/imports, ambiguous high-privilege native requests, and divergence between durable Room facts and runtime ownership.

**New Rooms default to YOLO for both participants.** Lower approval friction is not stronger security. Lead/Executor are responsibilities, not tool restrictions. There is no automatic Agent-relay count or cost ceiling. Use explicit native restrictions and controlled execution environments for tasks that require them.

## 2. Network security defaults

### 2.1 Numeric loopback only

`pairroom service`, `pairroom serve`, and Room listeners accept only numeric loopback addresses. Wildcard, LAN/public, hostname, and `localhost` binds are rejected before state is opened. **A token does not enable a non-loopback listener.** Tokenless compatibility `serve` still performs loopback Host and same-origin checks.

There is no built-in TLS or remote listener. Use SSH local port forwarding for remote access while retaining loopback and authentication checks. Treat the forwarded endpoint as access to local repositories and high-privilege Agent tools, not a multi-user hosting interface.

### 2.2 Management Shell authentication

Without an explicit Service token, PairRoom generates a random Management Bearer token and puts it in the startup URL fragment. The browser can enter through that full URL, or through the origin's login form using the token/full URL.

The browser removes the fragment with `history.replaceState`, exchanges the token through `POST /api/v1/session`, and receives a 12-hour sliding-expiry `HttpOnly`, `SameSite=Strict` cookie scoped to `/api/v1/`. CSRF is kept only in page memory; session-authenticated mutations send `X-PairRoom-CSRF`.

The bootstrap token is cleared from page memory/input after exchange. Neither tokens, session IDs, nor CSRF are stored in Web Storage. Refresh can recover CSRF via `GET /api/v1/session` while the cookie is valid. Restart, expiry, logout, or another browser context requires authentication again; explicit logout uses `DELETE /api/v1/session`. CLI/API clients may use a Bearer header. A query-string token does not authorize Management requests.

### 2.3 Room View authentication

A Service-managed Room has an independent token; compatibility `serve` may configure one. When token authentication is enabled, the browser exchanges a fragment credential for a 12-hour sliding-expiry `HttpOnly`, `SameSite=Strict` session cookie and uses per-session CSRF for writes. Tokens and CSRF do not enter query strings or Web Storage. REST, SSE, and attachments do not accept a query token as authorization.

Room A's token/session/CSRF, event cursor, and attachment authorization cannot authorize Room B. The Management same-origin Room gateway is not permission to transfer Room identities or reuse stale actions against a different embedded surface.

### 2.4 HTTP protections

Management mutations check origin/fetch-site context; cookie-authenticated writes additionally require CSRF. Room requests perform Host and same-origin checks, with CSRF for enabled browser sessions. Room rate limiting reduces local abuse and accidental request loops; it is not an Agent spending budget.

Both surfaces set CSP, no-referrer, no-sniff, and default `frame-ancestors 'none'`. Only the Management same-origin Room surface uses `frame-ancestors 'self'`; a direct Runtime URL remains unframeable. Attachments require the relevant authentication and use no-sniff, ETag, and inline disposition.

URL fragments are not sent as HTTP requests/Referer, but can leak through screen sharing, copied startup output, or browser extensions. Do not publish a complete Management or Room URL.

## 3. Project, Room, and Binding

Projects use absolute paths explicitly entered by the user, canonicalized through symlink and Git worktree-root resolution. PairRoom does not discover repositories by scanning common development directories or provide a general server filesystem browser.

Room provisioning is private until atomically published. The Service enforces Binding uniqueness by durable slot and native session ID; archive does not release ownership. An existing Binding must resume exactly. A deferred new Binding materializes only after real native input acceptance. Event/checkpoint/uniqueness failures fail closed rather than creating another owner.

The Binding's `agent` is the stable slot (`claude`/`codex`), not an assumption about its selected Runtime. PairRoom does not import the vendor transcript from before the Binding.

## 4. Attachment safety

Only verified PNG, JPEG, GIF, and WebP images are accepted. SVG, HTML, scripts, and arbitrary binaries are rejected. Content signatures, not filename/MIME alone, determine acceptance. Limits cover count, individual/combined size, edge length, and pixel count.

Attachments and manifests use opaque IDs and conservative permissions. Resolve checks size, regular-file/non-symlink status, dimensions, and SHA-256 again. Accepted Message image identity cannot be silently changed. Repository image import enforces canonical path/symlink boundaries; remote URLs are not automatically imported. Committed transcript attachments cannot be removed through the attachment DELETE API.

The API/transcript carries verified metadata, not an absolute host attachment path. Adapter-local resolution occurs only at the native boundary. Browser object URLs are transient, not persistent public links. Image validation cannot detect whether a screenshot visibly contains a secret; inspect content before sending or sharing.

## 5. Runtime and approvals

### 5.1 Claude

Native control initialize must succeed. Unknown control requests error; native tool/question requests enter the Room approval lifecycle. A read-only profile or preserved enforced legacy Reviewer uses plan permissions and blocked write tools, with another fail-closed control check for write requests that still arrive.

### 5.2 Codex

Unknown app-server requests fail closed. A read-only profile or preserved legacy Reviewer uses the read-only sandbox. Additional permissions can only be granted within the requested scope. Command/file/additional-permission requests use the approval lifecycle. A generic diagnostic `error` does not by itself prove a Turn ended.

### 5.3 Grok Build

Unspecified Provider/model/effort/native-policy overrides retain native inheritance. Prompt and instruction text travels through long-lived ACP stdio, not argv or a prompt-file transport. Supported configured credentials are passed in the child environment; known values are redacted at the relevant log/diagnostic boundaries.

PairRoom advertises `terminal=false`, retaining native tool execution. Permission choices retain the vendor's exact option identities; cancellation is cancelled, not remembered authorization. Unknown high-privilege reverse requests fail closed.

### 5.4 Approval lifecycle

Interrupt, stop/restart, terminal failure or confirmed exit, permission replacement, and PairRoom restart expire pending requests that cannot safely be reused. A stale browser decision must not authorize a new vendor request. Invalid or incomplete answers remain answerable rather than consuming the request.

Modern permission changes require an idle Room, empty FIFO, and no pending approval. Intent precedes effects; the old adapter stops before the effective policy is committed and the replacement starts. Failure cannot grant broader fallback access. Collaboration instructions and native session identity remain intact. Legacy role mutation is not a public operation. Exact wire semantics are in [API reference](docs/API_REFERENCE.md#native-approval-responses).

## 6. Workspace and responsibility boundaries

Modern Lead and Executor share the live workspace and default to YOLO. One native Turn owner is enforced **per Room**, not as a repository-wide lock or isolation from native children, MCP, Hooks, external editors, or other Rooms. A “reviewer” instruction does not create an independent read-only copy. Select actual native restrictions and use controlled containers/VMs or independently managed workspaces when isolation matters.

Legacy role-bound Reviewer snapshots preserve HEAD, staged/unstaged tracked changes, and untracked regular files; unsafe symlinks/out-of-bound references are rejected. The snapshot records provenance and removes write bits on POSIX, then layers the native read-only/plan policy. It is not a container, VM, read-only mount, or malware sandbox. Windows semantics, native bugs, external tools, and user configuration can widen access.

Legacy Driver/Reviewer boundaries remain legacy; upgrading does not convert them to modern YOLO. For independent parallel writing tasks, manage separate worktrees/branches and explicit merges rather than relying on Room labels.

## 7. Persistence and recovery

Data directories/files use conservative permissions where supported. Auditable events are synced before publication; high-frequency transient telemetry need not be durable. Sequences must begin at 1 and remain contiguous. Room identity is verified before repairing or appending to a published Room. Missing/empty/replaced histories, middle corruption, and unsupported schemas fail closed; only an incomplete final record can be repaired automatically.

The Registry can be rebuilt from authoritative Room records. Checkpoint failure blocks mutations when consistency cannot be proven. One Service owns a data root. Recover a crash-stale lock only after proving its recorded PID is gone; do not delete a live owner's lock.

Backup/restore validates paths, links, duplicates, declared files, bounds, hashes, and archive integrity. Outputs must be outside the source Room directory, including symlink aliases. A Room archive excludes the user's repository and native session stores; a full Service rollback needs a separate offline data-root backup. [Storage](docs/STORAGE.md) and [Operations](docs/OPERATIONS.md#backup) own the procedures.

Recovery does not re-execute uncertain or accepted native work automatically. The Event Log is not an exactly-once side-effect mechanism or a tamper-proof compliance ledger. Do not hand-edit sequence/schema/Binding/image identity fields to bypass verification.

## 8. Runtime capacity and shutdown

Capacity reclamation does not interrupt an active Turn. Cleanup-uncertain Runtimes continue to occupy capacity rather than being reported as safely suspended. Graceful shutdown stops Management mutation, drains admitted work/native Turns, then releases stores and the Service lock.

Forced termination can leave native side effects, stale locks, and pending state that needs reconciliation. Prefer explicit lifecycle actions. Desktop Quit drains only an embedded Service it owns; an external daemon remains running. Closing the window merely hides it. See [Operations](docs/OPERATIONS.md#desktop-lifecycle).

## 9. Sensitive local data

Room data can contain prompts, replies, source/diffs, filenames, command/tool output, errors, approval details, session IDs, and screenshots/customer material. Treat Event Logs, attachments, backups, and exports as private code assets.

Ordinary transcript export excludes the verbose Inspector tail. Diagnostics are designed to omit transcript bodies and attachment bytes, but can retain structured errors, paths, and environment facts. Neither is a blanket redaction guarantee for arbitrary user/Agent content. Inspect files before sharing them.

## 10. Vendor data path and custom configuration

Cloud-model requests can include code, images, and tool results sent through the selected native CLI to its Provider. PairRoom does not change or encrypt that vendor path. Local coordination/storage is not offline inference, an on-device model guarantee, or a promise that code never leaves the machine.

Native user/project configuration, Skills, MCP, Hooks, and plugins remain active within supported adapter behavior; PairRoom does not audit them. Unspecified overrides inherit native configuration. Malicious or broad native configuration can widen access.

Supported CC Switch references are read-only and re-resolved at creation/activation; changing an external Profile can affect a later activation. PairRoom does not change CC Switch's current Profile or maintain a second credential database. Unsupported credential/proxy/schema cases fail closed. [Configuration](docs/CONFIGURATION.md) owns the exact supported boundary.

## 11. Remote resources

PairRoom does not automatically load remote Markdown images. Opening an ordinary external link deliberately sends the browser to that site; the site then receives a normal network request. Review remote content and native tools with the same trust assumptions as other project inputs.

## 12. Recommended practice

Use trusted repositories and explicit native permissions; do not mistake responsibilities or natural-language “plan first” for enforcement. Keep unnecessary secrets out of the execution environment, review approval scope and screenshots, protect tokens/data/backups, and verify native CLI upgrades on a disposable read-only task before important work. Maintain normal shutdown and verified backups. Use controlled isolation for untrusted execution and keep the listener on numeric loopback.

## 13. Vulnerability reports

Do not post exploit details, credentials, private code, real attachments, tokens/cookies, or complete startup URLs to a public Issue. Prefer the repository's private security reporting channel, with a minimal reproduction, affected version/platform, threat assumptions, and expected boundary.

If no private channel is available, first open a public Issue without exploit details asking maintainers to establish one. Ordinary bug-report evidence and redaction guidance are in [Support](SUPPORT.md).
