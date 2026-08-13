# Contributing

PairRoom's priority is reliable use of official Claude Code and Codex Harnesses, not rapid expansion to many model APIs.

## Development checks

```bash
make fmt
make test
make race
make vet
node --check internal/server/assets/app.js
```

Before opening a change:

- Keep the Go core free of third-party modules unless there is a compelling reviewed reason.
- Do not parse terminal ANSI output to infer runtime state when a structured vendor protocol exists.
- Do not add undocumented vendor request fields merely for convenience.
- Preserve append-only event history; corrections should be new events/messages.
- Treat unknown approval/server requests as fail-closed.
- Add tests for lifecycle ordering, restart recovery and concurrent delivery races.
- Update protocol, architecture, product plan and validation docs when behavior changes.

## Protocol fixtures

Real vendor fixtures must be scrubbed of repository contents, prompts, credentials, tokens, local user paths and project names before they are committed.
