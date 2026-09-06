#!/usr/bin/env python3
"""Real Management assets with deterministic HTTP and inert Room-frame fixtures.

The test covers tab identity, cross-surface ownership, external archive updates,
configuration edits, and responsive presentation; it does not launch vendors.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import re
from pathlib import Path

from playwright.async_api import async_playwright

from test_room_browser import ROOT


def fixture_html() -> str:
    html = (ROOT / 'internal/service/assets/index.html').read_text(encoding='utf-8')

    def asset(src: str) -> str:
        directory = 'internal/webui/assets' if '/_pairroom/' in src else 'internal/service/assets'
        return (ROOT / directory / src.rsplit('/', 1)[-1]).read_text(encoding='utf-8')

    html = re.sub(r'<link\b[^>]*rel="icon"[^>]*>', '', html)
    html = re.sub(r'<link\b[^>]*rel="stylesheet"[^>]*href="([^"]+)"[^>]*>', lambda m: '<style>' + asset(m[1]) + '</style>', html)
    html = re.sub(r'<script\b[^>]*src="([^"]+)"[^>]*></script>', lambda m: '<script>' + asset(m[1]).replace('</script>', '<\\/script>') + '</script>', html)
    mock = r'''
      window.__snapshot = {
        version:'2.1.0',store_schema:9,data_root:'/state',healthy:true,
        generated_at:'2026-09-06T00:00:00Z',
        projects:[{id:'p1',root:'/workspace/example',available:true}],
        rooms:['r1','r2'].map((id,i)=>({id,project_id:'p1',name:i?'Review workspace':'Implementation workspace',
          lifecycle:'active',bindings:{claude:{agent:'claude',mode:'new',pending:true},codex:{agent:'codex',mode:'new',pending:true}},
          transcript_boundary_notice:'Earlier native history remains in its native harness.'})),
        runtimes:['r1','r2'].map(room_id=>({room_id,phase:'active',busy:false,occupies_capacity:true})),
        runtime_policy:{limit:4,idle_timeout_seconds:900},
        capabilities:{room_surface:true,room_deletion:true,project_refresh:true,project_removal:true,runtime_suspend:true}
      };
      window.__catalog = {profiles:[{name:'Example Provider',runtime:'claude',supported:true,provider:{source:'cc-switch',app_type:'claude',profile_id:'fixture'}}],runtimes:['claude','codex','grok'].map(runtime=>({runtime,
        display_name:{claude:'Claude Code',codex:'Codex',grok:'Grok Build'}[runtime],available:true})),
        defaults:{claude:{runtime:'claude',provider:{source:'native'}},codex:{runtime:'codex',provider:{source:'native'}}}};
      window.__serviceReads = 0; window.__readDelay = 0; window.__catalogDelay = 0;
      window.__catalogReads = 0; window.__catalogWrites = 0; window.__writes = [];
      window.fetch = async (path,options={}) => {
        let body = {};
        if (path === '/api/v1/session') body = {csrf_token:'fixture'};
        else if (path === '/api/v1/service') {
          __serviceReads++; body=structuredClone(__snapshot);
          await new Promise(resolve=>setTimeout(resolve,__readDelay));
        } else if (path.startsWith('/api/v1/agent-catalog')) {
          if (options.method==='POST') __catalogWrites++; else __catalogReads++;
          body=structuredClone(__catalog);
          await new Promise(resolve=>setTimeout(resolve,__catalogDelay));
        } else if (path.endsWith('/activate')) body={phase:'active'};
        else if (options.method && options.method!=='GET') __writes.push({path,body:options.body});
        return new Response(JSON.stringify(body),{status:200,headers:{'content-type':'application/json'}});
      };
    '''
    return html.replace('<head>', '<head><script>' + mock + '</script>', 1)


async def verify(browser_path: str | None, artifacts: Path) -> None:
    artifacts.mkdir(parents=True, exist_ok=True)
    results = {}
    async with async_playwright() as playwright:
        browser = await playwright.chromium.launch(headless=True, **({'executable_path': browser_path} if browser_path else {}))
        context = await browser.new_context(viewport={'width': 1440, 'height': 1000}, locale='en-US', reduced_motion='reduce')

        page = await context.new_page()
        page.set_default_timeout(5000)
        errors = []
        page.on('pageerror', lambda error: errors.append(str(error)))
        # No network navigation or real credentials. about:blank permits hash
        # routing while Room frame elements remain inert fixture identities.
        await page.evaluate("location.hash='#/overview'")
        await page.set_content(fixture_html())
        await page.wait_for_selector('#app:not([hidden]) .tree-room')
        await page.screenshot(path=str(artifacts / 'management-overview-light.png'))
        await page.locator('.tree-room', has_text='Implementation workspace').click()
        await page.wait_for_selector('#room-tablist .room-tab-target')
        await page.locator('.tree-room', has_text='Review workspace').click()
        await page.wait_for_selector('#room-stage [data-room-id="r2"] iframe')
        await page.wait_for_timeout(150)
        first = page.locator('#room-tablist [data-room-id="r1"] .room-tab-target')
        await first.focus()
        await page.evaluate('window.__focusedTab=document.activeElement')
        # Update the real shell through a legitimate iframe message. Focused tab
        # nodes must survive both metadata updates and a full polling refresh.
        await page.evaluate("""() => {
          const frame=document.querySelector('#room-stage [data-room-id="r2"] iframe');
          window.dispatchEvent(new MessageEvent('message',{origin:location.origin,source:frame.contentWindow,
            data:{type:'pairroom-surface',roomId:'r2',unread:3,pendingApprovals:0,error:''}}));
        }""")
        await page.wait_for_timeout(100)
        assert await page.evaluate('document.activeElement===__focusedTab && __focusedTab.isConnected'), 'unread update replaced the focused tab'
        await page.evaluate("document.getElementById('refresh-button').click()")
        await page.wait_for_timeout(120)
        assert await page.evaluate('document.activeElement===__focusedTab && __focusedTab.isConnected'), 'poll replaced the focused tab'
        # Existing UX keyboard ownership remains intact after incremental updates.
        await first.press('ArrowRight')
        await page.wait_for_function("document.activeElement.closest('.room-tab')?.dataset.roomId==='r2'")
        await page.evaluate("""() => {
          const frame=document.querySelector('#room-stage [data-room-id="r1"] iframe');
          window.dispatchEvent(new MessageEvent('message',{origin:location.origin,source:frame.contentWindow,
            data:{type:'pairroom-surface',action:'close-tab',roomId:'r2'}}));
        }""")
        assert await page.locator('#room-tablist .room-tab').count() == 2, 'a surface closed another Room tab'
        await page.evaluate("""() => {
          const frame=document.querySelector('#room-stage [data-room-id="r1"] iframe');
          window.dispatchEvent(new MessageEvent('message',{origin:location.origin,source:frame.contentWindow,
            data:{type:'pairroom-surface',action:'close-tab',roomId:'r1'}}));
        }""")
        await page.wait_for_function("document.querySelectorAll('#room-tablist .room-tab').length===1")
        await page.locator('.tree-room', has_text='Implementation workspace').click()
        await page.wait_for_selector('#room-stage [data-room-id="r1"] iframe')
        # External archive of a background Room must evict its tab and iframe.
        await page.evaluate("__snapshot.rooms.find(r=>r.id==='r2').lifecycle='archived'")
        await page.evaluate("document.getElementById('refresh-button').click()")
        await page.wait_for_function("!document.querySelector('#room-tablist [data-room-id=\"r2\"]')")
        assert await page.locator('#room-stage [data-room-id="r2"]').count() == 0
        # Removed active Room links must escape to overview rather than leave a
        # broken Reactivate button whose room.id access would throw.
        await page.evaluate("__snapshot.rooms=__snapshot.rooms.filter(r=>r.id!=='r1'); document.getElementById('refresh-button').click()")
        await page.wait_for_function("location.hash==='#/overview'")
        assert await page.locator('#room-tablist .room-tab').count() == 0
        results.update(stable_tab_focus=True, surface_identity_checked=True, stale_room_surfaces_removed=True)
        # Catalog refreshes must retain edits made while the request was pending.
        await page.evaluate("location.hash='#/projects/p1'")
        await page.wait_for_timeout(100)
        await page.get_by_role('button', name='+ Create Room', exact=True).first.click()
        await page.wait_for_function("!document.getElementById('room-submit').disabled")
        await page.locator('#claude-model').fill('model-before')
        await page.evaluate('__catalogDelay=250')
        await page.locator('#agent-catalog-refresh').click()
        await page.locator('#claude-model').fill('model-edited-while-refreshing')
        await page.wait_for_function("document.getElementById('agent-catalog-refresh').getAttribute('aria-busy')==='false'")
        assert await page.locator('#claude-model').input_value() == 'model-edited-while-refreshing', 'catalog refresh overwrote a new model override'
        results['catalog_edit_preserved'] = True
        provider = page.locator('#claude-provider')
        choice = await provider.locator('option').nth(1).get_attribute('value')
        await provider.select_option(choice)
        await page.evaluate('__catalog.profiles=[]')
        await page.locator('#agent-catalog-refresh').click()
        await page.wait_for_function("document.getElementById('agent-catalog-refresh').getAttribute('aria-busy')==='false'")
        assert await provider.input_value() == choice, 'deleted Provider silently fell back to native'
        assert not await provider.evaluate('node=>node.checkValidity()'), 'unavailable Provider was not blocked'
        await page.locator('#room-name').fill('Do not silently switch providers')
        writes = await page.evaluate('__writes.length')
        await page.locator('#room-submit').click()
        assert await page.evaluate('__writes.length') == writes, 'invalid provider selection reached room creation'
        results['unavailable_provider_requires_explicit_choice'] = True
        await provider.select_option(label='Native / Runtime default')
        await page.screenshot(path=str(artifacts / 'management-room-config-light.png'))
        await page.locator('#room-dialog [data-close-dialog="room-dialog"]').first.click()
        await page.get_by_role('button', name='Show archived', exact=True).click()
        await page.get_by_role('button', name='Permanently delete', exact=True).click()
        confirm_title = await page.locator('#confirm-title').text_content()
        assert 'Review workspace' in confirm_title
        confirm_ack = await page.locator('#confirm-ack-label').text_content()
        await page.locator('#confirm-ack').check()
        await page.evaluate("PairRoomI18n.setLang('zh-CN')")
        await page.wait_for_timeout(120)
        assert await page.locator('#confirm-title').text_content() == confirm_title, 'locale reset a destructive-operation title to generic startup text'
        assert await page.locator('#confirm-ack-label').text_content() == confirm_ack
        assert await page.locator('#confirm-ack').is_checked()
        await page.locator('#confirm-dialog [data-close-dialog="confirm-dialog"]').first.click()
        results['operation_identity_survives_locale'] = True
        # Dismiss tested transient notifications through their actual controls
        # so responsive evidence shows the page rather than earlier test toasts.
        await page.evaluate("document.querySelectorAll('#toasts .toast-close').forEach(button=>button.click())")
        for theme, language in [('light', 'en'), ('dark', 'zh-CN')]:
            await page.evaluate("args=>{PairRoomTheme.setTheme(args[0]);PairRoomI18n.setLang(args[1]);}", [theme, language])
            await page.wait_for_timeout(120)
            assert await page.locator('#page-title').text_content() == 'example', 'locale reset Project identity to Overview'
            await page.screenshot(path=str(artifacts / f'management-project-{theme}-{language}.png'))
            for width in [320, 390, 680, 900]:
                await page.set_viewport_size({'width': width, 'height': 844})
                await page.wait_for_timeout(80)
                assert not await page.evaluate('document.documentElement.scrollWidth>innerWidth'), f'horizontal overflow at {width}'
                assert await page.locator('#add-project-button').get_attribute('aria-label') or await page.locator('#add-project-button').inner_text(), 'compact action lost its accessible name'
                size = await page.locator('#add-project-button').bounding_box()
                assert size and size['height'] <= 48 and size['x'] + size['width'] <= width, f'compact action clipped or expanded at {width}: {size}'
            await page.set_viewport_size({'width': 390, 'height': 844})
            await page.wait_for_timeout(80)
            await page.screenshot(path=str(artifacts / f'management-mobile-{theme}-{language}.png'))
            await page.set_viewport_size({'width': 1440, 'height': 1000})
        results['responsive_header_and_locale_identity'] = True
        assert not errors, errors
        results['page_errors'] = errors
        (artifacts / 'results.json').write_text(json.dumps(results, indent=2) + '\n', encoding='utf-8')
        print(json.dumps(results, indent=2))
        await browser.close()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--browser', default=os.environ.get('PAIRROOM_BROWSER_EXECUTABLE'))
    parser.add_argument('--artifacts', type=Path, default=ROOT / '.browser-results' / 'management')
    args = parser.parse_args()
    asyncio.run(verify(args.browser, args.artifacts))
