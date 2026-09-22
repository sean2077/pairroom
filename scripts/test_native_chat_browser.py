#!/usr/bin/env python3
"""Native IM presentation regressions with a deterministic snapshot/HTTP fixture.

Runs the shipped HTML, CSS and JavaScript. API and locale data are synthetic;
this is not native runtime E2E. The real-transport suite remains separate.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import re
from pathlib import Path

from playwright.async_api import async_playwright, expect

ROOT = Path(__file__).resolve().parents[1]
ASSETS = ROOT / 'internal/service/assets'


def fixture_html(isolated: bool = False) -> str:
    html = (ASSETS / 'native-host.html').read_text(encoding='utf-8')
    html = re.sub(r'<script\b[^>]*>.*?</script>', '', html, flags=re.S)
    html = re.sub(r'<link\b[^>]*>', '', html)
    styles = (ASSETS / 'native-host.css').read_text(encoding='utf-8')
    # Isolated mode deliberately excludes shared chrome; default CI exercises
    # the native sheet followed by the actual production workbench overrides.
    if not isolated:
        styles += (ROOT / 'internal/webui/assets/workbench.css').read_text(encoding='utf-8')
    script = (ASSETS / 'native-host.js').read_text(encoding='utf-8')
    fixture = r'''
    window.__ids = 0;
    Object.defineProperty(crypto, 'randomUUID', {value:()=>'fixture-send-'+(++__ids)});
    window.__requests = [];
    window.__snapshot = {
      room:{id:'native-fixture',name:'Native IM collaboration',agents:{slot1:{runtime:'grok'},slot2:{runtime:'grok'}}},
      identities:{slot1:{MentionHandle:'@grok0'},slot2:{MentionHandle:'@grok1'}},
      relay:{sequence:1,bindings:{slot1:{active:true,session_id:'fixture-1',generation:1,park_enabled:true},slot2:{active:true,session_id:'fixture-2',generation:1,park_enabled:true}},messages:[],audit:[]}
    };
    const labels = {
      user:'You', target:'To', never:'Never observed', bound:'Associated', unbound:'Not bound',
      pending:'Pending', queued:'Queued', handed_off:'Handed off', unknown:'Unknown',
      cancelled:'Cancelled', cancel:'Cancel queued', retry:'Retry explicitly', published:'Published',
      commands:'Bind / wait commands', metadata:'Display-only configuration', showingLatest:'Showing latest',
      activityHelp:'Binding state and last activity are observations, not live presence.',
      empty:'No published messages yet.', placeholder:'Queue a message for a native session…'
    };
    window.PairRoomI18n={lang:'en',apply(){},t(key){return labels[key.split('.').pop()]||key;}};
    window.__message = (id,from='slot1',state='handed_off',text='Review '+id) => ({
      id,from,to:from==='slot1'?'slot2':'slot1',state,text,created_at:'2026-09-22T10:00:00Z'
    });
    __snapshot.relay.messages = Array.from({length:12},(_,i)=>__message('m'+i,i%3===0?'user':i%3===1?'slot1':'slot2'));
    window.EventSource = class {
      constructor(){window.__stream=this;this.listeners={};}
      addEventListener(kind,callback){this.listeners[kind]=callback;}
      close(){}
    };
    window.__update = () => {__snapshot.relay.sequence++;__stream.listeners.native();};
    window.fetch = async (path,options={}) => {
      __requests.push({path,method:options.method||'GET',body:options.body});
      if(path==='api/v1/session')return Response.json({csrf_token:'fixture-only'});
      if(path.startsWith('api/v1/snapshot'))return Response.json(__snapshot);
      if(path==='api/v1/messages'){
        const body=JSON.parse(options.body);
        if(window.__failSend){window.__failSend=false;return Response.json({error:'Uncertain send fixture'},{status:500});}
        if(!__snapshot.relay.messages.some(m=>m.id===body.id))__snapshot.relay.messages.push({...__message(body.id,'user','queued',body.text),to:body.to});
        __snapshot.relay.sequence++;return Response.json({id:body.id});
      }
      const match=path.match(/^api\/v1\/messages\/([^/]+)\/(retry|cancel)$/);
      if(match){
        const original=__snapshot.relay.messages.find(m=>m.id===decodeURIComponent(match[1]));
        if(match[2]==='cancel')original.state='cancelled';
        else __snapshot.relay.messages.push({...original,id:'retry-'+original.id,retry_of:original.id,state:'queued'});
        __snapshot.relay.sequence++;return Response.json({});
      }
      if(path.includes('/park')){__snapshot.relay.bindings[path.split('/')[3]].park_enabled=JSON.parse(options.body).enabled;__snapshot.relay.sequence++;return Response.json({});}
      return Response.json({error:'Unexpected fixture request'},{status:404});
    };
    '''
    # All inline scripts/styles here are repository-authored fixture content,
    # never Room input; real CSP is covered by test_native_host_browser.py.
    return html.replace('</head>', '<style>'+styles+'</style></head>').replace(
        '</body>', '<script>'+fixture+'</script><script>'+script+'</script></body>')


async def verify(browser_path: str | None, artifacts: Path, isolated: bool = False) -> None:
    artifacts.mkdir(parents=True, exist_ok=True)
    async with async_playwright() as p:
        options = {'headless': True}
        if browser_path:
            options['executable_path'] = browser_path
        browser = await p.chromium.launch(**options)
        try:
            page = await browser.new_page(viewport={'width':1440,'height':1000}, reduced_motion='reduce')
            page.set_default_timeout(5000)
            errors = []
            page.on('pageerror', lambda error: errors.append(str(error)))
            await page.set_content(fixture_html(isolated))
            await expect(page.locator('#messages .message-row')).to_have_count(12)
            await expect(page.locator('#native-inspector')).to_be_hidden()
            await expect(page.locator('#messages')).to_have_attribute('role','log')
            assert await page.get_by_role('button', name='Interrupt', exact=False).count() == 0
            await expect(page.locator('.participant-chip')).to_have_count(2)
            await expect(page.locator('.participant-chip.slot1')).to_contain_text('@grok0')
            await expect(page.locator('.participant-chip.slot2')).to_contain_text('@grok1')
            geometry = await page.evaluate('''() => {
              const user=document.querySelector('.message-row.user');
              const agent=document.querySelector('.message-row.slot1');
              return [user.querySelector('.message-avatar').getBoundingClientRect().left > user.querySelector('.message-bubble').getBoundingClientRect().right,
                agent.querySelector('.message-avatar').getBoundingClientRect().right < agent.querySelector('.message-bubble').getBoundingClientRect().left,
                getComputedStyle(user.querySelector('.message-bubble')).backgroundColor!==getComputedStyle(agent.querySelector('.message-bubble')).backgroundColor];
            }''')
            assert all(geometry), geometry
            await page.locator('#message-text').fill('Keep this unsent draft')
            await page.evaluate("window.__draftNode=document.getElementById('message-text');window.__oldMessage=document.querySelector('[data-message-id=\"m1\"]')")
            # First paint starts at the end; an incoming reply taller than the
            # viewport must still follow the end, not use post-append geometry.
            async def at_end():
                assert await page.locator('#messages').evaluate('el=>el.scrollHeight-el.scrollTop-el.clientHeight<3')
            await at_end()
            await page.evaluate("__snapshot.relay.messages.push(__message('long','slot2','queued','A long native reply\\n'.repeat(90)));__update()")
            await expect(page.locator('[data-message-id="long"]')).to_be_attached()
            await at_end()
            assert await page.locator('[data-message-id="m1"]').evaluate('el=>el===__oldMessage')
            await expect(page.locator('#message-text')).to_have_value('Keep this unsent draft')
            assert await page.locator('#message-text').evaluate('el=>el===__draftNode')
            # History position survives tail pruning and new-message arrival.
            await page.locator('#messages').evaluate('el=>el.scrollTop=400')
            await page.evaluate('''() => {
              const list=document.getElementById('messages'),top=list.getBoundingClientRect().top;
              window.__anchor=[...list.querySelectorAll('[data-message-id]')].find(n=>n.getBoundingClientRect().bottom>top);
              window.__anchorTop=__anchor.getBoundingClientRect().top;
              __snapshot.relay.messages.shift();
              __snapshot.relay.messages.push(__message('new','slot1','published','Another reply'));
              __update();
            }''')
            await expect(page.locator('[data-message-id="new"]')).to_be_attached()
            assert await page.evaluate('Math.abs(__anchor.getBoundingClientRect().top-__anchorTop)<3')
            await page.locator('#scroll-bottom').click()
            await at_end()
            # The drawer cannot eat drafts/disclosures or strand keyboard focus
            # during a normal last-activity refresh.
            await page.locator('.participant-chip.slot1').click()
            await expect(page.locator('#native-inspector')).to_be_visible()
            await expect(page.locator('[data-slot="slot1"]')).to_be_focused()
            summary = page.locator('[data-disclosure="slot1-commands"] summary')
            await summary.click()
            await summary.focus()
            await page.evaluate("__snapshot.relay.bindings.slot1.last_activity='2026-09-22T10:01:00Z';__update()")
            await expect(page.locator('[data-disclosure="slot1-commands"]')).to_have_attribute('open','')
            await expect(summary).to_be_focused()
            await summary.press('Escape')
            await expect(page.locator('#native-inspector')).to_be_hidden()
            await expect(page.locator('#inspector-toggle')).to_be_focused()
            # Quotes, literal HTML and intentional duplicate bodies are not
            # silently reinterpreted or coalesced by the presentation layer.
            await page.evaluate('''() => {
              const text='<img src=x onerror="window.__injected=true"> same body';
              const a=__message('unsafe-a','slot1','unknown',text);
              a.quote={from_handle:'You',text:'Quoted context <script>literal</scr'+'ipt>'};
              const b=__message('unsafe-b','slot1','published',text);
              b.attachments=[{id:'image/with space',name:'Fixture image'}];
              __snapshot.relay.messages.push(a,b);__update();
            }''')
            first=page.locator('[data-message-id="unsafe-a"]')
            await expect(first.locator('.reply-quote')).to_contain_text('Quoted context')
            await expect(first.locator('.message-body')).to_contain_text('<img src=x')
            assert await first.locator('img,script').count() == 0
            assert await page.evaluate('window.__injected===undefined')
            await expect(page.locator('[data-message-id="unsafe-b"] img')).to_have_attribute('src','api/v1/attachments/image%2Fwith%20space')
            await first.locator('[data-action="retry"]').click()
            await expect(page.locator('#confirm-dialog')).to_be_visible()
            assert await page.evaluate("__requests.every(r=>!r.path.endsWith('/retry'))")
            await page.locator('#confirm-dialog button[value="cancel"]').click()
            await first.locator('[data-action="retry"]').click()
            await page.locator('#confirm-retry').click()
            retry=page.locator('[data-message-id="retry-unsafe-a"]')
            await expect(retry).to_be_attached()
            await retry.locator('[data-action="cancel"]').click()
            await expect(retry).to_have_class(re.compile('state-cancelled'))
            # Unknown publication keeps its original ID/payload and the lock.
            await page.evaluate('window.__failSend=true')
            await page.locator('#send').click()
            await expect(page.locator('#message-text')).to_be_disabled()
            await page.locator('#send').click()
            await expect(page.locator('#message-text')).to_be_enabled()
            await expect(page.locator('#message-text')).to_have_value('')
            assert await page.evaluate("__requests.filter(r=>r.path==='api/v1/messages').slice(-2).map(r=>r.body).every((v,i,a)=>v===a[0])")
            # Retained totals are honest and the DOM remains bounded.
            await page.evaluate("__snapshot.relay.messages=Array.from({length:305},(_,i)=>__message('tail'+i,i%3===0?'user':i%3===1?'slot1':'slot2'));__snapshot.relay.total_messages=500;__update()")
            await expect(page.locator('#messages .message-row')).to_have_count(300)
            await expect(page.locator('#message-count')).to_have_text('500')
            await expect(page.locator('.truncated-note')).to_contain_text('300 / 500')
            # Locale changes retranslate handles/state without replacing input.
            await page.locator('#message-text').fill('仍保留草稿')
            await page.evaluate("PairRoomI18n.lang='zh-CN';document.dispatchEvent(new Event('pairroom:lang'))")
            await expect(page.locator('#message-text')).to_have_value('仍保留草稿')
            # Use a short realistic conversation for layout captures.
            await page.evaluate("__snapshot.relay.messages=[__message('view-user','user','handed_off','Please review the native Room layout.'),__message('view-lead','slot1','published','The conversation now takes priority.\\nBinding and delivery details stay available in Participants.'),__message('view-peer','slot2','queued','I will check narrow screens and preserve the current relay semantics.')];delete __snapshot.relay.total_messages;__update()")
            await expect(page.locator('#messages .message-row')).to_have_count(3)
            for theme in ('light','dark'):
                await page.evaluate('(theme)=>document.documentElement.dataset.theme=theme',theme)
                for width,height in ((1440,1000),(420,900),(320,700),(740,500)):
                    await page.set_viewport_size({'width':width,'height':height})
                    await expect(page.locator('#send')).to_be_in_viewport()
                    assert await page.locator('#messages').evaluate('el=>el.clientHeight>50'), (width,height)
                    assert await page.evaluate('document.documentElement.scrollWidth<=innerWidth+1'), (theme,width)
                    await page.locator('#inspector-toggle').click()
                    await expect(page.locator('#native-inspector')).to_be_visible()
                    assert await page.evaluate('document.documentElement.scrollWidth<=innerWidth+1'), ('drawer',theme,width)
                    await page.locator('#inspector-toggle').click()
                    await expect(page.locator('#message-text')).to_have_value('仍保留草稿')
                    if width in (1440,420):
                        await page.screenshot(path=str(artifacts/f'native-im-{theme}-{width}.png'))
            assert not errors, errors
            (artifacts/'results.json').write_text(json.dumps({'fixture':True,'shared_workbench':not isolated,
                'real_vendor_e2e':False,'checks':['IM alignment','duplicate runtime identities','initial and incoming scroll',
                'history anchor','draft/node preservation','binding disclosure/focus','literal text/quotes/attachments',
                'retry confirmation/cancel','uncertain send identity','300-message bound','locale switch','responsive light/dark'],
                'browser_errors':errors},indent=2)+'\n',encoding='utf-8')
            print('Native IM browser fixture passed (not vendor E2E)',flush=True)
        finally:
            await browser.close()


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--browser',default=os.environ.get('PAIRROOM_BROWSER_EXECUTABLE'))
    parser.add_argument('--artifacts',type=Path,default=ROOT/'.browser-results/native-chat')
    parser.add_argument('--isolated',action='store_true',help='Test native assets without shared workbench chrome')
    args=parser.parse_args()
    asyncio.run(verify(args.browser,args.artifacts,args.isolated))


if __name__=='__main__':
    main()
