# Contributing to PairRoom

## Principles

- Preserve the official Claude Code and Codex harnesses.
- Prefer documented structured protocols over terminal scraping.
- Fail closed for permissions and unknown server requests.
- Persist before publishing.
- Keep agent discussion bounded and user-preemptible.
- Do not add a framework dependency for functionality that can remain a small local abstraction.

## Development setup

```bash
go version   # 1.23+
make fmt
make test
make test-race
make vet
make build
```

Run the deterministic demo:

```bash
make demo
```

## Tests

A change to routing, persistence or protocol translation should include tests for:

- message targeting and role context
- user preemption
- hop limits or stop markers
- restart replay
- malformed/unknown protocol input
- permission responses that grant no more than requested

Native adapter changes should be validated against currently supported official CLI versions on a disposable repository.

## Code layout

```text
cmd/pairroom          CLI entry point
internal/agent        vendor adapters and mock runtime
internal/room         event-sourced collaboration engine
internal/model        canonical data model
internal/store        append-only persistence
internal/server       HTTP/SSE server and embedded UI
internal/prompt       room envelope and mention parsing
```

## Pull requests

Keep changes focused. Explain:

1. which invariant or user workflow changes;
2. whether the vendor protocol shape is documented and where;
3. how failure and restart behavior were tested;
4. whether security or permission behavior becomes broader.
