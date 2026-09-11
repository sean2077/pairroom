#!/usr/bin/env python3
"""Native-host real HTTP/CLI/browser recovery test with synthetic hook inputs.

Exercises the shipped binary, not fetch/EventSource mocks. It does NOT prove
Claude Code/Codex acceptance or real vendor E2E. No tokens/credentials exported.
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


async def verify(binary: Path | None, browser_path: str | None, artifacts: Path) -> None:
    artifacts.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='pairroom-native-browser-') as temporary:
        root = Path(temporary)
        if binary is None:
            binary = root / ('pairroom.exe' if os.name == 'nt' else 'pairroom')
            subprocess.run(['go', 'build', '-o', str(binary), './cmd/pairroom'], cwd=ROOT, check=True)
        binary = binary.resolve()
        repo = root / 'project'
        repo.mkdir()
        subprocess.run(['git', 'init', '-q', str(repo)], check=True)
        service = Service(binary, root)
        env = os.environ.copy()
        env.update(HOME=str(root/'home'), USERPROFILE=str(root/'home'), XDG_CONFIG_HOME=str(root/'home'/'.config'))
        env['PATH'] = str(binary.parent) + os.pathsep + env.get('PATH', '')
        errors = []

        def cli(args, payload=None):
            result = subprocess.run([str(binary), 'relay', *args, '--repo', str(repo)],
                                    input=None if payload is None else json.dumps(payload),
                                    text=True, capture_output=True, env=env, cwd=repo, timeout=45)
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
                await expect(page.locator('input[name="claude-mode"][value="existing"]')).to_be_disabled()
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
                print('Native browser: approved-hook setup fixture and nonce association through real CLI', flush=True)
                for slot in ('claude','codex'):
                    await asyncio.to_thread(cli, ['install','--runtime',slot])
                    raw = await asyncio.to_thread(cli, ['bind','--room',room_id,'--slot',slot,'--service-file',str(endpoint)])
                    bound = json.loads(raw)
                    response = await context.request.post(surface+f'/api/v1/participants/{slot}/park', headers=headers, data={'enabled':False})
                    assert response.status == 200
                    hook = {'hook_event_name':'Stop','session_id':'synthetic-'+slot,'cwd':str(repo),
                            'last_assistant_message':bound['bind_nonce'],'stop_hook_active':False,
                            'transcript_path':'/optional/unavailable/transcript'}
                    assert json.loads(await asyncio.to_thread(cli,['hook','--runtime',slot],hook)) == {}
                snapshot = await wait_snapshot(context,snapshot_url,lambda s:all(b.get('session_id') for b in s['relay']['bindings'].values()))
                assert snapshot['protocol'] == 'pairroom-protocol/v7'
                await expect(frame.locator('[data-slot="claude"]')).to_contain_text('Associated')
                # UI -> durable inbox -> CLI stdout -> explicit ack, with no model
                # acceptance fiction. A draft survives independent SSE messages.
                await frame.locator('#message-text').fill('User input for a native session')
                await frame.locator('#send').click()
                snapshot = await wait_snapshot(context,snapshot_url,lambda s:len(s['relay']['messages']) == 1)
                assert snapshot['relay']['messages'][0]['state'] == 'queued'
                received = await asyncio.to_thread(cli,['wait','--room',room_id,'--slot','claude','--timeout','1'])
                assert 'User input for a native session' in received
                await wait_snapshot(context,snapshot_url,lambda s:s['relay']['messages'][0]['state']=='handed_off')
                await frame.locator('#message-text').fill('Preserve this unsent draft')
                await asyncio.to_thread(cli,['send','--room',room_id,'--slot','claude','--id','explicit-fixture','--text','@user is body text: explicit send still targets the peer'])
                await expect(frame.locator('#message-text')).to_have_value('Preserve this unsent draft')
                snapshot = await wait_snapshot(context,snapshot_url,lambda s:len(s['relay']['messages']) == 2)
                assert snapshot['relay']['messages'][1]['to']=='codex'
                await asyncio.to_thread(cli,['send','--room',room_id,'--slot','claude','--id','explicit-fixture','--text','same ID returns original receipt'])
                assert len((await read_json(context,snapshot_url))['relay']['messages']) == 2
                await asyncio.to_thread(cli,['wait','--room',room_id,'--slot','codex','--timeout','1'])

                print('Native browser: real CLI killed after durable claim, unknown + explicit Retry dialog', flush=True)
                # A pipe with no reader blocks the large stdout write. Killing
                # there must never acknowledge delivery or automatically replay.
                raw = await asyncio.to_thread(cli,['send','--room',room_id,'--slot','codex','--id','kill-fixture','--text','large reply\n'+('x'*(96<<10))])
                interrupted = json.loads(raw)
                waiting = subprocess.Popen([str(binary),'relay','wait','--repo',str(repo),'--room',room_id,'--slot','claude','--timeout','1'],
                                           env=env,cwd=repo,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
                try:
                    await wait_snapshot(context,snapshot_url,lambda s:any(m['id']==interrupted['id'] and m['state']=='delivering' for m in s['relay']['messages']))
                    waiting.kill()
                    await asyncio.to_thread(waiting.communicate, timeout=5)
                finally:
                    if waiting.poll() is None:
                        waiting.kill()
                        await asyncio.to_thread(waiting.communicate, timeout=5)
                await wait_snapshot(context,snapshot_url,lambda s:any(m['id']==interrupted['id'] and m['state']=='unknown' for m in s['relay']['messages']))
                original = frame.locator(f'[data-message-id="{interrupted["id"]}"]')
                await original.locator('[data-action="retry"]').click()
                await expect(frame.locator('#confirm-dialog')).to_be_visible()
                await frame.locator('#confirm-dialog button[value="cancel"]').click()
                await expect(frame.locator('#confirm-dialog')).not_to_be_visible()
                await original.locator('[data-action="retry"]').click()
                await frame.locator('#confirm-retry').click()
                snapshot = await wait_snapshot(context,snapshot_url,lambda s:any(m.get('retry_of')==interrupted['id'] for m in s['relay']['messages']))
                retried = next(m for m in snapshot['relay']['messages'] if m.get('retry_of')==interrupted['id'])
                assert retried['id']!=interrupted['id'] and retried['state']=='queued'
                await frame.locator(f'[data-message-id="{retried["id"]}"] [data-action="cancel"]').click()
                await wait_snapshot(context,snapshot_url,lambda s:any(m['id']==retried['id'] and m['state']=='cancelled' for m in s['relay']['messages']))
                # Both actual Stop process calls park concurrently and exchange
                # replies. This is synthetic hook-input E2E, NOT native model E2E.
                print('Native browser: multi-round bidirectional Stop processes, no fetch/SSE mocks', flush=True)
                for slot in ('claude','codex'):
                    response = await context.request.post(surface+f'/api/v1/participants/{slot}/park',headers=headers,data={'enabled':True})
                    assert response.status == 200
                for round_no in range(3):
                    def hook(slot,peer):
                        return {'hook_event_name':'Stop','session_id':'synthetic-'+slot,'cwd':str(repo),
                                'last_assistant_message':f'@{peer} complete boundary reply {round_no} from {slot}',
                                'stop_hook_active':round_no>0}
                    left,right = await asyncio.gather(
                        asyncio.to_thread(cli,['hook','--runtime','claude'],hook('claude','codex')),
                        asyncio.to_thread(cli,['hook','--runtime','codex'],hook('codex','claude')))
                    assert json.loads(left)['decision'] == json.loads(right)['decision'] == 'block'
                    assert f'reply {round_no} from codex' in json.loads(left)['reason']
                    assert f'reply {round_no} from claude' in json.loads(right)['reason']
                await asyncio.to_thread(cli,['send','--room',room_id,'--slot','claude','--id','restart-fixture','--text','Queued across Service restart'])
                checkpoint=json.loads((root/'state'/'service-registry.json').read_text())
                assert checkpoint['schema']==2 and all('host_mode' not in r for r in checkpoint['rooms'])
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
                status=json.loads(await asyncio.to_thread(cli,['status','--room',room_id,'--slot','claude']))
                assert status['relay']['bindings']['claude']['session_id']=='synthetic-claude'
                assert any(m['text']=='Queued across Service restart' and m['state']=='queued' for m in status['relay']['messages'])
                received=await asyncio.to_thread(cli,['wait','--room',room_id,'--slot','codex','--timeout','1'])
                assert 'Queued across Service restart' in received
                assert not errors, f'browser errors: {errors}'
                (artifacts/'results.json').write_text(json.dumps({
                    'mode':'synthetic native-hook inputs over real CLI/HTTP/SSE/browser',
                    'real_vendor_e2e':False,'native_creation':True,'nonce_association':True,
                    'fifo_stdout_ack':True,'idempotent_explicit_send':True,'three_bidirectional_rounds':True,
                    'killed_cli_unknown':True,'explicit_retry_confirmation':True,'cancel_only_queued':True,
                    'checkpoint_schema_2_unchanged':True,'restart_queue_and_bindings':True,
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
