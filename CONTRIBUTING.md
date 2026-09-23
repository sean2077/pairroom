# Contributing to PairRoom

Read [AGENTS.md](AGENTS.md) and [project terminology](CONTEXT.md) before changing contracts. [Architecture](docs/ARCHITECTURE.md) describes state ownership; the [documentation map](docs/README.md) identifies each written contract's owner. Desktop has its own [nested Agent contract](desktop/AGENTS.md).

## Development setup

Install the latest stable Go release (minimum Go 1.27), Node.js (CI uses 22.x), Python 3, Git, Make, and Bash. The root CLI is CGo-free; race testing additionally requires `CGO_ENABLED=1` and a Go-supported C compiler on PATH. On Windows, use a supported toolchain such as MSYS2 MinGW.

```bash
git clone https://github.com/sean2077/pairroom.git
cd pairroom
make check
make smoke
```

`make check` includes formatting, static/unit/race/dependency checks, JavaScript regressions, desktop-source checks, and documentation/projection/release contracts. `make js-check` fails visibly if Node is missing. Dependency checks reject replacements and module/version drift. `make smoke` exercises deterministic Mock collaboration, media, backup, restore, and diagnostics, not vendor models. `make cover` is diagnostic coverage, not a percentage release gate.

`make install` installs to `GOBIN` (default `GOPATH/bin`), reports PATH visibility, and never edits PATH. `make dev` stops an installed daemon before running the current-tree Service. The Wails desktop is a separate module; keep GUI dependencies out of the root. Build/update and `DESKTOP_INSTALL_DIR` behavior belong in [Desktop development](desktop/README.md).

## Go version policy

CI and release builds select the latest stable Go release with `actions/setup-go` (`go-version: stable`, `check-latest: true`), not `go-version-file`. Both modules' `go` directives declare the minimum language/toolchain version and follow the latest stable major release; do not add a `toolchain` directive. Weekly Dependabot checks cover both modules. Review Go directive updates without changing the approved dependency closure: run `go mod tidy` separately in the root and `desktop/`, inspect both module locks, and run `go run scripts/check_dependencies.go`.

Local build/install/release commands preserve `GOTOOLCHAIN`. With `GOTOOLCHAIN=auto`, an older installation can download the module's required toolchain; this does not promise the newest available patch. Install the latest stable release before producing a local release. The release entry point prints the actual version and rejects a toolchain below the module minimum (including an outdated `GOTOOLCHAIN=local` installation). CI checks the actual toolchain after setup too.

`make vuln` checks reachable source vulnerabilities with the fixed standalone `govulncheck` version declared in `Makefile`. It needs network access and is deliberately separate from the offline `make check` gate. The CI `vulnerabilities` job runs on PRs and weekly even without commits, and blocks downstream CLI builds. Release publication additionally requires `make vuln-binary` on the built Linux CLI. These tool dependencies must not enter either application module. Repository required-check settings are managed separately from workflow files.

Every CLI artifact must report the same Go version as the release provenance's `go_version`; `scripts/verify-artifacts.sh` and CI check the actual binaries, not a hard-coded expected patch version. The release-contract regressions cover mixed toolchains and provenance mismatches. Reverting this build policy requires no Room-data migration. Version bumps, tags, and publication still require explicit authorization.

## Change workflow

Use a short-lived task branch/worktree from current `main`, respecting the assigned lifecycle owner. State the relevant invariant and failure boundary, then change the minimum source and owning documentation. Add regressions for actual state transitions. Submit a PR, not a direct push to `main`; PR handoff does not authorize merging or deleting the task worktree.

Concurrency/recovery changes should cover success, cancellation, process exit, restart, late events, duplicate callbacks, and unknown submission outcomes. Keep verification proportional, but never replace execution evidence with another Agent's assertion. The installed `agent-scaffold` skill's `verify --profile default --json` is the authoritative full harness check; do not hand-edit its runtime to bypass a failure.

## Browser verification

```bash
python3 -m venv .browser-venv
.browser-venv/bin/python -m pip install -r scripts/requirements-browser.txt
.browser-venv/bin/python -m playwright install chromium
make browser-check PYTHON=.browser-venv/bin/python
```

On Windows use `.browser-venv/Scripts/python.exe`. Managed Linux may require Playwright's `install --with-deps chromium`; `PAIRROOM_BROWSER_EXECUTABLE` selects an installed Chromium. Evidence goes to `.browser-results/`.

| Layer | What it verifies | What it does not prove |
|---|---|---|
| JavaScript regressions | Parsing, freshness guards, duplicate actions, deterministic client behavior | Browser layout, network authentication, vendor execution |
| Real-asset in-page fixtures | Rendering, drafts, focus/scroll/disclosure, SSE response handling, locale/theme/layout | Real Service auth/transport or model behavior |
| Production-CSP Management checks | External asset loading and interactions under the actual CSP | Authenticated vendor execution |
| Real-browser Mock Service smoke | Loopback HTTP/SSE, bootstrap cookie/CSRF, gateway, lifecycle/settings/permissions and restart without mocked fetch/EventSource | Native CLI/model correctness |
| Authenticated native E2E | The exercised CLI/Provider/session/permission combination and task | Every vendor version or unrelated configuration |

