# PairRoom Product Plan

## Product definition

PairRoom is a local collaboration control surface for existing top-tier Coding Agent Harnesses. A human, official Claude Code, and official Codex share one visible room; the official Harnesses continue to own reasoning, tools, context, sandbox, sessions, Skills, MCP, and project instructions.

PairRoom is not a model gateway, replacement Agent loop, terminal-output parser, hosted collaboration service, or credential broker.

## 1.0 principles

| Principle | Contract |
|---|---|
| Native Harness | PairRoom uses structured official interfaces and does not emulate Claude Code/Codex with generic model APIs. |
| Three-party visibility | Human and both Agents share one readable public timeline. |
| Process separation | Conclusions stay in chat; tools, commands, plans, diffs, usage, and approvals stay in the Inspector. |
| Human priority | New human instructions can cancel/supersede stale automatic handoff. |
| Single writer by default | Driver uses the live tree; Reviewer uses an independently materialized snapshot and native read-only policy. |
| Durable honesty | Delivery and processing are separate, terminal states are explicit, retries create new records, and restarts settle orphaned state. |
| Fail closed | Unsafe workspace creation, unknown privileged requests, invalid attachments, and corrupt restores are rejected. |
| Local-first privacy | No PairRoom cloud, telemetry, hosted credentials, or automatic remote-image fetch. |

## Delivered milestones

- **v0.1:** native adapters, shared room, routing, roles, SSE/JSONL, Git inspector, Mock mode.
- **v0.2:** delivery/processing lifecycle, retries, runtime probing, restart settlement, export/security hardening.
- **v0.3:** safe Markdown, native multimodal images, gallery, Claude control approvals, native Reviewer policies.
- **v0.4:** Reviewer Git snapshot with dirty/untracked state, boundary metadata, role-switch rollback.
- **v0.5:** append/next-turn/supersede/cancel semantics and stale-handoff suppression.
- **v0.6:** durable structured Turn/Tool/Command/Plan/Diff/Usage summaries.
- **v0.7:** strict verification, self-verifying backup/restore, redacted diagnostics.
- **v0.8:** long-room pagination, drafts, unread state, notifications, enhanced image viewer.
- **v0.9:** HttpOnly browser sessions, CSRF, query-token removal, rate limiting.
- **v1.0:** stable contract, CI, release automation, four-platform artifacts, SBOM, provenance, operations/privacy/support documentation.

## Stable 1.0 boundary

```text
one daemon
one Git repository
one human
one Claude Code participant
one Codex participant
one Driver + one Reviewer by default
```

The stable contract does not include multi-user identity, remote workers, cloud synchronization, team RBAC, hosted TLS, or additional Agent vendors.

## Post-1.0 priorities

1. Real-world dogfooding reports from current official Claude Code and Codex releases.
2. Better per-file Diff/test cards and explicit verification artifacts in the shared room.
3. Optional OS/container-grade Reviewer isolation without weakening the portable default.
4. Room archive/list management while keeping each active room single-repository.
5. Structured external RuntimeAdapter protocol only after the native two-Agent path remains stable.

## Explicit non-goals

- hosting or reselling model keys;
- replacing vendor Agent loops;
- parsing ANSI terminal output to infer state;
- silently uploading private code or images to a PairRoom service;
- replacing GitHub/GitLab PR review or CI;
- maintaining a permanent compatibility matrix for obsolete vendor CLIs.
