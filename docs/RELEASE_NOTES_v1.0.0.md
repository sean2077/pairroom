# PairRoom 1.0.0

PairRoom 1.0 is the first stable release of the local three-party collaboration room for a human, official Claude Code, and official Codex.

## Highlights

- Shared IM-style Markdown conversation with replies, threads, search, pagination, drafts, unread state, notifications, and rich image galleries.
- Native Claude Code stream-json/control integration and native Codex app-server integration; PairRoom does not replace either Agent Harness.
- User images delivered as native multimodal inputs to both Agents, plus safe preview of repository-local images referenced by Agent responses.
- Driver/Reviewer workflow with an independent Git snapshot containing HEAD, dirty tracked changes, and untracked regular files.
- Explicit append, next-turn, supersede, cancel, retry, delivery, and processing semantics.
- Durable structured Work Inspector for turns, tools, commands, plans, diffs, usage, approvals, and failures.
- Strict data verification, verified backup/restore, and redacted diagnostics commands.
- HttpOnly browser sessions, CSRF protection, query-token removal, same-origin/Host protection, CSP, and API rate limiting.
- Cross-platform static binaries, source archives, SHA-256 checksums, SPDX SBOM, and build provenance.

## Stable 1.0 scope

One PairRoom daemon manages one Git repository with one human, one Claude Code participant, and one Codex participant. One participant should normally be the Driver and the other the Reviewer.

Multi-user hosting, cloud sync, team RBAC, additional Agent vendors, and a hosted PairRoom service are not part of 1.0.

## Upgrade

Stop PairRoom, verify and back up the room data, install 1.0, run `doctor`, then open the room once with Agent auto-start disabled. See `docs/UPGRADING.md` and `docs/OPERATIONS.md`.

## Native runtime validation boundary

The release pipeline fully verifies PairRoom's adapters, state machines, room, browser, mock runtimes, media, isolation, backup, restore, and packaging. A real model/network run still requires official Claude Code and Codex to be installed and authenticated on the target machine. The release does not claim such a run when those CLIs are unavailable in the build environment.
