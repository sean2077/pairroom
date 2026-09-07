#!/usr/bin/env python3
"""Render native desktop Settings with production assets/CSP and an IPC fixture.

This validates visible UI behavior, not an actual OS login or Wails webview.
"""
from __future__ import annotations
import argparse
import asyncio
import os
from pathlib import Path
from playwright.async_api import async_playwright, expect
from test_management_browser import load_csp_fixture
from test_room_browser import ROOT


async def verify(executable: str | None, artifacts: Path) -> None:
    artifacts.mkdir(parents=True, exist_ok=True)
    async with async_playwright() as playwright:
        browser = await playwright.chromium.launch(executable_path=executable, args=['--no-sandbox'])
        page = await browser.new_page(viewport={'width': 1280, 'height': 800}, locale='en-US')
        errors = []
        page.on('pageerror', lambda error: errors.append(str(error)))
        await page.add_init_script('''
          window.__startupEnabled = false; window.__startupFailure = false; window.__startupWrites = 0;
          window.chrome = window.chrome || {};
          window.chrome.webview = {postMessage(message) {
            const request = JSON.parse(message);
            const reply = () => {
              if (request.action === 'set') {
                __startupWrites++;
                if (!__startupFailure) __startupEnabled = request.enabled;
              }
              window.PairRoomDesktop.receive({id: request.id, enabled: __startupEnabled,
                error: __startupFailure ? 'Fixture: system denied the change' : ''});
            };
            if (window.__startupHold) window.__startupReply = reply; else queueMicrotask(reply);
          }};
        ''')
        await load_csp_fixture(page)
        await page.goto('http://127.0.0.1:7332/?desktop=1#/settings')
        await page.get_by_role('button', name='Desktop', exact=True).click()
        toggle = page.get_by_role('switch', name='Launch at login', exact=True)
        await expect(toggle).to_be_enabled()
        await expect(toggle).to_have_attribute('aria-checked', 'false')
        assert await page.evaluate('__startupWrites') == 0, 'viewing settings implicitly enabled startup'
        await page.evaluate("__startupHold = true")
        await toggle.click()
        await expect(toggle).to_be_disabled()
        await page.evaluate("__startupHold = false; __startupReply()")
        await expect(toggle).to_have_attribute('aria-checked', 'true')
        await expect(toggle).to_be_enabled()
        await page.evaluate('__startupFailure = true')
        await toggle.click()
        await expect(page.get_by_role('alert').filter(has_text='Fixture: system denied')).to_be_visible()
        await expect(toggle).to_have_attribute('aria-checked', 'true')
        await page.evaluate('__startupFailure = false')
        await page.get_by_role('button', name='Retry now', exact=True).click()
        await expect(toggle).to_be_enabled()
        await toggle.click()
        await expect(toggle).to_have_attribute('aria-checked', 'false')
        await page.evaluate("PairRoomI18n.setLang('zh-CN')")
        await expect(page.get_by_role('switch', name='开机启动', exact=True)).to_be_visible()
        await page.screenshot(path=str(artifacts / 'desktop-settings-zh.png'))
        assert not errors, errors
        assert not await page.evaluate('__cspErrors'), 'desktop integration violates Management CSP'
        ordinary = await browser.new_page(locale='en-US')
        await load_csp_fixture(ordinary)
        await ordinary.goto('http://127.0.0.1:7332/?desktop=1#/settings')
        await expect(ordinary.locator('.settings-nav')).to_be_visible()
        assert await ordinary.get_by_role('button', name='Desktop', exact=True).count() == 0
        await browser.close()
        print('desktop Settings browser/CSP/locale/opt-in/failure-state contracts: ok (native IPC fixture)')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--browser', default=os.environ.get('PAIRROOM_BROWSER_EXECUTABLE'))
    parser.add_argument('--artifacts', type=Path, default=ROOT / '.browser-results' / 'desktop-settings')
    args = parser.parse_args()
    asyncio.run(verify(args.browser, args.artifacts))
