#!/usr/bin/env python3
"""Project/Room ordering and unified Settings, using real assets and no vendors."""
from __future__ import annotations
import argparse
import asyncio
import json
from pathlib import Path
from playwright.async_api import async_playwright, expect
from test_management_browser import fixture_html, load_csp_fixture, wait_fixture_state


async def verify_ordering(browser, artifacts: Path, in_page_fixture: bool = False) -> dict:
    page = await browser.new_page(viewport={'width': 1440, 'height': 1000}, locale='en-US', reduced_motion='reduce')
    page.set_default_timeout(5000)
    errors = []
    page.on('pageerror', lambda error: errors.append(str(error)))
    if in_page_fixture:
        await page.evaluate("location.hash='#/projects'")
        await page.set_content(fixture_html())
    else:
        await load_csp_fixture(page)
    await page.wait_for_selector('#app:not([hidden]) .tree-room')
    await page.evaluate(r"""() => {
      __snapshot.projects.push({id:'p2',root:'/workspace/another',available:true}, {id:'p3',root:'/workspace/third',available:true});
      __snapshot.rooms.push({...structuredClone(__snapshot.rooms[0]),id:'r3',name:'Third workspace'}, {...structuredClone(__snapshot.rooms[0]),id:'r4',project_id:'p2',name:'Other Project Room'});
      __snapshot.rooms.forEach(room=>room.agents=structuredClone(__catalog.defaults));
      __snapshot.navigation_order={schema:1,projects:[],rooms:{}};
      window.__moves=[]; window.__moveFail=false; window.__moveDelay=0;
      const original=window.fetch;
      window.fetch=async (path,options={}) => {
        if(path!=='/api/v1/navigation-order') return original(path,options);
        const input=JSON.parse(options.body); __moves.push(input);
        await new Promise(resolve=>setTimeout(resolve,__moveDelay));
        if(__moveFail) return new Response(JSON.stringify({error:'Write rejected'}),{status:503});
        const source=input.kind==='project'?__snapshot.projects:__snapshot.rooms.filter(r=>r.project_id===__snapshot.rooms.find(i=>i.id===input.id).project_id);
        const order=__snapshot.navigation_order;
        const project=source[0].project_id;
        const saved=input.kind==='project'?order.projects:order.rooms[project]||[];
        const ids=[...saved.filter(id=>source.some(i=>i.id===id)),...source.map(i=>i.id).filter(id=>!saved.includes(id))];
        ids.splice(ids.indexOf(input.id),1);
        ids.splice(ids.indexOf(input.target_id)+(input.position==='after'?1:0),0,input.id);
        if(input.kind==='project') order.projects=ids; else order.rooms[project]=ids;
        return new Response(JSON.stringify(order),{status:200});
      };
      location.hash='#/projects';
    }""")
    await page.locator('#refresh-button').click()
    await page.wait_for_selector('#view [data-order-id="p3"] .order-handle')

    async def ids(scope, kind):
        return await page.locator(f'{scope} [data-order-kind="{kind}"]').evaluate_all('(rows)=>rows.map(r=>r.dataset.orderId)')

    async def drag(scope, kind, source, target, *, cancel=False):
        handle = page.locator(f'{scope} [data-order-kind="{kind}"][data-order-id="{source}"] .order-handle')
        await handle.scroll_into_view_if_needed()
        box = await handle.bounding_box()
        destination = await page.locator(f'{scope} [data-order-kind="{kind}"][data-order-id="{target}"]').bounding_box()
        assert box and destination
        await page.mouse.move(box['x']+box['width']/2, box['y']+box['height']/2)
        await page.mouse.down()
        await page.mouse.move(destination['x']+min(100, destination['width']/2), destination['y']+4, steps=8)
        if cancel:
            await page.keyboard.press('Escape')
        await page.mouse.up()

    assert await ids('#view','project') == ['p1','p2','p3']
    boxes = await page.locator('.project-list-row').evaluate_all('(rows)=>rows.map(r=>({x:r.offsetLeft,y:r.offsetTop,w:r.offsetWidth,h:r.offsetHeight}))')
    assert len({b['x'] for b in boxes}) == 1 and len({b['w'] for b in boxes}) == 1
    assert all(boxes[i]['y']+boxes[i]['h']<=boxes[i+1]['y'] for i in range(2)), boxes
    await drag('#view','project','p3','p1')
    await wait_fixture_state(page, "__snapshot.navigation_order.projects[0]==='p3' && !document.querySelector('#view .order-handle').disabled")
    assert await ids('#view','project') == ['p3','p1','p2']
    assert await ids('#room-tree','project') == ['p3','p1','p2']
    await page.locator('#refresh-button').click()
    assert await ids('#view','project') == ['p3','p1','p2']
    await page.screenshot(path=str(artifacts / 'projects-ordered-light-en.png'), full_page=True)
    count = await page.evaluate('__moves.length')
    await drag('#view','project','p2','p3',cancel=True)
    assert await page.evaluate('__moves.length') == count, 'Escape submitted a drag'
    assert not await page.locator('.order-before,.order-after,.order-dragging').count()
    # A single-pointer (non-drag) alternative and native keyboard access.
    await page.locator('#view [data-order-id="p1"] .order-handle').click()
    await page.get_by_role('button', name='Move down', exact=True).click()
    await wait_fixture_state(page, "__snapshot.navigation_order.projects[2]==='p1' && !document.querySelector('#view .order-handle').disabled")
    assert await ids('#view','project') == ['p3','p2','p1']
    await page.locator('#view [data-order-id="p1"] .order-handle').press('ArrowUp')
    await wait_fixture_state(page, "__snapshot.navigation_order.projects[1]==='p1' && !document.querySelector('#view .order-handle').disabled")
    assert await page.locator('#view [data-order-id="p1"] .order-handle').evaluate('(e)=>e===document.activeElement')

    await page.evaluate("location.hash='#/projects/p1'")
    await page.wait_for_selector('#project-room-search')
    await drag('#view','room','r3','r1')
    await wait_fixture_state(page,"__snapshot.navigation_order.rooms.p1?.[0]==='r3' && !document.querySelector('#view .order-handle').disabled")
    assert await ids('#view','room') == ['r3','r1','r2']
    assert (await ids('#room-tree','room'))[:3] == ['r3','r1','r2']
    # Filtered movement keeps unseen Room IDs; arrows intentionally use the full
    # Project order, not only the visible search result.
    await page.locator('#project-room-search').fill('Review')
    await page.locator('#view [data-order-id="r2"] .order-handle').press('ArrowUp')
    await wait_fixture_state(page,"__snapshot.navigation_order.rooms.p1?.[1]==='r2' && !document.querySelector('#view .order-handle').disabled")
    assert await page.evaluate('__snapshot.navigation_order.rooms.p1') == ['r3','r2','r1']
    await page.locator('#project-room-search').fill('')
    # Rejected writes do not pretend to reorder the list.
    await page.evaluate('__moveFail=true')
    await page.locator('#view [data-order-id="r1"] .order-handle').press('ArrowUp')
    await wait_fixture_state(page,"document.getElementById('navigation-order-status')?.textContent.includes('Could not save order') && !document.querySelector('#view .order-handle').disabled")
    assert await ids('#view','room') == ['r3','r2','r1']
    await page.evaluate('__moveFail=false;__moveDelay=200')
    await page.locator('#view [data-order-id="r1"] .order-handle').press('ArrowUp')
    assert await page.locator('#view [data-order-id="r1"] .order-handle').is_disabled()
    await wait_fixture_state(page,"__snapshot.navigation_order.rooms.p1?.[1]==='r1' && !document.querySelector('#view .order-handle').disabled")
    # Polling during drag retains the exact DOM handle/pointer capture.
    handle = page.locator('#room-tree [data-order-id="r2"] .order-handle')
    await handle.evaluate('(e)=>window.__heldHandle=e')
    box = await handle.bounding_box()
    await page.mouse.move(box['x']+14,box['y']+10)
    await page.mouse.down()
    await page.mouse.move(box['x']+16,box['y']+25)
    await page.evaluate("__snapshot.generated_at='changed-during-drag';document.getElementById('refresh-button').click()")
    await page.wait_for_timeout(120)
    assert await page.evaluate('__heldHandle.isConnected && __heldHandle===document.querySelector(\'#room-tree [data-order-id="r2"] .order-handle\')')
    await page.keyboard.press('Escape'); await page.mouse.up()
    # Cross-Project hover must not move ownership or write a display rank.
    count = await page.evaluate('__moves.length')
    await drag('#room-tree','room','r2','r4')
    assert await page.evaluate('__moves.length') == count
    await page.evaluate("document.querySelectorAll('#toasts .toast-close').forEach(b=>b.click())")
    await page.screenshot(path=str(artifacts / 'rooms-ordered-light-en.png'), full_page=True)

    await page.evaluate("location.hash='#/diagnostics/r1'")
    await expect(page.locator('#settings-diagnostics')).to_be_visible()
    assert await page.evaluate('location.hash') == '#/settings/diagnostics/r1'
    assert await page.locator('[data-nav="diagnostics"]').count() == 0
    assert await page.locator('.settings-nav [aria-current="page"]').inner_text() == 'Diagnostics'
    assert await page.locator('#diagnostic-scope').input_value() == 'r1'
    fonts = await page.locator('.settings-nav button').evaluate_all('(buttons)=>buttons.map(b=>[parseFloat(getComputedStyle(b).fontSize),b.getBoundingClientRect().height])')
    assert all(font>=14 and height>=44 for font,height in fonts), fonts
    summary = page.locator('.settings-content details > summary')
    await summary.click()
    assert 'Service' in await page.locator('.settings-content details').inner_text()
    await summary.click()
    for theme, language in [('light','en'),('dark','zh-CN')]:
        await page.evaluate("args=>{PairRoomTheme.setTheme(args[0]);PairRoomI18n.setLang(args[1]);}",[theme,language])
        await page.screenshot(path=str(artifacts / f'settings-unified-{theme}-{language}.png'),full_page=True)
        for route in ['#/projects','#/projects/p1','#/settings/diagnostics']:
            await page.evaluate('(route)=>location.hash=route',route)
            for width in [320,390,768,1440]:
                await page.set_viewport_size({'width':width,'height':1000})
                await wait_fixture_state(page,'document.documentElement.scrollWidth<=innerWidth')
    if not in_page_fixture:
        assert not await page.evaluate('__cspErrors')
    assert not errors, errors
    await page.close()
    return dict(project_single_rows=True,project_and_room_drag_persisted=True,ordering_keyboard_and_click=True,ordering_failure_and_cancellation=True,ordering_poll_dom_stable=True,ordering_filtered_and_scoped=True,diagnostics_single_settings_destination=True,settings_readable_navigation=True)


async def main(args):
    args.artifacts.mkdir(parents=True,exist_ok=True)
    async with async_playwright() as p:
        browser=await p.chromium.launch(headless=True,**({'executable_path':args.browser} if args.browser else {}))
        result=await verify_ordering(browser,args.artifacts,args.in_page_fixture)
        print(json.dumps(result,indent=2))
        await browser.close()

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--browser')
    parser.add_argument('--in-page-fixture',action='store_true')
    parser.add_argument('--artifacts',type=Path,required=True)
    asyncio.run(main(parser.parse_args()))
