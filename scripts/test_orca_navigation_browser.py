"""Navigation regressions for the Orca-inspired workspace; no vendor sessions.

Uses the existing Management HTTP fixture under the production CSP. Called by
our shared workbench browser suite, so it runs in make browser-check and CI.
"""
from pathlib import Path
import json
import re
from playwright.async_api import expect
from test_management_browser import load_csp_fixture


async def verify_navigation(browser, artifacts: Path, *, fixture=None) -> None:
    artifacts.mkdir(parents=True, exist_ok=True)
    page = await browser.new_page(viewport={'width': 1440, 'height': 1000}, reduced_motion='reduce', locale='en-US')
    page.set_default_timeout(5000)
    errors = []
    page.on('pageerror', lambda error: errors.append(str(error)))
    try:
        if fixture:  # Development-only offline adapter; CI always uses HTTP/CSP.
            await page.set_content(fixture())
        else:
            await load_csp_fixture(page)
        await expect(page.locator('#app')).to_be_visible()
        separator = page.locator('.workbench-sidebar-resizer')
        launcher = page.locator('#management-command-button')
        await expect(separator).to_be_visible()
        await expect(page.locator('#sidebar #management-command-button')).to_be_visible()
        await page.evaluate("""() => {
          window.__sidebarNode = document.getElementById('sidebar');
          window.__commandNode = document.getElementById('management-command-button');
          window.__stageNode = document.getElementById('room-stage');
          window.__ruleCount = [...document.styleSheets].reduce((n,s)=>n+s.cssRules.length,0);
        }""")

        async def width(value):
            await expect(separator).to_have_attribute('aria-valuenow', str(value))
            actual = await page.locator('#sidebar').evaluate('el=>el.getBoundingClientRect().width')
            assert abs(actual - value) < 1, (value, actual)

        await width(244)
        await separator.focus()
        await page.keyboard.press('ArrowRight')
        await width(252)
        await page.keyboard.press('End')
        await page.keyboard.press('ArrowRight')
        await width(360)
        await page.keyboard.press('Home')
        await page.keyboard.press('ArrowLeft')
        await width(208)
        await separator.dblclick()
        await width(244)
        # One rule changes, not a new stylesheet per move or keyboard press.
        assert await page.evaluate('[...document.styleSheets].reduce((n,s)=>n+s.cssRules.length,0) === __ruleCount')

        box = await separator.bounding_box()
        start_x = box['x'] + box['width']/2
        await page.mouse.move(start_x, 280)
        await page.mouse.down()
        await page.mouse.move(start_x + 66, 280, steps=6)
        await page.mouse.up()
        await width(310)
        # Cancellation rolls back the live preview to its starting width.
        await page.mouse.down()
        await page.mouse.move(345, 280)
        await page.keyboard.press('Escape')
        await page.mouse.up()
        await width(310)
        await expect(page.locator('body')).not_to_have_class(re.compile(r'workbench-resizing'))

        await page.locator('.workbench-add-project').click()
        await expect(page.locator('#project-dialog')).to_be_visible()
        await page.keyboard.press('Escape')

        await launcher.click()
        await expect(page.locator('#management-command-dialog')).to_be_visible()
        await page.keyboard.press('Escape')
        await expect(launcher).to_be_focused()
        await page.locator('#sidebar-collapse').click()
        await expect(separator).not_to_be_visible()
        await expect(launcher).to_be_visible()
        await launcher.click()
        await page.keyboard.press('Escape')
        await expect(launcher).to_be_focused()
        await page.locator('#sidebar-collapse').click()
        await width(310)

        # A hidden desktop separator must not retain focus on mobile.
        await separator.focus()
        await page.set_viewport_size({'width': 390, 'height': 844})
        await expect(separator).not_to_be_visible()
        await expect(page.locator('#mobile-menu')).to_be_focused()
        await expect(page.locator('.topbar-actions #management-command-button')).to_be_visible()
        assert await launcher.evaluate('el=>el===__commandNode')
        await page.set_viewport_size({'width': 1440, 'height': 1000})
        await width(310)
        await expect(page.locator('#sidebar #management-command-button')).to_be_visible()
        await separator.dblclick()

        for theme, language in [('light', 'en'), ('dark', 'zh-CN')]:
            await page.evaluate("async v=>{PairRoomTheme.setTheme(v[0]);await PairRoomI18n.setLang(v[1]);}", [theme, language])
            await expect(separator).to_have_attribute('aria-label', 'Primary navigation' if language == 'en' else '主导航')
            palette = await page.evaluate("""() => [document.body, document.getElementById('sidebar')].map(el=>getComputedStyle(el).backgroundColor)""")
            assert palette[0] != palette[1], palette
            await page.evaluate("location.hash='#/projects'")
            await expect(page.locator('#page-title')).to_be_visible()
            await page.screenshot(path=str(artifacts / f'navigation-{theme}-{language}.png'))
            await page.evaluate("location.hash='#/settings'")
            await expect(page.locator('.settings-nav')).to_be_visible()
            await page.screenshot(path=str(artifacts / f'settings-{theme}-{language}.png'))
            await page.set_viewport_size({'width': 390, 'height': 844})
            assert await page.evaluate('document.documentElement.scrollWidth<=innerWidth'), 'horizontal overflow'
            await expect(page.locator('.topbar-actions #management-command-button')).to_be_visible()
            await page.screenshot(path=str(artifacts / f'mobile-{theme}-{language}.png'))
            await page.set_viewport_size({'width': 1440, 'height': 1000})

        assert await page.evaluate("""__sidebarNode===document.getElementById('sidebar') && __stageNode===document.getElementById('room-stage') && __commandNode===document.getElementById('management-command-button')""")
        if not fixture:
            # Resize beside a real owned Room iframe: navigation changes must
            # not dispose/reload the surface or its native conversation state.
            await page.locator('.tree-room[data-room-id]').first.click()
            await expect(page.locator('#room-stage iframe').first).to_be_visible()
            await page.evaluate("window.__frameNode=document.querySelector('#room-stage iframe');window.__frameSrc=__frameNode.src")
            await separator.focus()
            await page.keyboard.press('Shift+ArrowRight')
            await width(276)
            assert await page.evaluate("__frameNode===document.querySelector('#room-stage iframe') && __frameNode.src===__frameSrc && __frameNode.isConnected")
            assert await page.evaluate('__cspErrors.length') == 0, 'navigation violated production CSP'
            await page.reload()
            await expect(separator).to_have_attribute('aria-valuenow', '244')
        assert not errors, errors
        (artifacts / 'navigation-results.json').write_text(json.dumps({
            'keyboard_and_pointer_resize': True, 'cancel_restores_start_width': True,
            'one_cssom_rule': True, 'launcher_identity_and_focus': True,
            'collapsed_and_mobile_navigation': True, 'tab_local_width': True,
            'production_csp': fixture is None, 'page_errors': errors,
        }, indent=2) + '\n', encoding='utf-8')
    finally:
        await page.close()
