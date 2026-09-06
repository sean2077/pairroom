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
from urllib.parse import urlparse

from playwright.async_api import async_playwright, expect

from test_room_browser import ROOT, collaboration_fixture


def fixture_html() -> str:
    html = (ROOT / 'internal/service/assets/index.html').read_text(encoding='utf-8')

    def asset(src: str) -> str:
        directory = 'internal/webui/assets' if '/_pairroom/' in src else 'internal/service/assets'
        return (ROOT / directory / src.rsplit('/', 1)[-1]).read_text(encoding='utf-8')

    html = re.sub(r'<link\b[^>]*rel="icon"[^>]*>', '', html)
    html = re.sub(r'<link\b[^>]*rel="stylesheet"[^>]*href="([^"]+)"[^>]*>', lambda m: ('<style id="management-styles">' if m[1] == '/management.css' else '<style>') + asset(m[1]) + '</style>', html)
    html = re.sub(r'<script\b[^>]*src="([^"]+)"[^>]*></script>', lambda m: '<script>' + asset(m[1]).replace('</script>', '<\\/script>') + '</script>', html)
    mock = r'''
      window.__snapshot = {
        version:'2.1.0',store_schema:10,data_root:'/state',healthy:true,
        generated_at:'2026-09-06T00:00:00Z',
        projects:[{id:'p1',root:'/workspace/example',available:true}],
        rooms:['r1','r2'].map((id,i)=>({id,project_id:'p1',name:i?'Review workspace':'Implementation workspace',
          lifecycle:'active',bindings:{claude:{agent:'claude',mode:'new',pending:true},codex:{agent:'codex',mode:'new',pending:true}},
          transcript_boundary_notice:'Earlier native history remains in its native harness.'})),
        runtimes:['r1','r2'].map(room_id=>({room_id,phase:'active',busy:false,occupies_capacity:true})),
        runtime_policy:{limit:4,idle_timeout_seconds:900},
        capabilities:{room_surface:true,room_deletion:true,project_refresh:true,project_removal:true,runtime_suspend:true}
      };
      window.__catalog = {collaboration_default:COLLABORATION,profiles:[{name:'Example Provider',runtime:'claude',supported:true,provider:{source:'cc-switch',app_type:'claude',profile_id:'fixture'}}],runtimes:['claude','codex','grok'].map(runtime=>({runtime,
        display_name:{claude:'Claude Code',codex:'Codex',grok:'Grok Build'}[runtime],available:true})),
        defaults:{claude:{runtime:'claude',provider:{source:'native'},permission_mode:'yolo'},codex:{runtime:'codex',provider:{source:'native'},approval_policy:'yolo',sandbox:'danger-full-access'}}};
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
    mock = mock.replace('COLLABORATION', json.dumps(collaboration_fixture()).replace('</', '<\\/'))
    return html.replace('<head>', '<head><script>' + mock + '</script>', 1)


async def load_csp_fixture(page) -> None:
    """Load real external assets under the production CSP, with test-only HTTP state."""
    mock = re.search(r'<head><script>(.*?)</script>', fixture_html(), re.S)
    assert mock, 'missing Management state fixture'
    await page.add_init_script(mock[1])
    await page.add_init_script("window.__cspErrors=[]; document.addEventListener('securitypolicyviolation', e=>__cspErrors.push(e.violatedDirective))")
    server = (ROOT / 'internal/service/management.go').read_text(encoding='utf-8')
    csp = re.search(r'Set\("Content-Security-Policy", "([^"]+)"\)', server)
    assert csp, 'missing production Management CSP'

    async def route(request):
        path = urlparse(request.request.url).path
        if path == '/':
            await request.fulfill(status=200, content_type='text/html', headers={'Content-Security-Policy': csp[1]},
                                  body=(ROOT / 'internal/service/assets/index.html').read_text(encoding='utf-8'))
        elif path.endswith('/surface/'):
            await request.fulfill(status=200, content_type='text/html', body='<html><body>Inert Room fixture</body></html>')
        else:
            directory = 'internal/webui/assets' if path.startswith('/_pairroom/') else 'internal/service/assets'
            asset = ROOT / directory / path.rsplit('/', 1)[-1]
            if not asset.is_file():
                await request.fulfill(status=404, body='Fixture resource not found')
                return
            content_type = {'.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml'}.get(asset.suffix, 'text/plain')
            await request.fulfill(status=200, content_type=content_type, body=asset.read_bytes())

    await page.route('http://127.0.0.1:7332/**', route)
    await page.goto('http://127.0.0.1:7332/#/projects/p1')


async def verify_names(browser, artifacts: Path, in_page_fixture: bool = False) -> dict:
    page = await browser.new_page(viewport={'width': 1440, 'height': 1000}, locale='en-US', reduced_motion='reduce')
    page.set_default_timeout(5000)
    errors = []
    page.on('pageerror', lambda error: errors.append(str(error)))
    if in_page_fixture:
        await page.evaluate("location.hash='#/projects/p1'")
        await page.set_content(fixture_html())
    else:
        await load_csp_fixture(page)
    await page.wait_for_selector('#app:not([hidden]) .tree-room')
    # Stateful fixture mirrors only the name/read-side contract. Real persistence,
    # authentication and safe native boundaries are verified in Go tests.
    await page.evaluate("""() => {
      const original=window.fetch;
      window.__nameWrites=[]; window.__renameFail=false; window.__expired=false;
      const names=room => Object.fromEntries(['claude','codex'].map(actor=>[actor,`${room.name} · @${actor} · ${room.id.slice(-12)}`]));
      __snapshot.rooms.forEach(room=>room.runtime_names=names(room));
      window.fetch=async(path,options={})=>{
        const reply=(body,status=200)=>new Response(JSON.stringify(body),{status,headers:{'content-type':'application/json'}});
        if(__expired&&path==='/api/v1/service') return reply({error:'Expired'},401);
        if(options.method==='POST'&&path==='/api/v1/projects/p1/rooms'){
          const sent=JSON.parse(options.body); __nameWrites.push({path,...sent});
          const room={...structuredClone(__snapshot.rooms[0]),id:'room-111111111111cccccccccccc',name:sent.name||'Room-cccccccccccc',bindings:sent.bindings,agents:sent.agents};
          room.runtime_names=names(room); __snapshot.rooms.push(room);return reply(room,201);
        }
        if(options.method==='PATCH'&&path.startsWith('/api/v1/rooms/')){
          const sent=JSON.parse(options.body); __nameWrites.push({path,...sent});
          if(__renameFail) return reply({error:'Fixture rename failed'},409);
          const room=__snapshot.rooms.find(room=>room.id===path.split('/').at(-1));
          room.name=sent.name; room.runtime_names=names(room);return reply(room);
        }
        return original(path,options);
      };
      document.getElementById('refresh-button').click();
    }""")
    await page.get_by_role('button', name='+ Create Room', exact=True).first.click()
    await expect(page.locator('#room-submit')).to_be_enabled()
    assert not await page.locator('#room-name').get_attribute('required')
    assert await page.locator('#room-name').input_value() == ''
    await page.screenshot(path=str(artifacts / 'room-name-optional-light.png'))
    await page.locator('#room-submit').click()
    await expect(page.locator('#room-dialog')).not_to_be_visible()
    await page.wait_for_selector('.tree-room[data-room-id="room-111111111111cccccccccccc"]')
    assert await page.evaluate('__nameWrites[0].name') == '', 'browser generated its own competing name'
    auto = page.locator('.tree-room[data-room-id="room-111111111111cccccccccccc"]')
    assert 'Room-cccccccccccc' in await auto.inner_text()
    # A genuine mouse context menu on the sidebar addresses the clicked Room.
    await auto.click(button='right')
    assert await page.locator('#room-context-menu').is_visible()
    assert await page.locator('#context-room-name').inner_text() == 'Room-cccccccccccc'
    await page.screenshot(path=str(artifacts / 'room-context-menu-light.png'))
    await page.locator('#context-rename-room').click()
    assert await page.locator('#rename-room-id').input_value() == 'room-111111111111cccccccccccc'
    await page.locator('#rename-room-name').fill('地图渲染优化')
    await page.locator('#rename-form [type=submit]').click()
    await expect(page.locator('#rename-dialog')).not_to_be_visible()
    await expect(auto).to_contain_text('地图渲染优化')
    assert await page.locator('.room-row[data-room-id="room-111111111111cccccccccccc"] .binding-runtime-name').evaluate_all('nodes=>nodes.map(n=>n.textContent)') == ['地图渲染优化 · @claude · cccccccccccc', '地图渲染优化 · @codex · cccccccccccc']
    # The open tab's identity survives a display-name change. No new native ID.
    await auto.click()
    tab = page.locator('.room-tab[data-room-id="room-111111111111cccccccccccc"] .room-tab-target')
    await tab.wait_for()
    await tab.press('Shift+F10')
    assert await page.locator('#room-context-menu').is_visible()
    await page.keyboard.press('Escape')
    assert await tab.evaluate('node=>document.activeElement===node'), 'keyboard menu lost focus'
    await tab.click(button='right')
    await page.locator('#context-rename-room').click()
    await page.locator('#rename-room-name').fill('Renderer review')
    await page.locator('#rename-form [type=submit]').click()
    await expect(page.locator('#rename-dialog')).not_to_be_visible()
    await expect(tab).to_contain_text('Renderer review')
    assert await page.locator('#room-stage [data-room-id="room-111111111111cccccccccccc"]').count() == 1
    # Project-row context menu works without navigating into the Room first.
    await page.evaluate("location.hash='#/projects/p1'")
    row = page.locator('.room-row[data-room-id="r2"]')
    await row.wait_for()
    await row.click(button='right')
    await page.locator('#context-rename-room').click()
    assert await page.locator('#rename-room-id').input_value() == 'r2'
    count = await page.evaluate('__nameWrites.length')
    await page.locator('#rename-room-name').fill('名' * 54)
    await page.locator('#rename-form [type=submit]').click()
    assert await page.evaluate('__nameWrites.length') == count, 'UTF-8 limit bypassed'
    await page.locator('#rename-room-name').fill('Review workspace')
    await page.locator('#rename-form [type=submit]').click()
    assert await page.evaluate('__nameWrites.length') == count, 'unchanged name sent a mutation'
    assert not await page.locator('#rename-dialog').evaluate('node=>node.open')
    await row.click(button='right')
    await page.locator('#context-rename-room').click()
    await page.evaluate('__renameFail=true')
    await page.locator('#rename-room-name').fill('Retain failed rename')
    await page.locator('#rename-form [type=submit]').click()
    await expect(page.locator('#rename-form-error')).to_contain_text('Fixture rename failed')
    assert await page.locator('#rename-room-name').input_value() == 'Retain failed rename'
    await page.locator('#rename-dialog [data-close-dialog=rename-dialog]').first.click()
    # Viewport positioning and localized accessible operation labels.
    await page.evaluate("PairRoomI18n.setLang('zh-CN'); PairRoomTheme.setTheme('dark')")
    await page.set_viewport_size({'width':390,'height':844})
    await page.evaluate('new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))')
    await row.evaluate("node=>node.dispatchEvent(new MouseEvent('contextmenu',{bubbles:true,clientX:388,clientY:842}))")
    # Stylesheet-rule changes are reflected at the next rendering opportunity.
    await page.evaluate('new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))')
    menu = await page.locator('#room-context-menu').bounding_box()
    assert menu and menu['x'] >= 0 and menu['x']+menu['width']<=390 and menu['y']+menu['height']<=844, menu
    assert await page.locator('#context-rename-room').inner_text() == '重命名 Room'
    await page.screenshot(path=str(artifacts / 'room-context-menu-mobile-dark.png'))
    await page.keyboard.press('Escape')
    await page.set_viewport_size({'width':1440,'height':1000})
    await row.click(button='right')
    await page.evaluate("__snapshot.rooms=__snapshot.rooms.filter(r=>r.id!=='r2');document.getElementById('refresh-button').click()")
    await expect(page.locator('#room-context-menu')).not_to_be_visible()
    await page.locator('.tree-room[data-room-id="r1"]').click(button='right')
    await page.evaluate("__expired=true;document.getElementById('refresh-button').click()")
    await page.wait_for_selector('#login-screen:not([hidden])')
    assert not await page.locator('#room-context-menu').is_visible(), 'menu survived logout'
    assert not errors, errors
    if not in_page_fixture:
        assert not await page.evaluate('__cspErrors'), 'context menu violated production CSP'
    assert await page.locator('#room-context-menu').get_attribute('style') is None
    # Reopening the same menu must replace, not accumulate, positioning rules.
    assert await page.evaluate("Array.from(document.styleSheets).flatMap(s=>Array.from(s.cssRules)).filter(r=>r.selectorText==='#room-context-menu').length") == 1
    await page.close()
    return dict(context_menu_strict_csp=not in_page_fixture, optional_room_name=True, server_name_receipt=True, context_rename_sidebar_tab_row=True,
                name_maps_preserved=True, rename_keyboard_focus=True, rename_utf8_limit=True,
                rename_noop_no_post=True, rename_failure_preserves_text=True, stale_menu_closed=True,
                context_menu_mobile_clamped=True, names_page_errors=errors)


async def verify(browser_path: str | None, artifacts: Path, in_page_fixture: bool = False) -> None:
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
        assert await page.locator('#room-collaboration-mode option').evaluate_all('nodes=>nodes.map(n=>n.value)') == ['default', 'custom']
        assert await page.locator('#room-collaboration-mode').input_value() == 'default'
        assert 'Lead' in await page.locator('#claude-responsibility-label').inner_text()
        assert 'Executor' in await page.locator('#codex-responsibility-label').inner_text()
        assert await page.locator('#claude-permission-mode').input_value() == 'yolo'
        assert await page.locator('#codex-approval-policy').input_value() == 'yolo'
        assert await page.locator('#codex-sandbox').input_value() == 'danger-full-access'
        assert await page.locator('#claude-reviewer-policy, #codex-reviewer-policy').count() == 0
        await page.locator('#room-collaboration-mode').select_option('custom')
        assert not await page.locator('#room-collaboration-instructions').evaluate('node=>node.checkValidity()')
        custom = 'Agent 2 proposes. Agent 1 implements; both challenge unsupported assumptions.'
        await page.locator('#room-collaboration-instructions').fill(custom)
        await page.evaluate("PairRoomI18n.setLang('zh-CN')")
        await page.wait_for_timeout(80)
        assert await page.locator('#room-collaboration-instructions').input_value() == custom
        assert '主导者' not in await page.locator('#claude-responsibility-label').inner_text()
        await page.set_viewport_size({'width':390, 'height':844})
        assert not await page.locator('#room-dialog').evaluate('node=>node.scrollWidth>node.clientWidth')
        await page.screenshot(path=str(artifacts / 'creation-custom-mobile-zh-CN.png'))
        await page.set_viewport_size({'width':1440, 'height':1000})
        await page.evaluate("PairRoomI18n.setLang('en')")
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
        await page.locator('#claude-model').fill('model-before-submit')
        await page.screenshot(path=str(artifacts / 'management-room-config-light.png'))
        await page.locator('#room-submit').click()
        await page.wait_for_function("!document.getElementById('room-dialog').open")
        sent = await page.evaluate("JSON.parse(__writes.filter(w=>w.path.endsWith('/rooms')).at(-1).body)")
        assert sent['collaboration'] == {'mode':'custom', 'instructions':custom}
        assert sent['agents']['claude']['model'] == 'model-before-submit'
        assert all('ordinary_reviewer_policy' not in agent for agent in sent['agents'].values())
        await page.get_by_role('button', name='+ Create Room', exact=True).first.click()
        await page.wait_for_function("!document.getElementById('room-submit').disabled")
        assert await page.locator('#room-collaboration-mode').input_value() == 'default'
        assert await page.locator('#room-collaboration-instructions').input_value() == ''
        await page.screenshot(path=str(artifacts / 'creation-default-light.png'))
        results.update(two_creation_modes=True, default_yolo=True, custom_payload_exact=True, custom_locale_preserved=True)
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
        results.update(await verify_names(browser, artifacts, in_page_fixture))
        results['page_errors'] = errors
        (artifacts / 'results.json').write_text(json.dumps(results, indent=2) + '\n', encoding='utf-8')
        print(json.dumps(results, indent=2))
        await browser.close()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--browser', default=os.environ.get('PAIRROOM_BROWSER_EXECUTABLE'))
    parser.add_argument('--artifacts', type=Path, default=ROOT / '.browser-results' / 'management')
    parser.add_argument('--in-page-fixture', action='store_true', help='For isolated browsers that forbid navigation: skip the production CSP check, use inline fixture assets')
    args = parser.parse_args()
    asyncio.run(verify(args.browser, args.artifacts, args.in_page_fixture))
