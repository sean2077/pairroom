#!/usr/bin/env python3
"""Workbench presentation regressions using the existing deterministic UI fixtures.

Checks rendered contrast, focus, icon updates and responsive controls, not vendor
E2E. The existing Service/Native suites exercise production CSP and real HTTP/SSE.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
from pathlib import Path

from playwright.async_api import async_playwright, expect
from test_management_browser import fixture_html as management_html
from test_room_browser import ROOT, fixture_html as room_html
from test_orca_navigation_browser import verify_navigation


async def contrast(locator) -> float:
    return await locator.evaluate("""el => {
      const css = getComputedStyle(el);
      const luminance = color => {
        const c = color.match(/[\\d.]+/g).slice(0, 3).map(v => Number(v) / 255)
          .map(v => v <= .04045 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4);
        return c[0] * .2126 + c[1] * .7152 + c[2] * .0722;
      };
      const values = [luminance(css.color), luminance(css.backgroundColor)].sort((a,b) => b-a);
      return (values[0] + .05) / (values[1] + .05);
    }""")


async def fits(page, selector: str) -> None:
    geometry = await page.locator(selector).evaluate("""el => {
      const r = el.getBoundingClientRect();
      return {left:r.left, right:r.right, width:innerWidth,
        scroll:document.documentElement.scrollWidth, client:document.documentElement.clientWidth};
    }""")
    assert geometry['left'] >= -1 and geometry['right'] <= geometry['width'] + 1, (selector, geometry)
    assert geometry['scroll'] <= geometry['client'] + 1, geometry


async def verify(browser_path: str | None, artifacts: Path) -> None:
    artifacts.mkdir(parents=True, exist_ok=True)
    results = []
    async with async_playwright() as p:
        options = {'headless': True}
        if browser_path:
            options['executable_path'] = browser_path
        browser = await p.chromium.launch(**options)
        try:
            for surface, html in [('management', management_html), ('room', room_html)]:
                page = await browser.new_page(viewport={'width':1440,'height':1000}, locale='en-US', reduced_motion='reduce')
                page.set_default_timeout(5000)
                errors = []
                page.on('pageerror', lambda error: errors.append(str(error)))
                if surface == 'management':
                    # A fragment is required for History on an about:blank fixture.
                    await page.evaluate("location.hash='#/projects/p1'")
                await page.set_content(html())
                await expect(page.locator('#app' if surface == 'management' else '#connection.connected')).to_be_visible()
                if surface == 'management':
                    await expect(page.locator('.primary-nav .workbench-icon')).to_have_count(4)
                    field = page.locator('#global-search')
                else:
                    field = page.locator('#message-input')
                    await field.fill('Keep this draft while changing appearance.')
                    await page.evaluate("window.__draftNode = document.getElementById('message-input')")
                for theme, language in [('light','en'), ('dark','zh-CN')]:
                    await page.evaluate("args => {PairRoomTheme.setTheme(args[0]); PairRoomI18n.setLang(args[1]);}", [theme, language])
                    await expect(page.locator('html')).to_have_attribute('data-theme', theme)
                    await page.wait_for_timeout(100)
                    expected = 'rgb(255, 255, 255)' if theme == 'light' else 'rgb(14, 14, 14)'
                    assert await page.locator('body').evaluate('el=>getComputedStyle(el).backgroundColor') == expected
                    assert await page.locator('body').evaluate('el=>getComputedStyle(el).backgroundImage') == 'none'
                    primary = page.locator('#add-project-button' if surface == 'management' else '#send-button')
                    ratio = await contrast(primary)
                    assert ratio >= 4.5, (surface, theme, ratio)
                    if surface == 'room':
                        await expect(field).to_have_value('Keep this draft while changing appearance.')
                        assert await field.evaluate('el=>el===window.__draftNode'), 'appearance replaced the composer'
                    await page.screenshot(path=str(artifacts / f'{surface}-{theme}-{language}.png'))
                    results.append({'surface':surface, 'theme':theme, 'primary_contrast':round(ratio,2)})

                # Renderer text updates repaint only the existing chrome button.
                theme_button = page.locator('[data-theme-cycle]').first if surface == 'management' else page.locator('#theme-button')
                await theme_button.focus()
                await theme_button.evaluate("""el => {
                  window.__themeNode=el; window.__iconMutations=0;
                  new MutationObserver(records=>window.__iconMutations+=records.length).observe(el,{childList:true});
                  el.textContent='temporary theme glyph';
                }""")
                await expect(theme_button.locator('[data-workbench-icon="moon"]')).to_have_count(1)
                await page.wait_for_timeout(100)
                assert await theme_button.evaluate('el=>el===window.__themeNode && document.activeElement===el')
                assert await page.evaluate('window.__iconMutations') <= 3, 'icon observer did not settle'

                if surface == 'management':
                    await page.evaluate("location.hash='#/settings'")
                    await expect(page.locator('.settings-nav')).to_be_visible()
                    sizes = await page.locator('.settings-nav button').evaluate_all(
                        'nodes=>nodes.map(el=>[parseFloat(getComputedStyle(el).fontSize), el.getBoundingClientRect().height])')
                    assert sizes and all(font >= 14 and height >= 44 for font, height in sizes), sizes
                    await page.screenshot(path=str(artifacts / 'settings-dark-zh-CN.png'))
                else:
                    # Focused controls move, rather than clone, at the breakpoint.
                    await page.locator('#refresh-button').focus()
                    await page.evaluate("window.__refreshNode=document.activeElement")
                await page.set_viewport_size({'width':390,'height':844})
                await fits(page, '.workspace-shell' if surface == 'management' else '.topbar')
                if surface == 'room':
                    await expect(page.locator('#ux-layout-button')).to_be_focused()
                    await page.locator('#ux-layout-button').click()
                    await expect(page.locator('.workbench-room-utilities #refresh-button')).to_be_visible()
                    assert await page.locator('#refresh-button').evaluate('el=>el===window.__refreshNode')
                    await page.screenshot(path=str(artifacts / 'room-mobile-menu-dark-zh-CN.png'))
                    await page.keyboard.press('Escape')
                    await expect(page.locator('#ux-layout-button')).to_be_focused()
                    await page.set_viewport_size({'width':1440,'height':1000})
                    await expect(page.locator('.topbar-actions > #refresh-button')).to_be_visible()
                    assert await page.locator('#refresh-button').evaluate('el=>el===window.__refreshNode')
                    await expect(field).to_have_value('Keep this draft while changing appearance.')
                else:
                    await page.screenshot(path=str(artifacts / 'settings-mobile-dark-zh-CN.png'))
                await page.emulate_media(forced_colors='active')
                control = page.locator('#add-project-button' if surface == 'management' else '#ux-layout-button')
                await control.focus()
                assert await control.evaluate('el=>getComputedStyle(el).outlineStyle') != 'none'
                assert not errors, errors
                await page.close()
            await verify_navigation(browser, artifacts / 'orca-navigation')
        finally:
            await browser.close()
    (artifacts / 'results.json').write_text(json.dumps(results, indent=2) + '\n', encoding='utf-8')
    print('Workbench rendered contrast, themes, chrome identity, focus and responsive controls passed.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--browser', default=os.environ.get('PAIRROOM_BROWSER'))
    parser.add_argument('--artifacts', type=Path, default=ROOT / '.browser-results' / 'workbench')
    args = parser.parse_args()
    asyncio.run(verify(args.browser, args.artifacts))
