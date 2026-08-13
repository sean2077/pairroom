# Security Policy

## Threat model

PairRoom launches highly capable local coding agents. Those agents may read files, modify a repository, execute commands and call configured MCP servers. PairRoom's UI is not the primary execution sandbox; the official Claude Code and Codex permission systems remain the security boundary.

## Safe defaults

- HTTP binds to `127.0.0.1:7332` by default.
- A non-loopback bind automatically generates a bearer token when none is supplied.
- The UI removes the token from the visible URL and retains it only in `sessionStorage` for that tab.
- API requests enforce same-origin checks.
- Security headers include CSP, `frame-ancestors 'none'`, no-referrer and no-sniff.
- State directories are created with mode `0700`; event and runtime-prompt files use `0600` on POSIX systems.
- Codex Reviewer turns use a read-only sandbox policy.
- Unsupported Codex server requests fail closed rather than receiving a generic approval.
- Additional Codex permission requests can receive only the subset originally requested.

## Important caveats

### No built-in TLS

Do not expose PairRoom directly to the public internet. For remote access, bind to loopback and use an authenticated SSH tunnel, VPN, or trusted reverse proxy with TLS.

### Local transcripts may be sensitive

`events.jsonl` can contain:

- user prompts
- Agent responses
- file names and diffs
- command output
- tool arguments
- error messages

Treat the PairRoom data directory as sensitive project data. Do not commit it.

### Vendor data handling still applies

When Claude Code or Codex uses a cloud model, repository content and tool results may be sent to the corresponding provider under that product's terms and settings. PairRoom does not change that data path.

### Existing Agent customizations remain active

The native CLIs can load user/project configuration, Skills, MCP servers, Hooks and plugins. A malicious or overly broad customization can expand what an Agent can access. Review those configurations separately.

### Claude Reviewer isolation

In v0.1, Reviewer behavior is strongly instructed but not always enforced with a per-turn filesystem sandbox for Claude Code. Codex receives a read-only policy. Use a dedicated read-only checkout or conservative Claude permission configuration for untrusted review tasks.

### Shared working tree

Two agents can technically write the same working tree when permissions allow it. PairRoom recommends a single Driver and a Reviewer. Concurrent writes can create semantic conflicts even when Git reports no textual conflict.

## Recommended operation

1. Run only on trusted repositories.
2. Keep real secrets outside the repository or deny Agent read access.
3. Start with conservative vendor permissions.
4. Review command/file approval details before accepting.
5. Keep the HTTP listener on loopback.
6. Back up or remove room state according to the repository's sensitivity.
7. Use separate worktrees when intentionally asking both agents to implement.

## Reporting vulnerabilities

Do not open a public issue containing secrets, exploit payloads or private repository data. Report the minimum reproducible description to the maintainer through a private security channel once the repository is published.
