# PairRoom website

The bilingual static landing page is published with GitHub Pages. Product facts remain owned by the root README and the [documentation map](../docs/README.md); update this page alongside relevant product changes. The English HTML remains readable without JavaScript. Chinese, platform and screenshot switching, and clipboard feedback require JavaScript; `?lang=en` and `?lang=zh-CN` select the language.

The page leads with the existing-session use case and real product screenshots, then covers review examples, host-mode selection, installation, and practical questions. Keep copy concrete and the two languages equivalent. Use the existing paper/ink palette, system fonts, and simple ruled sections; the Chinese headings use upright sans-serif type. Screenshots stay uncropped and explicitly identify their synthetic demo conversations. Native's experimental status and runtime-specific limits belong beside the relevant choices. The optional Mock walkthrough uses a native disclosure so installation stays easy to scan.

## Local development and verification

From the repository root, run `python scripts/check_website.py`, then `python scripts/build_website.py --output dist/website`. The output directory must be new; the packager never deletes an existing directory. It includes only the four public website files, `.nojekyll`, the desktop application's `desktop/assets/icon.png` copied byte-identically as `favicon.png`, and six screenshots copied from their canonical `docs/images/` sources. The same desktop icon appears in the header, footer, and browser tab. Edit images at their source instead of keeping duplicate website copies.

Preview the packaged page with `python -m http.server 8000 --bind 127.0.0.1 --directory dist`, then open `http://127.0.0.1:8000/website/`. Resource references are relative so the same files work under the deployed `/pairroom/` path.

For browser checks, use the [existing browser environment](../CONTRIBUTING.md#browser-verification), then run `.browser-venv/bin/python scripts/test_website_browser.py` (Windows: `.browser-venv/Scripts/python.exe`). `--browser` or `PAIRROOM_BROWSER_EXECUTABLE` selects an installed Chromium. The test packages and serves the actual assets under `/pairroom/`, exercises both languages, installation/screenshot controls, and the Mock disclosure, verifies actual clipboard contents and failure feedback, checks responsive and 200% text layouts, and saves evidence in `.browser-results/website/`. This is website evidence, not Service or vendor E2E.

## Publishing

The repository Pages source must be **GitHub Actions**. `.github/workflows/pages.yml` validates relevant PRs without publishing. Relevant pushes to `main`, or a manual run on `main`, validate, package, and deploy only the public website. Manual runs on other branches validate without publishing. Only the deployment job receives `pages: write` and `id-token: write`; actions are pinned to upstream release commits. Future action updates are owned by the existing Dependabot configuration.

The expected default URL is `https://sean2077.github.io/pairroom/`; report it as live only after a successful deployment and public verification. No custom domain, analytics, model API, or automatic content refresh is configured. Releases and documentation links resolve to the GitHub project; no credentials or Service URLs are part of the public artifact. Follow the repository's PR and merge policy before publishing changes from `main`.
