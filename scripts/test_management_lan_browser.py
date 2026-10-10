"""Native LAN owner controls and guest publication under the Management CSP.

HTTP results are deterministic fixtures. This verifies the shipped browser UI,
not TLS admission, native process delivery, or real vendor model acceptance.
"""
from __future__ import annotations

from pathlib import Path

from playwright.async_api import expect


async def verify_lan(browser, artifacts: Path, in_page_fixture: bool = False) -> dict:
    from test_management_browser import fixture_html, load_csp_fixture, wait_fixture_state

    page = await browser.new_page(viewport={'width': 1440, 'height': 1000}, locale='en-US', reduced_motion='reduce')
    page.set_default_timeout(5000)
    errors = []
    page.on('pageerror', lambda error: errors.append(str(error)))
    if in_page_fixture:
        await page.evaluate("location.hash='#/projects/p1'")
        await page.set_content(fixture_html())
    else:
        await load_csp_fixture(page)
    await expect(page.locator('#project-room-search')).to_be_visible()
    await page.evaluate(r"""() => {
      const original = window.fetch;
      window.__lanWrites = [];
      window.__lanConfig = {enabled:false,address:'',host_pin:'a'.repeat(64)};
      window.__lanState = {invites:[],pending:[{request_id:'request-fixture',fingerprint:'b'.repeat(64),runtime:'grok',label:'Unverified teammate label'}]};
      window.__lanMessages = [{id:'shared-message',from:'user',author:'lan:'+ 'b'.repeat(64),to:'slot1',state:'queued',
        text:'Reproduction script attached',attachments:[{id:'evidence-id',kind:'file',media_type:'text/plain',name:'repro <img onerror=alert(1)>.txt',size:24,sha256:'c'.repeat(64)}]}];
      window.__lanUncertain = true;
      window.fetch = async (path, options={}) => {
        const method=options.method||'GET';
        const input=options.body ? JSON.parse(options.body) : {};
        if (path==='/api/v1/lan') {
          if(method==='PUT') {__lanWrites.push({path,input});__lanConfig={...__lanConfig,...input,endpoint:'https://'+input.address};}
          return Response.json(__lanConfig);
        }
        if (path==='/api/v1/projects/p1/rooms' && method==='POST') {
          __lanWrites.push({path,input});
          const peer=input.owner_slot==='slot1'?'slot2':'slot1';
          const room={...input,id:'shared-r',project_id:'p1',lifecycle:'active',agents:{...input.agents,[peer]:{awaiting_peer:true}}};
          __snapshot.rooms.push(room);
          __snapshot.runtimes.push({room_id:room.id,phase:'active',host_mode:'native',occupies_capacity:false});
          return Response.json(room);
        }
        if(path.startsWith('/api/v1/rooms/shared-r/lan')) {
          if(method==='POST') __lanWrites.push({path,input});
          if(path.endsWith('/invite')) return Response.json({invite:'pairroom://join/aW52aXRlLWZpeHR1cmU',expires_at:'2030-01-01T00:00:00Z'});
          if(path.endsWith('/accept')) {
            __lanState={invites:[],pending:[],member:{request_id:'request-fixture',binding:{slot:'slot1',runtime:'grok',remote_key:'b'.repeat(64),generation:1,active:true}}};
            __snapshot.rooms.find(room=>room.id==='shared-r').agents.slot1={runtime:'grok'};
            return Response.json({status:'accepted'});
          }
          return Response.json(__lanState);
        }
        if(path.startsWith('/api/v1/lan/joined/guest-local/')) {
          if(path.endsWith('/history')) return Response.json({messages:__lanMessages,has_more:false});
          if(path.endsWith('/send')) {
            __lanWrites.push({path,input});
            window.__lanAccepted={...input,from:'user',author:'lan:'+ 'b'.repeat(64),state:'queued'};
            if(__lanUncertain) {__lanUncertain=false;return Response.json({error:'Publication response lost'},{status:500});}
            return Response.json(__lanAccepted);
          }
          if(path.endsWith('/receipt')) {__lanWrites.push({path,input});return Response.json({accepted:true,message:__lanAccepted});}
        }
        return original(path,options);
      };
    }""")
    await page.evaluate("location.hash='#/settings/lan'")
    await expect(page.locator('#lan-listen-address')).to_be_enabled()
    assert await page.evaluate('__lanWrites.length') == 0, 'opening LAN settings changed the listener'
    await page.locator('#lan-listen-enabled').check()
    await page.locator('#lan-listen-address').fill('192.168.1.23:8877')
    await page.locator('#view .lan-settings-actions .primary-button').click()
    await expect(page.locator('#lan-listen-address')).to_be_enabled()
    assert await page.evaluate('__lanWrites[0]') == {'path':'/api/v1/lan','input':{'enabled':True,'address':'192.168.1.23:8877'}}

    await page.evaluate("location.hash='#/projects/p1'")
    await page.get_by_role('button', name='+ Create Room', exact=True).first.click()
    await expect(page.locator('#room-submit')).to_be_enabled()
    await expect(page.locator('#room-host-mode')).to_have_value('native')
    await page.locator('#room-share-lan').check()
    await page.locator('#room-lan-owner-slot').select_option('slot2')
    await expect(page.locator('fieldset[data-actor="slot1"]')).to_be_hidden()
    await expect(page.locator('fieldset[data-actor="slot2"]')).to_be_visible()
    await expect(page.locator('#room-pair-profile-controls')).to_be_hidden()
    await expect(page.locator('#room-lan-peer')).to_contain_text('awaiting teammate')
    await page.locator('#room-name').fill('LAN bug reproduction')
    await page.locator('#room-submit').click()
    await expect(page.locator('#lan-accept-receipt')).to_be_visible()
    created = await page.evaluate("__lanWrites.find(w=>w.path==='/api/v1/projects/p1/rooms').input")
    assert created['host_mode'] == 'native' and created['sharing'] == 'lan' and created['owner_slot'] == 'slot2'
    assert set(created['agents']) == {'slot2'}, 'creation selected an invented runtime for the teammate'
    assert await page.locator('#lan-accept-receipt').input_value() == '', 'untrusted pending identity prefilled approval'
    await page.get_by_role('button', name='Create or copy current invitation', exact=True).click()
    await expect(page.locator('#lan-invite-output')).to_have_value("pairroom relay join 'pairroom://join/aW52aXRlLWZpeHR1cmU'")
    await page.get_by_role('button', name='Accept this receipt', exact=True).click()
    assert await page.evaluate("__lanWrites.filter(w=>w.path.endsWith('/accept')).length") == 0
    receipt = 'pairroom-accept:dHJ1c3RlZC1maXh0dXJl'
    await page.locator('#lan-accept-receipt').fill(receipt)
    await page.get_by_role('button', name='Accept this receipt', exact=True).click()
    await expect(page.get_by_role('button', name='Revoke teammate access', exact=True)).to_be_visible()
    assert await page.evaluate("__lanWrites.find(w=>w.path.endsWith('/accept')).input") == {'receipt':receipt}
    for width in (1440, 390):
        await page.set_viewport_size({'width':width,'height':1000})
        assert not await page.locator('#lan-room-dialog').evaluate('node=>node.scrollWidth>node.clientWidth'), f'LAN receipt/member overflow at {width}'
        await page.screenshot(path=str(artifacts / f'management-lan-access-{width}.png'))
    await page.locator('#lan-room-dialog [data-close-dialog]').last.click()
    await page.set_viewport_size({'width':1440,'height':1000})

    await page.evaluate("""() => {
      __snapshot.joined_rooms=[{id:'guest-local',remote_room_id:'host-room',name:'Teammate bug room',workspace:'/local/project',
        slot:'slot2',runtime:'codex',status:'accepted',connected:true,host_pin:'a'.repeat(64),endpoint:'https://192.168.1.23:8877',generation:1}];
      location.hash='#/settings/lan';
    }""")
    await page.locator('#refresh-button').click()
    joined = page.locator('#view .setting-row').filter(has=page.get_by_text('Teammate bug room', exact=True))
    await joined.get_by_role('button', name='Open', exact=True).click()
    evidence = page.locator('#lan-room-body a.lan-artifact')
    await expect(evidence).to_have_attribute('href','/api/v1/lan/joined/guest-local/attachments/evidence-id')
    await expect(evidence).to_contain_text('repro <img onerror=alert(1)>.txt')
    assert await page.locator('#lan-room-body img').count() == 0, 'evidence filename was interpreted as markup'
    await expect(page.locator('#lan-room-body .lan-shared-message strong')).to_have_text('Teammate')
    if not in_page_fixture:
        message_label = await page.evaluate("PairRoomI18n.t('room.native.message')")
        await page.get_by_role('textbox', name=message_label, exact=True).fill('Please reproduce this failure')
        await page.locator('#lan-room-body .primary-button').click()
        await wait_fixture_state(page, "Boolean(PairRoomNativeOutbox.load(localStorage,'guest-local'))")
        await expect(page.get_by_role('textbox', name=message_label, exact=True)).to_be_disabled()
        original = await page.evaluate("__lanWrites.find(w=>w.path.endsWith('/send')).input")
        assert await page.evaluate("PairRoomNativeOutbox.load(localStorage,'guest-local')") == original
        receipt_label = await page.evaluate("PairRoomI18n.t('room.native.checkReceipt')")
        await page.get_by_role('button', name=receipt_label, exact=True).click()
        await wait_fixture_state(page, "PairRoomNativeOutbox.load(localStorage,'guest-local')===null")
        assert await page.evaluate("__lanWrites.filter(w=>w.path.endsWith('/send')).length") == 1, 'receipt lookup repeated publication'
        assert await page.evaluate("__lanWrites.find(w=>w.path.endsWith('/receipt')).input") == {'id':original['id']}
        await expect(page.get_by_role('textbox', name=message_label, exact=True)).to_have_value('')
        assert await page.evaluate('__cspErrors') == [], 'LAN owner UI violated the production CSP'
    await page.set_viewport_size({'width':390,'height':1000})
    assert not await page.locator('#lan-room-dialog').evaluate('node=>node.scrollWidth>node.clientWidth'), 'joined-room dialog overflow'
    await page.screenshot(path=str(artifacts / 'management-lan-joined-mobile.png'))
    assert not errors, errors
    await page.close()
    return {'lan_owner_explicit_configuration':True, 'lan_native_owner_slot_only':True,
            'lan_receipt_explicit':True, 'lan_evidence_safe_download':True,
            'lan_publication_receipt_recovery':not in_page_fixture, 'lan_responsive':True}
