# Contributing to PairRoom

Read [AGENTS.md](AGENTS.md) and the [project terminology](CONTEXT.md) before changing contracts. [Architecture](docs/ARCHITECTURE.md) describes state ownership; [documentation map](docs/README.md) identifies the owner of each written contract.

## Development setup

Install Go 1.25, Node.js (CI uses 22.x), Python 3, Git, Make, and Bash. The root CLI is CGo-free; race testing additionally requires `CGO_ENABLED=1` and a Go-supported C compiler on `PATH`. On Windows, use a supported toolchain such as MSYS2 MinGW.

```bash
git clone https://github.com/sean2077/pairroom.git
cd pairroom
make check
make smoke
```

`make check` includes formatting, static/unit/race/dependency checks, JavaScript regressions, desktop-source checks, and the documentation contract. `make js-check` runs the fast JavaScript layer and fails visibly when Node.js is missing. Dependency checks reject replacements and module/version drift. `make smoke` exercises deterministic Mock collaboration and recovery, not vendor models.

The Wails desktop is a separate module with native build dependencies. Use [Desktop development](desktop/README.md) and [Desktop Agent contract](desktop/AGENTS.md) for build, packaging, and local-update verification. Do not move GUI dependencies into the root module.

## Change workflow

Create a short-lived branch/worktree from current `main`, state the relevant invariants and failure boundaries, then change the minimum source and owning documentation. Add regression tests for actual state transitions. Land through a PR, not a direct push to `main`.

Concurrency/recovery changes should cover success, cancellation, process exit, restart, late events, duplicate callbacks, and unknown native-submission outcomes. Keep verification proportional to the change, but do not replace execution evidence with another Agent's assertion.

## Browser verification

```bash
python3 -m venv .browser-venv
.browser-venv/bin/python -m pip install -r scripts/requirements-browser.txt
.browser-venv/bin/python -m playwright install chromium
make browser-check PYTHON=.browser-venv/bin/python
```

On Windows use `.browser-venv/Scripts/python.exe`. Managed Linux may require Playwright's `install --with-deps chromium`; `PAIRROOM_BROWSER_EXECUTABLE` can select an installed Chromium. Results/screenshots go to `.browser-results/`.

| Layer | What it verifies | What it does not prove |
|---|---|---|
| JavaScript/client regressions | Parsing, state/freshness guards, duplicate actions, and deterministic client behavior | Browser layout, real network authentication, vendor execution |
| Real-asset in-page browser fixtures | Room/Management rendering, input/drafts, focus/scroll/disclosure, SSE response handling, locale/theme/layout, approvals and configuration interactions | Real Service auth/transport or model behavior |
| Production-CSP Management browser checks | External asset loading and naming/menu interactions under the actual CSP | Authenticated vendor execution |
| Real-browser Mock Service smoke | Actual loopback HTTP/SSE, bootstrap cookie/CSRF, gateway, Room lifecycle/settings/permissions and restart, with no mocked fetch/EventSource | Native CLI/model correctness |
| Authenticated native E2E | The selected real CLI/Provider/session/permission combination and task | Every vendor release or unrelated combination |

The real-browser Service layer is `scripts/test_service_browser.py`; it builds the CLI or accepts `--binary`, uses a disposable Git repository and isolated home/data root, and removes temporary credentials/state on exit. Its evidence goes to `.browser-results/service/`.

A browser that blocks loopback cannot run that layer. Report it unverified rather than relaxing the browser boundary or replacing HTTP with fixtures. `python3 scripts/test_management_browser.py --in-page-fixture` is a limited interaction fallback when navigation is blocked; its output explicitly does not certify CSP. `node scripts/test_management_client.js` covers client freshness/catalog/action guards without browser timing.

## Focused allocation checks

```bash
go test ./internal/room -run '^$' -bench 'Benchmark(TextDeltaSummaryProjection|RecentEventTail)$' -benchmem
go test ./internal/room -run '^$' -bench BenchmarkWindowedSnapshot -benchmem
```

Compare fixed workload/window sizes and allocations, not machine-dependent pass/fail timing thresholds. These isolated operation benchmarks do not establish whole-workflow latency, model accuracy, or token-billing savings.

## Documentation changes

```bash
make docs-check
```

The checker covers repository Markdown links/images, including root policy/support pages, nested docs, and newly added non-ignored files, plus the curated public-guide and source-derived CLI/API/config inventories. It does not test external links, all heading fragments, example commands, or the factual accuracy of competitor claims.

Use [docs/README.md](docs/README.md) for ownership. Keep current technical documents in English and the root English/Chinese READMEs equivalent. Avoid copying detailed semantics between README, architecture, operations, and troubleshooting. Do not hard-code the current release in the product entry points.

Facts have explicit owners: CLI source/`--help` for flags; production Service/Room registrations for routes; configuration/model structs and strict parser for fields; Store/model/apply code for schemas. Breaking changes update both [Changelog](CHANGELOG.md) and [Upgrading](docs/UPGRADING.md). Preserve historical release/validation records as history, not current support guarantees.

Why/Alternatives pages explain fit rather than specifying new behavior. Keep their review date, source revision, and distinction between source observation, inference, and measured results. Re-check primary sources when changing competitor claims; a `main` snapshot or open issue is not a published release certification. Plans and one-off audits belong in Issues/PRs until implemented.

Do not edit generated `CLAUDE.md`, skill, or subagent projections independently; follow their source/generator contract. Keep Markdown checks offline and dependency-light rather than adding a documentation framework for a small guide set.

## PR evidence

Include the problem, design boundaries, user-visible changes, migration/rollback impact, actual verification commands/results, and what was not verified. Distinguish local results from CI and both from authenticated native E2E. List an unavailable dependency/environment honestly instead of claiming a test passed or weakening the test.

Documentation-only changes need no fictional runtime migration or release bump. A docs-checker change does need its own regression tests and an actual run against the repository. Prefer reviewable commits; do not add temporary workflows whose only purpose is exporting source or shuffling patches.
