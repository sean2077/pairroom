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
          window.__updates = {enabled: false, offline: false, checked: ''};
          window.chrome = window.chrome || {};
          window.chrome.webview = {postMessage(message) {
            const request = JSON.parse(message);
            if (request.kind === 'pairroom.desktop.browser') {
              (window.__browserLinks ||= []).push(request.url);
              return;
            }
            if (request.kind === 'pairroom.desktop.updates') {
              (window.__updateRequests ||= []).push(request.action);
              if (request.action === 'set') __updates.enabled = request.enabled;
              const response = {id: request.id, enabled: __updates.enabled, current: '5.6.0'};
              if (request.action === 'check' && __updates.offline) response.error = 'Fixture: GitHub unreachable';
              else if (request.action === 'check') __updates.checked = '2026-09-27T12:00:00Z';
              if (__updates.enabled && __updates.checked) {
                response.checked_at = __updates.checked;
                if (!__updates.offline) Object.assign(response, {latest: '5.7.0', url: 'https://github.com/sean2077/pairroom/releases/tag/v5.7.0'});
              }
              queueMicrotask(() => window.PairRoomDesktop.receive(response));
              return;
            }
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
        bridge = (ROOT / 'desktop/main.go').read_text(encoding='utf-8').split('const desktopWindowBridge = `', 1)[1].split('`', 1)[0]
        await page.evaluate(bridge)
        await page.evaluate(bridge)  # reinjection must not open links twice
        await page.evaluate('''() => {
          const link = document.createElement('a');
          link.id = 'browser-test-link'; link.href = 'https://example.com/path?q=test';
          Object.assign(link.style, {position: 'fixed', top: '0', left: '0', zIndex: '99999'});
          link.innerHTML = '<span>External link</span>'; document.body.prepend(link);
        }''')
        await page.locator('#browser-test-link span').click(modifiers=['Control'])
        await page.locator('#browser-test-link span').click(modifiers=['Meta'])
        assert await page.evaluate('__browserLinks') == ['https://example.com/path?q=test'] * 2
        await page.evaluate('''() => {
          const frame = document.createElement('iframe'); frame.id = 'browser-test-frame';
          Object.assign(frame.style, {position: 'fixed', top: '40px', left: '0', zIndex: '99999'});
          frame.srcdoc = '<a href="https://example.com/room"><b>Room link</b></a>';
          document.body.prepend(frame);
        }''')
        await page.frame_locator('#browser-test-frame').locator('b').click(modifiers=['Control'])
        assert await page.evaluate('__browserLinks.at(-1)') == 'https://example.com/room'
        await page.evaluate('''() => {
          const link = document.querySelector('#browser-test-link');
          link.addEventListener('click', event => event.preventDefault());
          link.dispatchEvent(new MouseEvent('click', {ctrlKey: true, bubbles: true, cancelable: true}));
          document.querySelector('#browser-test-frame').remove();
          document.querySelector('#browser-test-link').remove();
        }''')
        assert await page.evaluate('__browserLinks.length') == 3, 'synthetic clicks must not launch the browser'
        await page.get_by_role('button', name='Desktop', exact=True).click()
        toggle = page.get_by_role('switch', name='Launch at login', exact=True)
        await expect(toggle).to_be_enabled()
        await expect(toggle).to_have_attribute('aria-checked', 'false')
        assert await page.evaluate('__startupWrites') == 0, 'viewing settings implicitly enabled startup'
        updates = page.get_by_role('switch', name='Check for updates', exact=True)
        await expect(updates).to_be_enabled()
        await expect(updates).to_have_attribute('aria-checked', 'false')
        await expect(page.get_by_role('button', name='Check now', exact=True)).to_have_count(0)
        assert await page.evaluate('__updateRequests') == ['get'], 'viewing settings must not check or enable updates'
        await updates.click()
        await expect(updates).to_have_attribute('aria-checked', 'true')
        await page.evaluate('__updates.offline = true')
        await page.get_by_role('button', name='Check now', exact=True).click()
        await expect(page.get_by_role('alert').filter(has_text='Fixture: GitHub unreachable')).to_be_visible()
        await page.evaluate('__updates.offline = false')
        await page.get_by_role('button', name='Check now', exact=True).click()
        await expect(page.get_by_text('PairRoom 5.7.0 is available')).to_be_visible()
        await expect(page.get_by_role('alert')).to_have_count(0)
        await page.get_by_role('button', name='Open release page', exact=True).click()
        assert await page.evaluate('__browserLinks.at(-1)') == 'https://github.com/sean2077/pairroom/releases/tag/v5.7.0'
        assert await page.evaluate('__updateRequests') == ['get', 'set', 'check', 'check']
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
        await expect(page.get_by_text('PairRoom 5.7.0 已发布')).to_be_visible()
        await page.screenshot(path=str(artifacts / 'desktop-settings-zh.png'))
        await page.get_by_role('switch', name='检查更新', exact=True).click()
        await expect(page.get_by_text('PairRoom 5.7.0 已发布')).to_have_count(0)
        assert not errors, errors
        assert not await page.evaluate('__cspErrors'), 'desktop integration violates Management CSP'
        ordinary = await browser.new_page(locale='en-US')
        await load_csp_fixture(ordinary)
        await ordinary.goto('http://127.0.0.1:7332/?desktop=1#/settings')
        await expect(ordinary.locator('.settings-nav')).to_be_visible()
        assert await ordinary.get_by_role('button', name='Desktop', exact=True).count() == 0
        await browser.close()
        print('desktop Settings browser/CSP/locale/opt-in/failure-state/update-check contracts: ok (native IPC fixture)')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--browser', default=os.environ.get('PAIRROOM_BROWSER_EXECUTABLE'))
    parser.add_argument('--artifacts', type=Path, default=ROOT / '.browser-results' / 'desktop-settings')
    args = parser.parse_args()
    asyncio.run(verify(args.browser, args.artifacts))
