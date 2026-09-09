#!/usr/bin/env python3
"""Headless Room UI contract with deterministic in-page HTTP/SSE fixtures.

No vendor process, credentials, external page, or model tokens are used. Go and
Mock smoke tests cover the real HTTP/runtime boundary separately. Install the
pinned browser test dependency from requirements-browser.txt before running.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import re
from pathlib import Path

from playwright.async_api import async_playwright

ROOT = Path(__file__).resolve().parents[1]


def collaboration_fixture() -> dict:
    # Read the versioned default prose from its Go authority, not a second copy.
    source = (ROOT / "internal/model/collaboration.go").read_text(encoding="utf-8")
    match = re.search(r'DefaultCollaborationInstructions\s*=\s*("(?:[^"\\]|\\.)*")', source)
    assert match, "default instructions must remain a versioned constant"
    return {"version": 1, "mode": "default", "instructions": json.loads(match[1])}


def snapshot_fixture() -> dict:
    participants = {}
    for actor, name, responsibility in [("claude", "Claude Code", "lead"), ("codex", "Codex", "executor")]:
        participants[actor] = {
            "id": actor, "display_name": name, "mention_handle": "@" + actor,
            "role": "peer", "responsibility": responsibility, "permission_profile": "configured",
            "state": "idle", "session_id": "fixture-" + actor,
            "model": "deterministic-fixture", "runtime_kind": actor,
            "runtime": {"available": True, "command": "fixture", "protocol": "browser-fixture", "capabilities": [],
                        "session_name": f"Example workspace · @{actor} · 123456789abc",
                        "session_name_status": "configured" if actor == "claude" else "synced",
                        # One slot resolves a CC Switch Profile (internal reference label plus the
                        # redacted display name) and one stays native, so the card must name both.
                        **({"provider": "cc-switch:claude/6687bc5e-aaac-462d-bcc7-6d735f092f30",
                            "provider_name": "Example Provider", "effort": "high",
                            "permission_mode": "yolo"} if actor == "claude"
                           else {"provider": "native",
                                 "approval_policy": "yolo", "sandbox": "danger-full-access"})},
            "workspace": {"kind": "driver-live", "path": "/workspace/example", "read_only": False},
        }
    return {
        "meta": {"id": "browser-fixture", "name": "Example workspace", "repo": "/workspace/example", "collaboration": collaboration_fixture()},
        "settings": {"stall_warning_seconds": 300}, "participants": participants,
        "messages": [], "approvals": [], "turns": [], "latest_seq": 1,
        "message_window": {"total": 0, "loaded": 0, "has_more": False},
        "events": [{"seq": 0, "kind": "system.notice", "data": {"level": "info", "text": "Earlier native session history stays in the native harness. This Room starts at its binding boundary."}}],
    }


def fixture_html() -> str:
    html = (ROOT / "internal/server/assets/index.html").read_text(encoding="utf-8")

    def asset(src: str) -> str:
        directory = "internal/webui/assets" if "/_pairroom/" in src else "internal/server/assets"
        return (ROOT / directory / src.rsplit("/", 1)[-1]).read_text(encoding="utf-8")

    html = re.sub(r'<link\b[^>]*rel="icon"[^>]*>', '', html)
    html = re.sub(r'<link\b[^>]*rel="stylesheet"[^>]*href="([^"]+)"[^>]*>', lambda m: '<style>' + asset(m[1]) + '</style>', html)
    html = re.sub(r'<script\b[^>]*src="([^"]+)"[^>]*></script>', lambda m: '<script>' + asset(m[1]).replace('</script>', '<\\/script>') + '</script>', html)
    mock = r'''
      const memory = new Map();
      Object.defineProperty(window, 'localStorage', {value: {
        getItem: key => memory.get(key) || null,
        setItem: (key, value) => memory.set(key, String(value)), removeItem: key => memory.delete(key)
      }});
      window.__snapshot = SNAPSHOT;
      window.__sent = []; window.__sources = []; window.__snapshotRequests = 0;
      window.__postDelay = 250; window.__snapshotDelay = 0; window.__failPost = false;
      window.__approvalSent = []; window.__failApproval = false; window.__approvalGate = null; window.__permissionSent = [];
      window.fetch = async (path, options = {}) => {
        let body = {}, status = 200;
        if (path.includes('/session')) body = {csrf_token: 'fixture'};
        else if (path.includes('/snapshot')) {
          window.__snapshotRequests++;
          body = structuredClone(window.__snapshot);
          await new Promise(resolve => setTimeout(resolve, window.__snapshotDelay));
        } else if (path.endsWith('/messages') && options.method === 'POST') {
          window.__sent.push(JSON.parse(options.body));
          await new Promise(resolve => setTimeout(resolve, window.__postDelay));
          if (window.__failPost) { status = 503; body = {error: 'fixture: unavailable'}; }
        } else if (path.includes('/approvals/') && options.method === 'POST') {
          window.__approvalSent.push({path, ...JSON.parse(options.body)});
          if (window.__approvalGate) await window.__approvalGate;
          else await new Promise(resolve => setTimeout(resolve, window.__postDelay));
          if (window.__failApproval) { status = 400; body = {error: 'fixture: invalid choice'}; }
        } else if (path.endsWith('/permissions') && options.method === 'PUT') {
          const actor=path.split('/').at(-2), request=JSON.parse(options.body);
          __permissionSent.push({actor,...request});
          await new Promise(resolve => setTimeout(resolve, __postDelay));
          __snapshot.participants[actor].permission_profile=request.profile;
          __snapshot.participants[actor].runtime.sandbox=request.profile==='read-only'?'read-only':'danger-full-access';
        } else if (path.includes('/git/status')) body = {status: 'clean'};
        return new Response(JSON.stringify(body), {status, headers: {'content-type': 'application/json'}});
      };
      class FixtureEventSource extends EventTarget {
        constructor(url) {
          super(); this.url = url; this.closed = false; window.__sources.push(this);
          setTimeout(() => this.dispatchEvent(new Event('open')), 0);
        }
        close() { this.closed = true; }
      }
      window.EventSource = FixtureEventSource;
    '''.replace('SNAPSHOT', json.dumps(snapshot_fixture()).replace('</', '<\\/'))
    return html.replace('<head>', '<head><script>' + mock + '</script>', 1)


async def verify_approvals(browser, artifacts: Path) -> dict:
    page = await browser.new_page(viewport={"width": 1440, "height": 1000}, locale="en-US")
    page.set_default_timeout(5000)
    errors = []
    page.on("pageerror", lambda error: errors.append(str(error)))
    await page.set_content(fixture_html())
    await page.wait_for_selector("#connection.connected")
    await page.evaluate("""() => {
      __snapshot.participants.claude.runtime_kind='codex';
      __snapshot.participants.claude.display_name='Codex';
      __snapshot.participants.claude.mention_handle='@codex';
      __snapshot.participants.codex.runtime_kind='claude';
      __snapshot.participants.codex.display_name='Claude Code';
      __snapshot.participants.codex.mention_handle='@claude';
      __snapshot.approvals = [
        {id:'question-1',agent:'codex',kind:'claude.userQuestion',title:'Choose the review boundary',status:'pending',
         detail:{input:{questions:[{question:'What should remain native?',options:[{label:'Session ownership',description:'Keep the vendor session authoritative.'}]}]}}},
        {id:'command-1',agent:'claude',kind:'item/commandExecution/requestApproval',title:'Run focused tests',status:'pending',
         detail:{command:['go','test','./internal/agent/...'],cwd:'/workspace/example'}}
      ];
    }""")
    await page.locator("#refresh-button").click()
    await page.locator('[data-tab="approvals"]').first.click()
    field = page.locator(".question-other")
    await field.fill("Preserve native sessions and exact permission scope.")
    await page.locator('.question-option input').check()
    await page.locator('.approval-raw summary').click()
    await field.focus()
    await page.evaluate("window.__approvalField = document.activeElement")
    # Unrelated telemetry/approval events must not replace the native question DOM.
    await page.evaluate("""() => __sources.at(-1).dispatchEvent(new MessageEvent('pairroom', {
      data:JSON.stringify({seq:2,kind:'approval.updated',data:__snapshot.approvals[1]})}))""")
    await page.wait_for_timeout(120)
    assert await page.evaluate("document.activeElement === __approvalField && __approvalField.isConnected"), "approval refresh stole focus"
    await page.locator("#refresh-button").click()
    await page.wait_for_timeout(120)
    assert await field.input_value() == "Preserve native sessions and exact permission scope.", "snapshot cleared question draft"
    assert await page.locator('.question-option input').is_checked(), "snapshot cleared selected answer"
    assert await page.locator('.approval-raw').evaluate("node => node.open"), "snapshot collapsed native details"
    await page.wait_for_selector('[data-approval-card="command-1"] [data-decision="acceptForSession"]')
    summary = await page.locator('[data-approval-card="command-1"] .approval-summary').inner_text()
    assert 'go test ./internal/agent/...' in summary and '/workspace/example' in summary, "native command or cwd hidden from decision summary"
    for theme, language in [("light", "en"), ("dark", "zh-CN")]:
        await page.evaluate("args => {PairRoomTheme.setTheme(args[0]); PairRoomI18n.setLang(args[1]);}", [theme, language])
        await page.wait_for_timeout(120)
        assert await field.input_value() == "Preserve native sessions and exact permission scope.", "locale switch cleared the draft"
        assert await page.locator('#connection span:last-child').get_attribute('data-i18n') == 'room.live', "locale switch reset live status to Connecting"
        await page.screenshot(path=str(artifacts / f"approvals-{theme}-{language}.png"))
    # Form Enter is a controlled native answer, not a browser navigation. An IME
    # confirmation must not send it. Re-render while pending cannot unlock it.
    await field.evaluate("node => node.dispatchEvent(new KeyboardEvent('keydown', {key:'Enter',isComposing:true,bubbles:true,cancelable:true}))")
    assert await page.evaluate('__approvalSent.length') == 0
    # page.evaluate awaits a returned Promise; keep this assignment from completing as one.
    await page.evaluate("""() => {
      window.__failApproval = true;
      window.__approvalGate = new Promise((resolve) => { window.__releaseApproval = resolve; });
    }""")
    await field.press('Enter')
    await page.wait_for_function('__approvalSent.length === 1')
    await page.locator('#refresh-button').click()
    await page.wait_for_timeout(50)
    assert await page.locator('[data-question-submit]').is_disabled()
    await page.evaluate('__releaseApproval(); __approvalGate = null')
    await page.wait_for_function("!document.querySelector('[data-question-submit]').disabled")
    assert await field.input_value() == "Preserve native sessions and exact permission scope."
    await page.evaluate('__failApproval = false')
    await field.press('Enter')
    await page.wait_for_function('__approvalSent.length === 2')
    await page.wait_for_timeout(300)
    assert await page.locator('[data-question-submit]').is_disabled(), "accepted resolution unlocked before durable state"
    assert await page.locator('#connection.connected').count() == 1, "form submission navigated away"
    # Native Grok labels disambiguate two choices with the same kind. Slot identity
    # and locale must not replace these vendor-provided options with generic grants.
    await page.evaluate("""() => {
      __snapshot.participants.claude.runtime_kind='grok';
      __snapshot.participants.claude.display_name='Grok Build';
      __snapshot.approvals=[{id:'grok-1',agent:'claude',kind:'grok.permission',status:'pending',title:'Select native execution mode',
        detail:{toolCall:{title:'Run checks',rawInput:{command:'go test ./...'}},options:[
          {optionId:'manual',name:'Allow once, inspect each edit',kind:'allow_once'},
          {optionId:'automatic',name:'Allow once, apply this batch',kind:'allow_once'},
          {optionId:'remember',name:'Remember this permission',kind:'allow_always'},
          {optionId:'deny',name:'Reject this request',kind:'reject_once'}]}}];
    }""")
    await page.locator('#refresh-button').click()
    choice = page.get_by_role('button', name='Allow once, apply this batch', exact=True)
    await choice.wait_for()
    assert await page.locator('[data-decision="acceptForSession"]').count() == 0, "invented session scope for Grok"
    await page.set_viewport_size({"width": 390, "height": 844})
    # The Inspector is a mobile drawer; expose the real approvals tab.
    await page.locator('#ux-layout-button').click()
    await page.locator('[data-ux-action="inspector"]').click()
    await page.wait_for_timeout(100)
    await page.screenshot(path=str(artifacts / 'approvals-native-mobile-dark.png'))
    assert not await page.evaluate("document.documentElement.scrollWidth > innerWidth"), "native option label caused horizontal overflow"
    await page.set_viewport_size({"width": 1440, "height": 1000})
    await choice.click()
    await page.wait_for_function('__approvalSent.length === 3')
    assert await page.evaluate('__approvalSent.at(-1).decision') == 'option:automatic'
    await page.wait_for_timeout(300)
    assert await choice.is_disabled(), "native permission could be sent twice"
    assert not errors, errors
    await page.close()
    return {"approval_draft_preserved": True, "native_option_identity": True, "approval_single_submission": True, "approval_page_errors": errors}


async def verify_collaboration(browser, artifacts: Path) -> dict:
    page = await browser.new_page(viewport={"width": 1440, "height": 1000}, locale="en-US")
    page.set_default_timeout(5000)
    errors = []
    page.on("pageerror", lambda error: errors.append(str(error)))
    await page.set_content(fixture_html())
    await page.wait_for_selector("#connection.connected")
    assert await page.locator("[data-role-actor]").count() == 0
    assert await page.locator(".target-button").evaluate_all("nodes=>nodes.map(n=>n.dataset.target)") == ["claude", "codex"]
    assert "lead" in (await page.locator("#participants").inner_text()).lower()
    assert "executor" in (await page.locator("#participants").inner_text()).lower()
    await page.locator("#room-collaboration summary").click()
    assert await page.locator("#room-collaboration-instructions").text_content() == collaboration_fixture()["instructions"]
    assert await page.locator("[data-permission-actor=codex]").evaluate("node=>{const s=getComputedStyle(node); return parseFloat(s.borderTopLeftRadius)>0 && parseFloat(s.paddingLeft)>=8;}"), "permission control lost shared form styling"
    assert await page.locator('.runtime-session-name').evaluate_all('nodes=>nodes.map(n=>n.textContent)') == ['Example workspace · @claude · 123456789abc', 'Example workspace · @codex · 123456789abc']
    statuses = await page.locator('.runtime-name-status').evaluate_all('nodes=>nodes.map(n=>n.textContent)')
    assert len(set(statuses)) == 2, 'configured CLI label falsely rendered as an acknowledged sync'
    assert 'fixture-claude' in await page.locator('#participants').inner_text() or await page.locator('#participants [title="fixture-claude"]').count() > 0, 'native ID lost behind the display name'
    # A CC Switch Provider is named by its redacted Profile display name; the
    # internal reference label stays reachable as a tooltip instead of vanishing.
    participants_text = await page.locator('#participants').inner_text()
    assert 'Example Provider' in participants_text, 'CC Switch Profile display name was not shown'
    assert 'cc-switch:claude' not in participants_text, 'internal Provider reference label rendered as display text'
    assert await page.locator('#participants [title="cc-switch:claude/6687bc5e-aaac-462d-bcc7-6d735f092f30"]').count() >= 1, 'internal Provider reference lost from the tooltip'
    assert 'Native / Runtime default' in participants_text, 'native Provider was not named'
    assert 'Effort: high' in participants_text, 'effort was not surfaced on the Agent card'
    assert await page.locator('.participant-card .slot-badge').evaluate_all('nodes=>nodes.map(n=>n.textContent)') == ['Agent 1', 'Agent 2'], 'durable slot identity was not visible'
    assert await page.locator('.participants-panel').evaluate('node=>node.scrollWidth<=node.clientWidth'), 'Agent card burst the fixed-width participants panel'
    # An error gets its own region so a broken runtime keeps its identification.
    await page.evaluate("()=>{__snapshot.participants.claude.last_error='native runtime failed: '+'x'.repeat(400);}")
    await page.locator('#refresh-button').click()
    await page.wait_for_function("document.querySelectorAll('.participant-error').length===1")
    errored_text = await page.locator('#participants').inner_text()
    assert 'fixture-claude' in errored_text, 'a runtime error replaced the participant identity'
    assert await page.locator('.participant-error').first.evaluate('node=>node.title.length>node.textContent.length'), 'long error was truncated without keeping the full text reachable'
    assert await page.locator('.participants-panel').evaluate('node=>node.scrollWidth<=node.clientWidth'), 'a long runtime error burst the participants panel'
    await page.evaluate("()=>{delete __snapshot.participants.claude.last_error;}")
    await page.locator('#refresh-button').click()
    await page.wait_for_function("document.querySelectorAll('.participant-error').length===0")
    await page.screenshot(path=str(artifacts / "collaboration-default-light.png"))
    # Two DOM change events during one pending PUT still have one mutation owner.
    await page.locator("[data-permission-actor=codex]").evaluate("""node=>{
      node.value='read-only'; node.dispatchEvent(new Event('change',{bubbles:true}));
      node.dispatchEvent(new Event('change',{bubbles:true}));
    }""")
    await page.wait_for_function("__permissionSent.length===1 && document.querySelector('[data-permission-actor=codex]').value==='read-only' && !document.querySelector('[data-permission-actor=codex]').disabled")
    assert await page.evaluate("__permissionSent.length") == 1
    assert "executor" in (await page.locator("#participants").inner_text()).lower(), "permission change replaced responsibility"
    custom = "Agent 2 proposes a plan; Agent 1 implements. Ask the user before deployment."
    await page.evaluate("text=>{__snapshot.meta.collaboration={version:1,mode:'custom',instructions:text};Object.values(__snapshot.participants).forEach(p=>p.responsibility='participant');}", custom)
    await page.locator("#refresh-button").click()
    await page.wait_for_function("document.getElementById('room-collaboration-label').textContent==='Custom instructions'")
    assert await page.locator("#room-collaboration-instructions").text_content() == custom
    await page.evaluate("PairRoomI18n.setLang('zh-CN'); PairRoomTheme.setTheme('dark')")
    await page.set_viewport_size({"width": 390, "height": 844})
    await page.wait_for_timeout(100)
    assert await page.locator("#room-collaboration-instructions").text_content() == custom
    assert not await page.evaluate("document.documentElement.scrollWidth>innerWidth")
    await page.screenshot(path=str(artifacts / "collaboration-custom-mobile-dark.png"))
    # Legacy state is visible but cannot silently opt into new permissions.
    await page.evaluate("delete __snapshot.meta.collaboration")
    await page.locator("#refresh-button").click()
    await page.wait_for_function("document.querySelectorAll('[data-permission-actor]').length===0")
    assert await page.locator("[data-role-actor]").count() == 0
    assert not errors, errors
    await page.close()
    return {"creation_only_mode_display": True, "permission_single_submission": True,
            "responsibility_not_permission": True, "permission_control_styled": True, "custom_instructions_verbatim": True,
            "provider_display_name_not_reference": True, "slot_identity_visible": True, "error_does_not_replace_identity": True,
            "collaboration_page_errors": errors}


async def verify_activity(browser, artifacts: Path) -> dict:
    """The renderer, rather than SSE, owns identity and unfinished reading."""
    page = await browser.new_page(viewport={"width": 1440, "height": 1000})
    page.set_default_timeout(10000)
    errors = []
    page.on('pageerror', lambda error: errors.append(str(error)))
    snap = snapshot_fixture()
    snap['turns'] = [
        {'id': f'claude:identical-truncated-prefix-{i}', 'turn_id': f'identical-truncated-prefix-{i}',
         'agent': 'claude', 'status': 'completed', 'updated_at': f'2026-01-01T12:{59-i:02}:00Z',
         'plan': f'Plan {i}: preserve the complete native evidence and the user reading position.',
         'items': [{'id': f'command-{i}', 'kind': 'command', 'status': 'completed', 'name': 'go test ./...',
                    'detail': 'Verified evidence\n' + ('Long output remains inspectable.\n' * 12), 'data': {'exit_code': 0}}]}
        for i in range(24)
    ]
    html = fixture_html().replace(json.dumps(snapshot_fixture()).replace('</', '<\\/'), json.dumps(snap).replace('</', '<\\/'))
    await page.set_content(html)
    await page.wait_for_selector('.turn-card')
    first = page.locator('[data-turn-id="claude:identical-truncated-prefix-0"]')
    second = page.locator('[data-turn-id="claude:identical-truncated-prefix-1"]')
    await first.locator(':scope > summary').click()
    await first.locator('[data-section="plan"] > summary').click()
    await first.locator('.turn-item > summary').click()
    assert not await second.evaluate('node => node.open'), 'abbreviated IDs shared state'
    await page.evaluate("""() => {
      window.__activityCard = document.querySelector('.turn-card');
      window.__activityText = __activityCard.querySelector('[data-section="plan"] pre').firstChild;
      window.__activityFocus = __activityCard.querySelector('.turn-item > summary');
      __activityFocus.focus();
      getSelection().setBaseAndExtent(__activityText, 0, __activityText, 6);
      window.__emit = (kind, data) => {
        __snapshot.latest_seq++;
        __sources.at(-1).dispatchEvent(new MessageEvent('pairroom', {data:JSON.stringify({
          seq:__snapshot.latest_seq, id:'event-'+__snapshot.latest_seq, kind, data})}));
      };
      __emit('runtime.event',{kind:'log',agent:'codex',text:'Unrelated native event'});
    }""")
    await page.wait_for_timeout(200)
    assert await page.evaluate('document.querySelector(".turn-card") === __activityCard && __activityText.isConnected')
    assert await page.evaluate('document.activeElement === __activityFocus && getSelection().toString() === "Plan 0"'), 'event stole focus or selection'
    # A logically identical new snapshot must retain the actual nodes.
    await page.evaluate("document.getElementById('refresh-button').click()")
    await page.wait_for_timeout(200)
    assert await page.evaluate('document.querySelector(".turn-card") === __activityCard && getSelection().toString() === "Plan 0"')
    # Updating another section must preserve open Plan/tool sections.
    await page.evaluate("""() => {const value={...__snapshot.turns[0],usage:{tokens:42}};
      __snapshot.turns[0]=value; __emit('turn.summary.updated',value); }""")
    await page.wait_for_selector('[data-section="usage"]')
    assert await first.locator('[data-section="plan"]').evaluate('node => node.open')
    assert await first.locator('.turn-item').evaluate('node => node.open')
    # A hidden tab does no DOM work; returning renders the pending state once.
    await page.locator('[data-tab="diff"]').click()
    await page.evaluate("""() => {window.__activityMutations=0;
      window.__observer=new MutationObserver(records => __activityMutations+=records.length);
      __observer.observe(document.getElementById('activity-tab'),{childList:true,subtree:true,characterData:true});
      const value={...__snapshot.turns[0],final_text:'Final evidence after hidden update'};
      __snapshot.turns[0]=value; __emit('turn.summary.updated',value); }""")
    await page.wait_for_timeout(200)
    assert await page.evaluate('__activityMutations') == 0, 'hidden Inspector rebuilt DOM'
    await page.locator('[data-tab="activity"]').click()
    await page.wait_for_selector('[data-section="final"]')
    await page.evaluate('__observer.disconnect(); getSelection().removeAllRanges()')
    # Scroll away from the top, reorder two offscreen cards, retain visible anchor.
    # Keep a viewport of slack below the anchor: at the bottom, reordering
    # cannot preserve an anchor beyond the new scroll range on compact fonts.
    await page.locator('#activity-tab').evaluate('node => node.scrollTop=Math.min(700, (node.scrollHeight-node.clientHeight)/2)')
    assert await page.locator('#activity-tab').evaluate('node => node.scrollTop > 0 && node.scrollHeight-node.clientHeight-node.scrollTop > 160')
    anchor = """() => {const el=document.getElementById('activity-tab'),top=el.getBoundingClientRect().top;
      const node=[...el.children].find(n=>n.dataset.activityKey && n.getBoundingClientRect().bottom>top);
      return {key:node.dataset.activityKey,offset:node.getBoundingClientRect().top-top};}"""
    before = await page.evaluate(anchor)
    await page.evaluate("""() => {const value={...__snapshot.turns[20],updated_at:'2026-01-02T00:00:00Z'};
      __snapshot.turns[20]=value; __emit('turn.summary.updated',value); }""")
    await page.wait_for_timeout(200)
    await page.wait_for_function("() => document.querySelector('#activity-tab .turn-card')?.dataset.turnId === 'claude:identical-truncated-prefix-20'")
    after = await page.evaluate(anchor)
    assert before['key'] == after['key'] and abs(before['offset']-after['offset']) < 2, (before, after)
    await page.locator('#activity-tab').evaluate('node => node.scrollTop=0')
    await page.screenshot(path=str(artifacts / 'inspector-light-en.png'))
    await page.evaluate("() => {PairRoomI18n.setLang('zh-CN'); PairRoomTheme.setTheme('dark');}")
    await page.set_viewport_size({'width':390,'height':844})
    await page.locator('#ux-layout-button').click()
    await page.locator('[data-ux-action="inspector"]').click()
    await page.wait_for_timeout(150)
    assert not await page.evaluate('document.documentElement.scrollWidth > innerWidth'), 'Inspector causes viewport overflow'
    await page.screenshot(path=str(artifacts / 'inspector-mobile-dark.png'))
    assert not errors, errors
    await page.close()
    return {'inspector_full_id_keys':True,'inspector_nodes_focus_selection_retained':True,
            'inspector_hidden_render_deferred':True,'inspector_scroll_anchor':True,'inspector_page_errors':errors}


async def verify(browser_path: str | None, artifacts: Path) -> None:
    artifacts.mkdir(parents=True, exist_ok=True)
    results = {}
    async with async_playwright() as playwright:
        options = {"headless": True}
        if browser_path:
            options["executable_path"] = browser_path
        browser = await playwright.chromium.launch(**options)
        page = await browser.new_page(viewport={"width": 1440, "height": 1000}, locale="en-US")
        page.set_default_timeout(5000)
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        await page.set_content(fixture_html())
        await page.wait_for_selector("#connection.connected")
        assert await page.locator(".timeline-empty").count() == 1, "boundary notice suppressed onboarding"
        await page.screenshot(path=str(artifacts / "room-light.png"))
        # All optional rows must stay content-sized, not steal the flexible timeline.
        await page.evaluate("""() => { for (const id of ['turn-owner-bar','timeline-scope','reply-banner']) {
            const element = document.getElementById(id);
            element.classList.remove('hidden');
            const label = element.querySelector('#timeline-scope-text, #reply-preview');
            if (label) label.textContent = 'Active Turn / thread / reply';
            else element.textContent = 'Active Turn / thread / reply';
        }}""")
        timeline_box = await page.locator("#timeline").bounding_box()
        owner_box = await page.locator("#turn-owner-bar").bounding_box()
        assert timeline_box["height"] > 350 and owner_box["height"] < 80, "optional rows displaced conversation"
        results["active_timeline_height"] = timeline_box["height"]
        results["active_owner_height"] = owner_box["height"]
        await page.screenshot(path=str(artifacts / "room-active-rows.png"))
        await page.evaluate("""() => { for (const id of ['turn-owner-bar','timeline-scope','reply-banner'])
            document.getElementById(id).classList.add('hidden'); }""")
        input_field = page.locator("#message-input")
        # Browser-side IME contract: synthetic composition cannot verify the OS
        # candidate popup, but it can catch layout churn and focus/selection loss.
        await input_field.fill("draft " * 200)
        focus_state = await input_field.evaluate("""element => {
          element.focus(); element.setSelectionRange(4, 12, 'backward');
          element.scrollTop = 25;
          const before = element.scrollTop;
          let blurs = 0;
          element.addEventListener('blur', () => blurs++, {once:true});
          window.dispatchEvent(new Event('focus'));
          return {blurs, focused: document.activeElement === element,
            start:element.selectionStart, end:element.selectionEnd,
            direction:element.selectionDirection, scroll:element.scrollTop, before};
        }""")
        assert focus_state == dict(blurs=1, focused=True, start=4, end=12,
                                   direction="backward", scroll=focus_state["before"], before=focus_state["before"]), focus_state
        preedit = await input_field.evaluate("""async element => {
          const before = element.getBoundingClientRect().toJSON();
          const observer = new MutationObserver(() => {});
          observer.observe(element, {attributes:true, attributeFilter:['style']});
          element.dispatchEvent(new CompositionEvent('compositionstart', {data:'ni'}));
          element.value = 'ni';
          element.dispatchEvent(new InputEvent('input', {isComposing:true, inputType:'insertCompositionText', data:'ni'}));
          window.dispatchEvent(new Event('focus'));
          const changes = observer.takeRecords().length;
          observer.disconnect();
          const after = element.getBoundingClientRect().toJSON();
          element.dispatchEvent(new CompositionEvent('compositionend', {data:'你'}));
          return {changes, before, after, heightAfterCommit:element.getBoundingClientRect().height};
        }""")
        assert preedit["changes"] == 0 and preedit["before"] == preedit["after"], preedit
        assert preedit["heightAfterCommit"] < preedit["before"]["height"], preedit
        await page.locator('#message-search').focus()
        await page.evaluate("window.dispatchEvent(new Event('focus'))")
        assert await page.locator('#message-search').evaluate('element => document.activeElement === element')
        results["ime_layout_and_focus_preserved"] = True
        await input_field.fill("中文输入确认")
        await input_field.evaluate("element => element.dispatchEvent(new KeyboardEvent('keydown', {key:'Enter', isComposing:true, bubbles:true, cancelable:true}))")
        assert await page.evaluate("__sent.length") == 0, "IME confirmation sent a message"
        await input_field.fill("first message")
        await input_field.press("Enter")
        await input_field.press("Enter")
        await input_field.fill("next draft while sending")
        await page.wait_for_function("document.getElementById('send-button').getAttribute('aria-busy') === 'false'")
        assert await page.evaluate("__sent.length") == 1, "duplicate native submission"
        assert await input_field.input_value() == "next draft while sending", "new draft was lost"
        # A failed POST is not automatically retried and does not destroy edits.
        await page.evaluate("__failPost = true")
        await input_field.press("Enter")
        await page.wait_for_function("document.getElementById('send-button').getAttribute('aria-busy') === 'false'")
        assert await input_field.input_value() == "next draft while sending"
        assert await page.evaluate("__sent.length") == 2
        await page.evaluate("__failPost = false")
        # A burst from an obsolete source must cause one read, not N competing snapshots.
        sources = await page.evaluate("__sources.length")
        reads = await page.evaluate("__snapshotRequests")
        await page.evaluate("""() => {
          __snapshotDelay = 100; __snapshot.latest_seq = 100;
          const old = __sources.at(-1);
          for (let i=0; i<40; i++) old.dispatchEvent(new MessageEvent('pairroom', {data: JSON.stringify({seq: 3+i, kind:'system.notice',data:{text:'gap'}})}));
        }""")
        await page.wait_for_function("count => __sources.length > count", arg=sources)
        assert await page.evaluate("__snapshotRequests") == reads + 1, "resynchronization storm"
        assert await input_field.input_value() == "next draft while sending"
        # Render a long transcript, scroll back, and refresh without losing the visible anchor.
        await page.evaluate("""() => {
          __snapshotDelay = 0;
          __snapshot.messages = Array.from({length:80}, (_,i) => ({id:'m'+i,seq:i+2,from:'user',to:['claude'],
            text:'Message '+i+'\\n'+('Visible transcript content. '.repeat(15)),created_at:'2026-01-01T12:00:00Z'}));
          __snapshot.message_window = {total:80,loaded:80,has_more:false};
        }""")
        await page.locator("#refresh-button").click()
        await page.wait_for_selector('[data-message-id="m79"]')
        await page.wait_for_timeout(100)
        await page.locator("#timeline").evaluate("element => { element.style.scrollBehavior='auto'; element.scrollTop=600; }")
        await page.wait_for_timeout(100)
        anchor = """() => {const t=document.getElementById('timeline'), top=t.getBoundingClientRect().top;
          const row=[...t.querySelectorAll('[data-message-id]')].find(r=>r.getBoundingClientRect().bottom>top);
          return {id:row.dataset.messageId, offset:row.getBoundingClientRect().top-top};}"""
        before = await page.evaluate(anchor)
        sources = await page.evaluate("__sources.length")
        await page.locator("#refresh-button").click()
        await page.wait_for_function("count => __sources.length > count", arg=sources)
        await page.wait_for_timeout(100)
        after = await page.evaluate(anchor)
        assert before["id"] == after["id"] and abs(before["offset"] - after["offset"]) < 2, f"scroll anchor moved: {before} -> {after}"
        results["scroll_anchor_preserved"] = True
        # Fault injection is complete; keep theme evidence free of expected-error overlays.
        await page.locator('.toast-close').evaluate_all("buttons => buttons.forEach(button => button.click())")
        for theme, language in [("light", "en"), ("dark", "zh-CN")]:
            await page.evaluate("args => {PairRoomTheme.setTheme(args[0]); PairRoomI18n.setLang(args[1]);}", [theme, language])
            await page.wait_for_timeout(100)
            await page.screenshot(path=str(artifacts / f"room-{theme}-{language}.png"))
            await page.set_viewport_size({"width": 390, "height": 844})
            await page.wait_for_timeout(100)
            assert not await page.evaluate("document.documentElement.scrollWidth > innerWidth"), "mobile horizontal overflow"
            await page.screenshot(path=str(artifacts / f"room-mobile-{theme}-{language}.png"))
            await page.set_viewport_size({"width": 1440, "height": 1000})
        results.update(await verify_approvals(browser, artifacts))
        results.update(await verify_collaboration(browser, artifacts))
        results.update(await verify_activity(browser, artifacts))
        assert not errors, errors
        results.update(ime_submissions=0, rapid_enter_submissions=1, retained_draft=True,
                       resync_reads_per_burst=1, page_errors=errors)
        (artifacts / "results.json").write_text(json.dumps(results, indent=2) + "\n", encoding="utf-8")
        print(json.dumps(results, indent=2))
        await browser.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--browser", default=os.environ.get("PAIRROOM_BROWSER_EXECUTABLE"))
    parser.add_argument("--artifacts", type=Path, default=ROOT / ".browser-results")
    arguments = parser.parse_args()
    asyncio.run(verify(arguments.browser, arguments.artifacts))