`scripts/test_service_browser.py` builds the CLI or accepts `--binary`, uses an isolated home/data root and disposable Git repository, and cleans temporary state. Evidence goes to `.browser-results/service/`.

If loopback is blocked, report that layer unverified rather than weakening authentication or substituting a fixture. `python3 scripts/test_management_browser.py --in-page-fixture` is an interaction fallback, not CSP certification. `node scripts/test_management_client.js` covers deterministic client guards without browser timing. Native layout fixtures likewise do not prove that a vendor model received an envelope.

## Focused allocation checks

```bash
go test ./internal/room -run '^$' -bench 'Benchmark(TextDeltaSummaryProjection|RecentEventTail)$' -benchmem
go test ./internal/room -run '^$' -bench BenchmarkWindowedSnapshot -benchmem
go test ./internal/relay -run '^$' -bench BenchmarkNativeLongRoom -benchmem
```

Compare fixed workloads and allocations, not machine-dependent pass/fail timing. Microbenchmarks do not establish whole-workflow latency, model accuracy, or billing savings. Dated benchmark tables remain historical measurements unless rerun with a stated baseline and environment.

## Documentation changes

```bash
make docs-check
```

The checker covers repository Markdown paths/images (including nested and newly added non-ignored files), the curated public-guide set, and source-derived CLI/API/config inventories. It does **not** verify external links, every heading fragment, example execution, or semantic accuracy. Review changed fragment links and commands separately; a passing inventory cannot detect a false statement such as applying Embedded Turn ownership to Native.

Keep one owner for each detailed contract and link to it from overview/recipes. Current technical documents are English; root English/Chinese READMEs must remain equivalent. Avoid release-number churn in those entry points. Scope process ownership, permissions, scheduling, identity, and recovery by host mode. Distinguish a desktop-owned embedded Service from an Embedded Room.

Use source/`--help` for flags, production registrations for routes, strict parsers/model structs for configuration, and Store/apply code for schemas. Preserve generated inventory markers and entries when only prose changes. Breaking changes update [Changelog](CHANGELOG.md) and [Upgrading](docs/UPGRADING.md); documentation corrections need neither fictional migrations nor release bumps.

Preserve published release/validation evidence as dated history. Completed plans must not keep instructing Agents to start implementation or purge data; retain rationale and a historical source link, then point to current contracts. New plans and one-off audits belong in Issues/PRs unless they add a durable design decision. A small flat status/date is sufficient where history and current design could be confused; do not introduce a documentation workflow framework.

Why/Alternatives explain fit, not new product behavior. Keep their source revisions, review dates, and distinction between observations, inference, and measurements. Recheck primary sources before changing external claims; a repository snapshot or issue is not release certification.

`CLAUDE.md` and project skill/subagent projections are not independent sources. Follow [AGENTS.md](AGENTS.md) for generators and the separate product relay-skill mirror. Keep checks offline and dependency-light.

## Release verification

`make bump-version NEW_VERSION=X.Y.Z` synchronizes root `VERSION`, `version.Current` in `internal/version/`, `desktop/build/config.yml`, and the canonical `CHANGELOG.md` release heading, moving Unreleased notes into the new section. It fails closed and does not commit or tag. The exact `vX.Y.Z` tag, binary version, and `## [vX.Y.Z] — YYYY-MM-DD` changelog heading must agree.

`make release` requires a clean tree and builds/verifies the complete local payload. It does not create a tag or publish a Release. CLI CI must retain uniquely named, checksummed Linux amd64, Windows amd64, macOS arm64, and macOS amd64 artifacts, then re-download and verify the complete set.

`.github/workflows/release.yml` owns publication: validate/extract changelog notes, build and verify CLI artifacts, publish the Release, then re-download/recheck its CLI payload. The desktop workflow attaches `pairroom-desktop-*` packages to the same Release on `v*` tags. Its Windows `inno` winget manifest submission to `microsoft/winget-pkgs` uses the configured `WINGET_TOKEN` (classic PAT with `public_repo`) through a fork PR, is idempotent per version, and must not rewrite the Release on failure. Desktop production signing/notarization may be claimed only after it actually runs in the release environment.

Preserve these checks when changing build/release tooling. A documentation-only PR does not need to invoke publication, create tags, alter secrets, or run paid vendor acceptance.

## PR evidence

Include the problem, user-visible changes, affected boundaries, migration/rollback impact, commands/results, and what was not verified. Separate local results from CI and both from authenticated native E2E. List missing dependencies or restricted environments honestly; do not mark checks passed based on inspection or a partial reconstruction.

A docs-checker change needs its own regressions and an actual repository run. Prefer reviewable commits. Do not add temporary workflows merely to export source or shuffle patches, weaken checks to pass a restricted environment, or publish private transcripts/credentials as evidence.
