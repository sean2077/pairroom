# Security Policy

## Threat model

PairRoom launches highly capable local coding agents. Those agents may read files, modify repositories, execute commands and call configured MCP servers. PairRoom's Web UI is a control and observation surface, not the primary execution sandbox; the official Claude Code and Codex permission systems remain important security boundaries.

## Safe defaults

- HTTP binds to `127.0.0.1:7332` by default.
- A non-loopback bind automatically generates a bearer token when none is supplied.
- The UI removes a URL token from browser history and keeps it in per-tab `sessionStorage`.
- Query-string tokens are accepted only by the read-only SSE endpoint; all other APIs require the `Authorization` header.
- A tokenless server accepts only loopback Host headers, reducing DNS-rebinding exposure.
- API requests enforce same-origin checks.
- Security headers include CSP, `frame-ancestors 'none'`, no-referrer and no-sniff.
- State directories use `0700`; event and runtime-prompt files use `0600` on POSIX.
- Codex Reviewer turns request a read-only sandbox.
- Unsupported Codex server requests fail closed.
- Additional Codex permissions can receive only the subset originally requested.
- Store schema newer than this binary is rejected instead of being rewritten.
- Normal transcript export excludes verbose Inspector event data unless explicitly requested.

## Important caveats

### No built-in TLS

Do not expose PairRoom directly to the public internet. For remote access, keep it on loopback and use an authenticated SSH tunnel/VPN, or place it behind a trusted TLS reverse proxy. A bearer token does not protect plaintext traffic from network observers.

### Local transcripts are sensitive

`events.jsonl` can contain:

- user prompts and Agent responses
- file names, diffs and tool arguments
- command output and errors
- local paths, model/runtime diagnostics
- approval details

Treat the PairRoom data directory as project-sensitive. Do not commit or casually share it.

Markdown and normal JSON exports intentionally omit Inspector events, but still contain the full human/Agent conversation. `include_events=1` creates a more sensitive forensic export.

### Vendor data handling still applies

When Claude Code or Codex uses a cloud model, repository content and tool results may be sent to the corresponding provider under that product's terms and settings. PairRoom does not change or proxy that data path.

### Existing Agent customizations remain active

Official CLIs can load user/project configuration, Skills, MCP servers, Hooks and plugins. A malicious or overly broad customization can expand access. Review those configurations separately.

### Claude Reviewer isolation

Reviewer instructions do not guarantee OS-level read-only access for Claude Code. Codex receives a read-only sandbox policy, but actual enforcement depends on the installed runtime and platform. Use a dedicated read-only checkout or conservative vendor permissions for untrusted review tasks.

### Shared working tree

Two agents can technically write the same working tree when permissions allow it. PairRoom recommends one Driver and one Reviewer. Concurrent writes can create semantic conflicts even when Git has no textual conflict.

### Runtime probing

`pairroom doctor` and adapter startup run only CLI `--version`/`--help`/`app-server --help` checks. Wrapper scripts are executable code; configure only trusted commands and paths.

## Recommended operation

1. Run only on trusted repositories.
2. Keep secrets outside the repository or deny Agent access.
3. Start with conservative vendor permissions.
4. Review command/file approval details before accepting.
5. Keep the listener on loopback.
6. Back up or remove room state according to repository sensitivity.
7. Use separate worktrees when intentionally asking both Agents to implement.
8. Run `pairroom doctor` after upgrading either vendor CLI.

## Reporting vulnerabilities

Do not open a public issue containing secrets, exploit payloads or private repository data. Once the repository is published, use its private security-reporting channel and include only the minimum reproducible details.
