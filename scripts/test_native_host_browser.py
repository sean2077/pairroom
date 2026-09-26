#!/usr/bin/env python3
"""Native-host IM presentation and real HTTP/CLI/browser recovery checks.

The IM check uses a snapshot fixture; transport checks exercise the shipped
binary with synthetic hook inputs. Neither proves real vendor E2E or model
acceptance. No tokens/credentials exported.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import subprocess
import tempfile
from pathlib import Path

from playwright.async_api import async_playwright, expect
from test_service_browser import ROOT, Service, read_json, wait_snapshot
from test_native_chat_browser import verify as verify_chat


async def verify(binary: Path | None, browser_path: str | None, artifacts: Path) -> None:
    artifacts.mkdir(parents=True, exist_ok=True)
    await verify_chat(browser_path, artifacts/'im')
    with tempfile.TemporaryDirectory(prefix='pairroom-native-browser-') as temporary:
        root = Path(temporary)
        if binary is None:
            binary = root / ('pairroom.exe' if os.name == 'nt' else 'pairroom')
            subprocess.run(['go', 'build', '-o', str(binary), './cmd/pairroom'], cwd=ROOT, check=True)
        binary = binary.resolve()
        repo = root / 'project'
        repo.mkdir()
        subprocess.run(['git', 'init', '-q', str(repo)], check=True)
        subprocess.run(['git', '-C', str(repo), '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.test', 'commit', '--allow-empty', '-qm', 'Initial review fixture'], check=True)
        service = Service(binary, root)
        env = os.environ.copy()
        env.update(HOME=str(root/'home'), USERPROFILE=str(root/'home'), XDG_CONFIG_HOME=str(root/'home'/'.config'))
        env['PATH'] = str(binary.parent) + os.pathsep + env.get('PATH', '')
        # bind associates from the official session id the harness exposes to its
        # tool-call subprocess. Expose exactly one per invocation (clearing the
        # others) so caller resolution never sees conflicting metadata; hooks match
        # by payload session_id and explicit-flag commands need none.
        session_vars = ('CLAUDE_CODE_SESSION_ID', 'CODEX_SESSION_ID', 'GROK_SESSION_ID')
        # Never inherit the surrounding (real) harness session into the fixture;
        # cli() exposes exactly one synthetic id per bind, and direct subprocess
        # calls then run with no session metadata so explicit flags win.
        for key in (*session_vars, 'CLAUDE_CONFIG_DIR', 'CODEX_HOME', 'GROK_HOME'):
            env[key] = ''
        errors = []

        def cli(args, payload=None, session=None, expect_error=None):
            call_env = dict(env)
            for key in session_vars:
                call_env[key] = ''
            if session:
                call_env[session[0]] = session[1]
            result = subprocess.run([str(binary), 'relay', *args, '--repo', str(repo)],
                                    input=payload if isinstance(payload, str) else None if payload is None else json.dumps(payload),
                                    text=True, capture_output=True, env=call_env, cwd=repo, timeout=45)
            if expect_error is not None:
                assert result.returncode != 0 and not result.stdout and expect_error in result.stderr, result
                return result.stderr
            assert result.returncode == 0, f'relay {args[0]} failed: {result.stderr[:1000]}'
            return result.stdout

        async with async_playwright() as p:
            options = {'headless': True}
            if browser_path:
                options['executable_path'] = browser_path
            browser = await p.chromium.launch(**options)
            try:
                management_url = await service.start()
                origin = management_url.split('/#', 1)[0].rstrip('/')
                context = await browser.new_context(viewport={'width':1440,'height':1050}, locale='en-US', reduced_motion='reduce')
                context.set_default_timeout(20000)
                page = await context.new_page()
                page.on('pageerror', lambda error: errors.append(str(error)))
                try:
                    await page.goto(management_url)
                except Exception:
                    # Browser policy/navigation errors can echo the bootstrap
                    # fragment. Never export its temporary bearer token.
                    raise RuntimeError('Management bootstrap navigation failed; browser access is required') from None
                await expect(page.locator('#app')).to_be_visible()
                await page.locator('#add-project-button').click()
                await page.locator('#project-path').fill(str(repo))
                await page.locator('#project-submit').click()
                await expect(page.locator('#project-dialog')).not_to_be_visible()
                await page.get_by_role('button', name='Create Room', exact=False).first.click()
                await page.locator('#room-name').fill('Native collaboration workspace')
                await page.locator('#room-host-mode').select_option('native')
                await expect(page.locator('#room-native-help')).to_be_visible()
                await expect(page.locator('input[name="slot1-mode"][value="existing"]')).to_be_disabled()
                for slot in ('slot1', 'slot2'):
                    await expect(page.locator(f'#{slot}-runtime option[value="grok"]')).to_be_enabled()
                    await page.locator(f'#{slot}-runtime').select_option('grok')
                await page.locator('#room-host-mode').select_option('embedded')
                await page.locator('#room-host-mode').select_option('native')
                for slot, runtime in (('slot1', 'claude'), ('slot2', 'codex')):
                    await expect(page.locator(f'#{slot}-runtime')).to_have_value('grok')
                    await page.locator(f'#{slot}-runtime').select_option(runtime)
                async with page.expect_response(lambda r: r.url.endswith('/rooms') and r.request.method == 'POST') as created:
                    await page.locator('#room-submit').click()
                creation = await created.value
                assert creation.status == 201, f'Native Room creation: {creation.status}'
                room = await creation.json()
                assert room['host_mode'] == 'native' and all(b['pending'] for b in room['bindings'].values())
                room_id = room['id']
                await page.locator(f'.tree-room[data-room-id="{room_id}"]').click()
                frame = page.frame_locator(f'#room-stage [data-room-id="{room_id}"] iframe')
                await expect(frame.locator('#messages-title')).to_have_text('Relay messages')
                assert await frame.get_by_role('button', name='Interrupt', exact=False).count() == 0
                surface = origin + f'/api/v1/rooms/{room_id}/surface'
                snapshot_url = surface + '/api/v1/snapshot'
                csrf = (await read_json(context, origin+'/api/v1/session'))['csrf_token']
                headers = {'X-PairRoom-CSRF': csrf}
                endpoint = root/'state'/'relay-endpoint.json'
                print('Native browser: approved-hook setup fixture and bind-time environment association through real CLI', flush=True)
                for slot, runtime, svar in (('slot1', 'claude', 'CLAUDE_CODE_SESSION_ID'), ('slot2', 'codex', 'CODEX_SESSION_ID')):
                    await asyncio.to_thread(cli, ['install','--runtime',runtime])
                    raw = await asyncio.to_thread(cli, ['bind','--room',room_id,'--slot',slot,'--service-file',str(endpoint)], None, (svar, 'synthetic-'+slot))
                    bound = json.loads(raw)
                    # bind associates immediately from the harness environment; no
                    # nonce echo round-trip is required or returned.
                    assert bound['binding']['session_id'] == 'synthetic-'+slot, bound['binding']
                    assert 'bind_nonce' not in bound, bound
                    response = await context.request.post(surface+f'/api/v1/participants/{slot}/park', headers=headers, data={'enabled':False})
                    assert response.status == 200
                snapshot = await wait_snapshot(context,snapshot_url,lambda s:all(b.get('session_id') for b in s['relay']['bindings'].values()))
                assert snapshot['protocol'] == 'pairroom-protocol/v8'
                await expect(frame.locator('[data-slot="slot1"]')).to_contain_text('Associated')
                # UI -> durable inbox -> CLI stdout -> explicit ack, with no model
                # acceptance fiction. A draft survives independent SSE messages.
                await frame.locator('#message-text').fill('User input for a native session')
                await frame.locator('#send').click()
                snapshot = await wait_snapshot(context,snapshot_url,lambda s:len(s['relay']['messages']) == 1)
                assert snapshot['relay']['messages'][0]['state'] == 'queued'
                received = await asyncio.to_thread(cli,['wait','--room',room_id,'--slot','slot1','--timeout','1'])
                assert 'User input for a native session' in received
                await wait_snapshot(context,snapshot_url,lambda s:s['relay']['messages'][0]['state']=='handed_off')
                await frame.locator('#message-text').fill('Preserve this unsent draft')
                await asyncio.to_thread(cli,['send','--room',room_id,'--slot','slot1','--id','explicit-fixture','--text','@user is body text: explicit send still targets the peer'])
                await expect(frame.locator('#message-text')).to_have_value('Preserve this unsent draft')
                snapshot = await wait_snapshot(context,snapshot_url,lambda s:len(s['relay']['messages']) == 2)
                assert snapshot['relay']['messages'][1]['to']=='slot2'
                await asyncio.to_thread(cli,['send','--room',room_id,'--slot','slot1','--id','explicit-fixture','--text','@user is body text: explicit send still targets the peer'])
                await asyncio.to_thread(cli,['send','--room',room_id,'--slot','slot1','--id','explicit-fixture','--text','changed body must not reuse the ID'], expect_error='was already used for a different message')
                assert len((await read_json(context,snapshot_url))['relay']['messages']) == 2
                await asyncio.to_thread(cli,['wait','--room',room_id,'--slot','slot2','--timeout','1'])

                print('Native browser: real CLI killed after durable claim, unknown + explicit Retry dialog', flush=True)
                # A pipe with no reader blocks the large stdout write. Killing
                # there must never acknowledge delivery or automatically replay.
                raw = await asyncio.to_thread(cli,['send','--room',room_id,'--slot','slot2','--id','kill-fixture'],'large reply\n'+('x'*(96<<10)))
                interrupted_id = json.loads(raw)['published']
                waiting = subprocess.Popen([str(binary),'relay','wait','--repo',str(repo),'--room',room_id,'--slot','slot1','--timeout','1'],
                                           env=env,cwd=repo,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
                try:
                    await wait_snapshot(context,snapshot_url,lambda s:any(m['id']==interrupted_id and m['state']=='delivering' for m in s['relay']['messages']))
                    waiting.kill()
                    await asyncio.to_thread(waiting.communicate, timeout=5)
                finally:
                    if waiting.poll() is None:
                        waiting.kill()
                        await asyncio.to_thread(waiting.communicate, timeout=5)
                await wait_snapshot(context,snapshot_url,lambda s:any(m['id']==interrupted_id and m['state']=='unknown' for m in s['relay']['messages']))
                original = frame.locator(f'[data-message-id="{interrupted_id}"]')
                await original.locator('[data-action="retry"]').click()
                await expect(frame.locator('#confirm-dialog')).to_be_visible()
                await frame.locator('#confirm-dialog button[value="cancel"]').click()
                await expect(frame.locator('#confirm-dialog')).not_to_be_visible()
                await original.locator('[data-action="retry"]').click()
                await frame.locator('#confirm-retry').click()
                snapshot = await wait_snapshot(context,snapshot_url,lambda s:any(m.get('retry_of')==interrupted_id for m in s['relay']['messages']))
                retried = next(m for m in snapshot['relay']['messages'] if m.get('retry_of')==interrupted_id)
                assert retried['id']!=interrupted_id and retried['state']=='queued'
                await frame.locator(f'[data-message-id="{retried["id"]}"] [data-action="cancel"]').click()
                await wait_snapshot(context,snapshot_url,lambda s:any(m['id']==retried['id'] and m['state']=='cancelled' for m in s['relay']['messages']))
                # Both actual Stop process calls park concurrently and exchange
                # replies. This is synthetic hook-input E2E, NOT native model E2E.
                print('Native browser: multi-round bidirectional Stop processes, no fetch/SSE mocks', flush=True)
                for slot in ('slot1','slot2'):
                    response = await context.request.post(surface+f'/api/v1/participants/{slot}/park',headers=headers,data={'enabled':True})
                    assert response.status == 200
                for round_no in range(3):
                    def hook(runtime,peer,slot):
                        return {'hook_event_name':'Stop','session_id':'synthetic-'+slot,'cwd':str(repo),
                                'last_assistant_message':f'@{peer} complete boundary reply {round_no} from {runtime}',
                                'stop_hook_active':round_no>0}
                    left,right = await asyncio.gather(
                        asyncio.to_thread(cli,['hook','--runtime','claude'],hook('claude','codex','slot1')),
                        asyncio.to_thread(cli,['hook','--runtime','codex'],hook('codex','claude','slot2')))
                    assert json.loads(left)['decision'] == json.loads(right)['decision'] == 'block'
                    assert f'reply {round_no} from codex' in json.loads(left)['reason']
                    assert f'reply {round_no} from claude' in json.loads(right)['reason']
                await asyncio.to_thread(cli,['send','--room',room_id,'--slot','slot1','--id','restart-fixture','--text','Queued across Service restart'])
                checkpoint=json.loads((root/'state'/'service-registry.json').read_text())
                assert checkpoint['schema']==3 and all(r.get('host_mode') in ('embedded','native') for r in checkpoint['rooms'])
                print('Native browser: independent pending history and accepted-response loss across refresh', flush=True)
                # Add terminal publications through the actual authenticated API,
                # forcing the original unknown message outside the recent window.
                for i in range(301):
                    response = await context.request.post(surface+'/api/v1/messages', headers=headers,
                        data={'id':f'window-{i}','text':'Completed history fixture','to':'slot2'})
                    assert response.status == 200
                    terminal = await response.json()
                    response = await context.request.post(surface+f'/api/v1/messages/{terminal["id"]}/cancel',headers=headers,data={})
                    assert response.status == 200
                tail=await read_json(context,snapshot_url+'?tail=1')
                assert not any(m['id']==interrupted_id for m in tail['relay']['messages'])
                pending=await read_json(context,surface+'/api/v1/pending?limit=10')
                assert any(m['id']==interrupted_id and m['state']=='unknown' for m in pending['messages'])
                await page.goto(surface+'/')
                await page.locator('[data-pending-open]').click()
                await expect(page.locator(f'[data-pending-id="{interrupted_id}"]')).to_be_visible()
                await page.locator(f'[data-pending-id="{interrupted_id}"] .message-action').first.click()
                await expect(page.locator(f'[data-history-id="{interrupted_id}"]')).to_be_visible()
                await page.locator('#diagnose').click()
                await expect(page.locator('#diagnostics')).to_contain_text('Uncertain delivery')
                published=[]
                async def lose_response(route):
                    if route.request.method != 'POST':
                        await route.continue_(); return
                    payload=route.request.post_data_json
                    response=await route.fetch()
                    assert response.status == 200
                    published.append(payload)
                    await route.abort('failed') # Only the response is lost; Service acceptance is real.
                await page.route('**/api/v1/messages',lose_response)
                await page.locator('#message-text').fill('One publication across reload')
                await page.locator('#send').click()
                await expect(page.locator('#outbox')).to_be_visible()
                assert len(published)==1
                original_id=published[0]['id']
                await page.reload()
                await expect(page.locator('#outbox')).not_to_be_visible()
                assert len(published)==1,'reload automatically sent again'
                receipt=await read_json(context,surface+f'/api/v1/sends/{original_id}')
                assert receipt['found'] and receipt['message']['text']=='One publication across reload'
                # The original uncertain body has not been duplicated in the authoritative history.
                full=await read_json(context,snapshot_url)
                assert sum(m['text']=='One publication across reload' for m in full['relay']['messages'])==1
                await page.unroute('**/api/v1/messages',lose_response)
                # A real optional review version can be displayed and checked without model calls.
                await page.locator('#review-anchor').check()
                await page.locator('#message-text').fill('## Review evidence\n\n**Versioned** review with `code` and [unsafe](javascript:alert).')
                await page.locator('#send').click()
                await expect(page.locator('#outbox')).not_to_be_visible()
                review_message=await wait_snapshot(context,snapshot_url,lambda v:any(m.get('review') for m in v['relay']['messages']))
                reviewed=next(m for m in review_message['relay']['messages'] if m.get('review'))
                card=page.locator(f'[data-message-id="{reviewed["id"]}"]')
                await expect(card.locator('.rich-content h2')).to_have_text('Review evidence')
                assert await card.locator('a[href^="javascript:"]').count()==0
                await card.locator('.review-evidence summary').click()
                await card.locator('[data-review]').click()
                await expect(card).to_contain_text('Same observed version')
                (repo/'review-new-file.txt').write_text('changed version',encoding='utf-8')
                await card.locator('[data-review]').click()
                await expect(card).to_contain_text('Evidence changed')
                # Use a direct native surface for responsive/locale screenshots.
                await page.goto(surface+'/')
                await expect(page.locator('#messages-title')).to_have_text('Relay messages')
                await page.evaluate("PairRoomTheme.setTheme('light')")
                await page.screenshot(path=str(artifacts/'native-light-en.png'),full_page=True)
                await page.locator('#language-button').click()
                await expect(page.locator('#messages-title')).to_have_text('中继消息')
                await page.evaluate("PairRoomTheme.setTheme('dark')")
                await page.screenshot(path=str(artifacts/'native-dark-zh.png'),full_page=True)
                await page.set_viewport_size({'width':420,'height':900})
                assert await page.evaluate('document.documentElement.scrollWidth <= innerWidth+1'), 'native surface overflows on mobile'
                await page.screenshot(path=str(artifacts/'native-mobile-zh.png'),full_page=True)
                await context.close()
                await service.stop()
                print('Native browser: restart preserves binding identity and queued delivery', flush=True)
                management_url = await service.start()
                origin=management_url.split('/#',1)[0].rstrip('/')
                # CLI reloads the new endpoint from the same owner-only file.
                status=json.loads(await asyncio.to_thread(cli,['status','--room',room_id,'--slot','slot1','--brief=false']))
                assert status['relay']['bindings']['slot1']['session_id']=='synthetic-slot1'
                assert any(m['text']=='Queued across Service restart' and m['state']=='queued' for m in status['relay']['messages'])
                received=await asyncio.to_thread(cli,['wait','--room',room_id,'--slot','slot2','--timeout','1'])
                assert 'Queued across Service restart' in received
                assert not errors, f'browser errors: {errors}'
                (artifacts/'results.json').write_text(json.dumps({
                    'mode':'synthetic native-hook inputs over real CLI/HTTP/SSE/browser',
                    'real_vendor_e2e':False,'native_creation':True,'native_grok_selection':True,'bind_env_association':True,
                    'fifo_stdout_ack':True,'idempotent_explicit_send':True,'conflicting_send_id_rejected':True,'three_bidirectional_rounds':True,
                    'pending_outside_tail':True,'refresh_receipt_recovery_without_resend':True,'review_anchor_staleness':True,'safe_native_markdown':True,'killed_cli_unknown':True,'explicit_retry_confirmation':True,'cancel_only_queued':True,
                    'checkpoint_schema_3_canonical':True,'restart_queue_and_bindings':True,
                    'english_chinese_light_dark_responsive':True,'browser_errors':errors,
                },indent=2)+'\n',encoding='utf-8')
                print('Native host real-transport browser checks passed (vendor E2E unverified)',flush=True)
            finally:
                await service.stop()
                await browser.close()


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary',type=Path)
    parser.add_argument('--browser',default=os.environ.get('PAIRROOM_BROWSER_EXECUTABLE'))
    parser.add_argument('--artifacts',type=Path,default=ROOT/'.browser-results/native-host')
    args=parser.parse_args()
    asyncio.run(verify(args.binary,args.browser,args.artifacts))


if __name__=='__main__':
    main()
