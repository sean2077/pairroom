# Contributing to PairRoom

## Development setup

Install Go 1.25, Node.js (CI uses 22.x), Python 3, Git, Make, and Bash. The root CLI is CGo-free; the race test additionally needs the compiler described below.

```bash
git clone https://github.com/sean2077/pairroom.git
cd pairroom
make check
make smoke
```

`make check` runs format, static checks, unit tests, race / dependency checks, JavaScript syntax/client regressions, and the documentation contract. `make js-check` runs the fast JavaScript layer alone and fails with an actionable error when Node.js is missing; it never silently skips frontend verification. The dependency check rejects local/versioned replacements as well as module/version drift. `make smoke` runs the full Mock collaboration / recovery scenario. `make race` (included in `make check`) requires `CGO_ENABLED=1` and a Go-supported C compiler on `PATH`; on Windows, use an MSYS2 MinGW toolchain or an equivalent supported compiler.

UI changes also run the isolated browser contract in CI. To reproduce it locally:

```bash
python3 -m venv .browser-venv
.browser-venv/bin/python -m pip install -r scripts/requirements-browser.txt
.browser-venv/bin/python -m playwright install chromium
make browser-check PYTHON=.browser-venv/bin/python
```

On Windows, the environment's interpreter is `.browser-venv/Scripts/python.exe`. A managed Linux machine may need Playwright's `install --with-deps chromium` command. To use an already installed Chromium, set `PAIRROOM_BROWSER_EXECUTABLE` to its executable path.

The fast browser regression suites load the real embedded assets with deterministic in-page HTTP/SSE fixtures and write screenshots plus assertions to `.browser-results/`. They cover IME input, duplicate submissions, draft retention, reconnect bursts, scroll anchors, older date separators, optional status/reply rows, English/Chinese light/dark responsive views, native approval drafts/options, Management tab focus and surface identity, external archive/removal, and configuration edits and unavailable explicit Provider selections during catalog refresh. Management frame identities are inert fixtures; Room assets are exercised separately. These fixture suites do not verify browser authentication, vendor processes, or model behavior. Inspector regressions additionally cover full-ID disclosure state, selection/focus retention, scroll anchors, deferred hidden rendering, and expanded tool evidence. The client suite also verifies pending native actions after DOM replacement.

Room naming regressions load the real external Management assets under the production Content Security Policy and verify optional names, context-menu targeting/keyboard focus, rename failures, and viewport positioning. CI runs this CSP check by default. In an isolated browser that forbids navigation, `python3 scripts/test_management_browser.py --in-page-fixture` runs the interaction checks with inline fixture assets instead; its results explicitly report that CSP was not verified. Neither form authenticates to real services or runs a vendor model.

`scripts/test_service_browser.py` is a separate real-browser Mock Service smoke, also included in `make browser-check` and CI. It builds the current CLI (or accepts `--binary`), creates a disposable Git repository and isolated home/data root, then exercises real bootstrap-cookie authentication, missing-CSRF rejection, custom Room creation, the same-origin Room gateway, SSE-backed output, settings, permissions, rename, and restart. It does not mock `fetch` or EventSource and does not launch vendor CLIs. Results and screenshots go to `.browser-results/service/`; temporary credentials and runtime state are removed on exit. A managed browser that blocks loopback navigation cannot run this layer; report it as unverified locally rather than relaxing browser policy or replacing its HTTP with fixtures.

`node scripts/test_management_client.js` additionally checks post-mutation freshness, stale browser-session responses, catalog ordering, and duplicate action guards without browser timing. Native approval contract and concurrency tests run as part of `make check`.

For telemetry/tail allocation regressions, use `go test ./internal/room -run '^$' -bench 'Benchmark(TextDeltaSummaryProjection|RecentEventTail)$' -benchmem`. Measure these isolated operations separately from model token billing or whole-request latency.

For message-window allocation regressions, use `go test ./internal/room -run '^$' -bench BenchmarkWindowedSnapshot -benchmem`. Compare allocations at a fixed window size rather than imposing a machine-dependent timing threshold.

## Change workflow

1. Create a short-lived branch from latest `main`;
2. Write the behavior invariants and failure boundaries first;
3. Change the minimum necessary code and documentation;
4. Add tests that cover real state transitions;
5. Run verification and list it honestly in the PR;
6. Land through a PR; do not push directly to `main`.

Fixes involving concurrency and recovery should at least cover: the happy path, cancel, process exit, restart, late events, and duplicate callbacks.

## Documentation ownership

[docs/README.md](docs/README.md) defines the unique responsibility of each document. Do not copy the same collaboration semantics into the README, architecture, operations, and troubleshooting guides.

Rules:

- Long-lived Reference records current behavior only; plans and one-off reviews belong in an Issue / PR;
- Do not hard-code the current release in the README;
- CLI flags have `cmd/pairroom/` as the source of truth;
- HTTP routes have `internal/server/` and `internal/service/` as the source of truth;
- JSON fields have `internal/config/` struct tags as the source of truth;
- Event schema has `internal/model/types.go`, apply code, and `internal/store/` as the source of truth;
- Breaking changes update `CHANGELOG.md` and `docs/UPGRADING.md` together.

Before committing:

```bash
make docs-check
```

## Testing claims

Distinguish clearly:

- unit / race tests;
- Mock E2E;
- build and cross-build;
- real Claude Code / Codex / Grok Build native E2E.

Do not describe Mock results as native E2E when the official CLIs were not run.

## Pull request

A PR should include: the problem, design boundaries, user-visible changes, migration impact, verification commands, what was not verified, and how to recover. Prefer splitting a large refactor into independently reviewable commits, but do not keep temporary workflows that exist only to shuffle patches or export source.
