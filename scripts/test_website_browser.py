#!/usr/bin/env python3
"""Exercise the real static site under the /pairroom/ Pages path; no Service or models."""
import argparse
import functools
import http.server
import os
import tempfile
import threading
from pathlib import Path

from playwright.sync_api import sync_playwright
from build_website import build


class QuietHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *_):
        pass


def test(browser_path, output):
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="pairroom-website-browser-") as directory:
        build(Path(directory) / "pairroom")
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(QuietHandler, directory=directory))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        origin = f"http://127.0.0.1:{server.server_port}"
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(headless=True, **({"executable_path": browser_path} if browser_path else {}))
                context = browser.new_context(viewport={"width": 1440, "height": 1000}, reduced_motion="reduce")
                context.grant_permissions(["clipboard-read", "clipboard-write"], origin=origin)
                page = context.new_page()
                errors = []
                page.on("pageerror", lambda error: errors.append(str(error)))
                page.on("response", lambda response: errors.append(f"HTTP {response.status}: {response.url}") if response.status >= 400 else None)
                page.goto(origin + "/pairroom/")
                page.locator("h1").filter(has_text="Two agents.").wait_for()
                page.keyboard.press("Tab")
                assert page.locator('.skip-link').evaluate("element => element === document.activeElement")
                page.locator('[data-platform="linux"]').focus()
                page.keyboard.press("Space")
                assert page.locator('[data-install="linux"]').is_visible()
                page.locator('[data-platform="windows"]').click()
                page.locator('[data-copy="windows-command"]').click()
                page.get_by_role("status").filter(has_text="Command copied.").wait_for()
                assert page.evaluate("navigator.clipboard.readText()") == "winget install PairRoom"
                for platform in ("macos", "linux", "windows"):
                    page.locator(f'[data-platform="{platform}"]').click()
                    assert page.locator(f'[data-install="{platform}"]').is_visible()
                    assert page.locator('[data-install]:visible').count() == 1
                for view in ("embedded", "management", "native"):
                    page.locator(f'[data-view="{view}"]').click()
                    image = page.locator(f'[data-shot="{view}"] img')
                    page.wait_for_function("img => img.complete && img.naturalWidth > 0", arg=image.element_handle())
                    assert page.locator('[data-shot]:visible').count() == 1
                page.locator('#language').click()
                page.wait_for_function("document.documentElement.lang === 'zh-CN'")
                assert "lang=zh-CN" in page.url and "两位 Agent" in page.title()
                assert "开始使用" in page.locator('.hero .primary').inner_text()
                for view in ("embedded", "management", "native"):
                    page.locator(f'[data-view="{view}"]').click()
                    image = page.locator(f'[data-shot="{view}"] img')
                    page.wait_for_function("img => img.complete && img.naturalWidth > 0", arg=image.element_handle())
                    assert "-zh.png" in image.get_attribute("src")
                page.reload()
                page.wait_for_function("document.documentElement.lang === 'zh-CN'")
                page.locator('#language').click()
                page.wait_for_function("document.documentElement.lang === 'en'")
                page.locator('.faq-list summary').first.click()
                assert page.locator('.faq-list details').first.get_attribute('open') is not None
                # A denied clipboard must never announce success, and must select the actual command.
                page.evaluate("Object.defineProperty(navigator, 'clipboard', {configurable:true, value:{writeText:async()=>{throw new Error('Denied')}}})")
                page.locator('[data-copy="windows-command"]').click()
                page.get_by_role("status").filter(has_text="Could not copy.").wait_for()
                assert page.evaluate("getSelection().toString()") == "winget install PairRoom"
                for lang in ("en", "zh-CN"):
                    page.goto(origin + f"/pairroom/?lang={lang}")
                    page.wait_for_function("lang => document.documentElement.lang === lang", arg=lang)
                    for width in (1440, 768, 390, 320):
                        page.set_viewport_size({"width": width, "height": 1000})
                        assert page.evaluate("document.documentElement.scrollWidth <= innerWidth"), f"Overflow: {lang}/{width}"
                    page.set_viewport_size({"width": 1440, "height": 1000})
                    page.screenshot(path=str(output / f"hero-{lang}.png"))
                    page.screenshot(path=str(output / f"desktop-{lang}.png"), full_page=True)
                    page.set_viewport_size({"width": 390, "height": 844})
                    page.screenshot(path=str(output / f"mobile-{lang}.png"), full_page=True)
                    page.evaluate("document.documentElement.style.fontSize = '200%'")
                    overflow = page.evaluate("[...document.querySelectorAll('body *')].filter(e => e.getBoundingClientRect().right > innerWidth + 1).map(e => e.tagName + '.' + e.className).slice(0,20)")
                    assert page.evaluate("document.documentElement.scrollWidth <= innerWidth"), f"200% text overflow: {lang}: {overflow}"
                # Back/forward query navigation must update the language without reloading.
                page.goto(origin + "/pairroom/?lang=en")
                page.evaluate("history.pushState(null, '', '?lang=zh-CN'); dispatchEvent(new PopStateEvent('popstate'))")
                page.wait_for_function("document.documentElement.lang === 'zh-CN'")
                page.go_back()
                page.wait_for_function("document.documentElement.lang === 'en'")
                assert not errors, errors
                # Translation failure is recoverable and must retain the working English page.
                failure = context.new_page()
                failure.route('**/zh-CN.json', lambda route: route.abort())
                failure.goto(origin + '/pairroom/?lang=zh-CN')
                failure.get_by_role('status').filter(has_text='Chinese could not load').wait_for()
                assert failure.locator('html').get_attribute('lang') == 'en'
                failure.unroute('**/zh-CN.json')
                failure.locator('#language').click()
                failure.wait_for_function("document.documentElement.lang === 'zh-CN'")
                # Core English content and links remain usable without JavaScript.
                no_js = browser.new_context(java_script_enabled=False)
                plain = no_js.new_page()
                plain.goto(origin + '/pairroom/')
                assert plain.locator('h1').is_visible() and plain.locator('noscript').is_visible()
                context.close()
                no_js.close()
                browser.close()
        finally:
            server.shutdown()
            server.server_close()
            thread.join()
    print("Website browser checks passed: both languages, Pages subpath, platform/screenshots, clipboard success/failure, responsive layout, 200% text, history, translation recovery, no-JS.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--browser', default=os.environ.get('PAIRROOM_BROWSER_EXECUTABLE'))
    parser.add_argument('--output', type=Path, default=Path('.browser-results/website'))
    args = parser.parse_args()
    test(args.browser, args.output)
