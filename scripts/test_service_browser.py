#!/usr/bin/env python3
"""Real browser -> authenticated HTTP/SSE -> Mock runtime -> restart smoke.

No fetch/EventSource mocks, vendor CLIs, external services, or stored auth state.
Only safe screenshots and assertion results are exported; ephemeral tokens stay
inside the test's temporary directory and memory.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import re
import subprocess
import tempfile
from pathlib import Path
from urllib.parse import urlsplit

from playwright.async_api import async_playwright, expect

ROOT = Path(__file__).resolve().parents[1]


class Service:
    def __init__(self, binary: Path, root: Path):
        self.binary, self.root = binary, root
        self.process = None
        self.output = None
        self.log = root / 'service.log'

    async def start(self) -> str:
        env = os.environ.copy()
        home = self.root / 'home'
        home.mkdir(exist_ok=True)
        env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / '.config'))
        self.output = self.log.open('w')
        self.process = subprocess.Popen(
            [str(self.binary), 'service', '--mock', '--no-browser', '--listen', '127.0.0.1:0',
             '--data-root', str(self.root / 'state'), '--shutdown-timeout', '10s'],
            cwd=self.root, env=env, stdout=self.output, stderr=subprocess.STDOUT,
        )
        for _ in range(300):
            content = self.log.read_text(encoding='utf-8', errors='replace')
            match = re.search(r'management:\s+(http://127\.0\.0\.1:\d+/\S*)', content)
            if match:
                return match[1]
            if self.process.poll() is not None:
                raise AssertionError(f'Mock service exited during startup ({self.process.returncode}); raw log withheld')
            await asyncio.sleep(.1)
        raise AssertionError('Mock service did not publish a Management URL')

    async def stop(self) -> None:
        if self.process and self.process.poll() is None:
            self.process.terminate()
            try:
                await asyncio.to_thread(self.process.wait, 15)
            except subprocess.TimeoutExpired:
                self.process.kill()
                await asyncio.to_thread(self.process.wait, 5)
                raise AssertionError('Mock service failed to drain normally')
            if self.process.returncode != 0:
                raise AssertionError(f'Mock service shutdown exit {self.process.returncode}')
        if self.output:
            self.output.close()


async def read_json(context, url: str) -> dict:
    response = await context.request.get(url)
    assert response.status == 200, f'GET {urlsplit(url).path}: {response.status}'
    return await response.json()


async def wait_snapshot(context, url, predicate):
    for _ in range(200):
        snapshot = await read_json(context, url)
        if predicate(snapshot):
            return snapshot
        await asyncio.sleep(.1)
    raise AssertionError(f'Snapshot condition not satisfied: {urlsplit(url).path}')


async def verify(binary: Path | None, browser_path: str | None, artifacts: Path) -> None:
    artifacts.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='pairroom-browser-') as temp:
        root = Path(temp)
        if binary is None:
            binary = root / ('pairroom.exe' if os.name == 'nt' else 'pairroom')
            subprocess.run(['go', 'build', '-o', str(binary), './cmd/pairroom'], cwd=ROOT, check=True)
        binary = binary.resolve()
        repo = root / 'project'
        repo.mkdir()
        subprocess.run(['git', 'init', '-q', str(repo)], check=True)
        (repo / 'README.md').write_text('# Deterministic Mock workspace\n', encoding='utf-8')
        subprocess.run(['git', '-C', str(repo), 'add', 'README.md'], check=True)
        subprocess.run(['git', '-C', str(repo), '-c', 'user.name=Browser test', '-c', 'user.email=test@example.invalid',
                        'commit', '-qm', 'fixture'], check=True)
        service = Service(binary, root)
        errors, event_streams = [], []
        instructions = 'Agent 1 plans and reviews. Agent 2 implements and tests. Preserve exact peer mentions.'
        results = {}
        try:
            async with async_playwright() as playwright:
                options = {'headless': True}
                if browser_path:
                    options['executable_path'] = browser_path
                browser = await playwright.chromium.launch(**options)
                try:
                    management_url = await service.start()
                    origin = management_url.split('/#', 1)[0].rstrip('/')
                    context = await browser.new_context(viewport={'width':1440,'height':1000}, locale='en-US', reduced_motion='reduce')
                    context.set_default_timeout(20000)
                    page = await context.new_page()
                    page.on('pageerror', lambda error: errors.append(str(error)))
                    page.on('response', lambda response: event_streams.append(response.status) if '/api/v1/events' in response.url else None)
                    response = await page.goto(management_url)
                    csp = response.headers.get('content-security-policy', '')
                    assert "script-src 'self'" in csp and 'unsafe-eval' not in csp, 'Management CSP changed'
                    await expect(page.locator('#app')).to_be_visible()
                    # Authentication is established by the actual bootstrap UI.
                    assert 'token=' not in page.url, 'bootstrap token retained in URL'
                    denied = await context.request.post(origin+'/api/v1/projects', data={'path':str(repo)})
                    assert denied.status == 403, 'cookie-authenticated write without CSRF accepted'
                    await page.locator('#add-project-button').click()
                    await page.locator('#project-path').fill(str(repo))
                    async with page.expect_response(lambda r: r.url.endswith('/api/v1/projects') and r.request.method=='POST') as registered:
                        await page.locator('#project-submit').click()
                    assert (await registered.value).status == 201
                    await expect(page.locator('#project-dialog')).not_to_be_visible()
                    await page.get_by_role('button', name=re.compile('Create Room', re.I)).first.click()
                    await page.locator('#room-collaboration-mode').select_option('custom')
                    await page.locator('#room-collaboration-instructions').fill(instructions)
                    await expect(page.locator('#room-name')).to_have_value('')
                    async with page.expect_response(lambda r: r.url.endswith('/rooms') and r.request.method=='POST') as created:
                        await page.locator('#room-submit').click()
                    creation = await created.value
                    assert creation.status == 201, f'Room creation status {creation.status}'
                    room = await creation.json()
                    room_id = room['id']
                    assert room['name'].startswith('Room-') and room['collaboration']['instructions'] == instructions
                    await expect(page.locator('#room-dialog')).not_to_be_visible()
                    await page.locator(f'.tree-room[data-room-id="{room_id}"]').click()
                    frame = page.frame_locator(f'#room-stage [data-room-id="{room_id}"] iframe')
                    await expect(frame.locator('#connection')).to_have_class(re.compile(r'\bconnected\b'))
                    surface = origin+f'/api/v1/rooms/{room_id}/surface'
                    snapshot_url = surface+'/api/v1/snapshot'
                    assert 200 in event_streams, 'real SSE was not opened'
                    # One explicit peer mention exercises both native Mock slots.
                    text = '@codex Review the deterministic HTTP path and reply once.'
                    await frame.locator('#message-input').fill(text)
                    await frame.locator('#send-button').click()
                    snapshot = await wait_snapshot(context, snapshot_url, lambda s:
                        {'claude','codex'}.issubset({m['from'] for m in s['messages']})
                        and all(v not in ('waiting','working') for m in s['messages'] for v in m.get('processing',{}).values()))
                    for message in snapshot['messages']:
                        await expect(frame.locator(f'[data-message-id="{message["id"]}"]')).to_be_visible()
                    identities = {actor: participant['session_id'] for actor,participant in snapshot['participants'].items()}
                    assert all(identities.values()), 'native Mock session identity not materialized'
                    # Settings travel through Room HTTP, its event log and actual SSE.
                    await frame.locator('#stall-warning').fill('600')
                    await frame.locator('#save-settings').click()
                    await expect(frame.locator('#settings-status')).to_have_text('')
                    assert (await read_json(context,snapshot_url))['settings']['stall_warning_seconds'] == 600
                    await frame.locator('[data-permission-actor="codex"]').select_option('read-only')
                    snapshot = await wait_snapshot(context,snapshot_url,lambda s:s['participants']['codex']['permission_profile']=='read-only')
                    assert snapshot['meta']['collaboration']['instructions'] == instructions
                    assert {a:p['session_id'] for a,p in snapshot['participants'].items()} == identities
                    await page.screenshot(path=str(artifacts/'service-live-room.png'))
                    await page.locator(f'.tree-room[data-room-id="{room_id}"]').click(button='right')
                    await page.locator('#context-rename-room').click()
                    await page.locator('#rename-room-name').fill('Verified HTTP workspace')
                    await page.locator('#rename-form [type="submit"]').click()
                    await expect(page.locator('#rename-dialog')).not_to_be_visible()
                    await expect(page.locator(f'.tree-room[data-room-id="{room_id}"]')).to_contain_text('Verified HTTP workspace')
                    await context.close()
                    await service.stop()
                    # New process and cookie jar: no stale lock recovery, no reuse of auth.
                    management_url = await service.start()
                    origin = management_url.split('/#',1)[0].rstrip('/')
                    context = await browser.new_context(viewport={'width':1440,'height':1000},locale='en-US',reduced_motion='reduce')
                    context.set_default_timeout(20000)
                    page = await context.new_page()
                    page.on('pageerror', lambda error: errors.append(str(error)))
                    await page.goto(management_url)
                    await page.locator(f'.tree-room[data-room-id="{room_id}"]').click()
                    frame = page.frame_locator(f'#room-stage [data-room-id="{room_id}"] iframe')
                    await expect(frame.locator('#connection')).to_have_class(re.compile(r'\bconnected\b'))
                    recovered = await read_json(context,origin+f'/api/v1/rooms/{room_id}/surface/api/v1/snapshot')
                    assert recovered['meta']['name'] == 'Verified HTTP workspace'
                    assert recovered['meta']['collaboration']['instructions'] == instructions
                    assert recovered['settings']['stall_warning_seconds'] == 600
                    assert recovered['participants']['codex']['permission_profile'] == 'read-only'
                    assert {a:p['session_id'] for a,p in recovered['participants'].items()} == identities
                    assert [m['id'] for m in recovered['messages']] == [m['id'] for m in snapshot['messages']]
                    await expect(frame.locator('#stall-warning')).to_have_value('600')
                    await page.screenshot(path=str(artifacts/'service-restored-room.png'))
                    await context.close()
                    assert not errors, errors
                    results = dict(real_http_authentication=True,csrf_required=True,creation_custom_optional_name=True,
                                   real_sse=True,both_mock_slots=True,settings_persisted=True,permission_independent=True,
                                   rename_restart_identity=True,unchanged_management_csp=True,page_errors=errors)
                finally:
                    await browser.close()
        finally:
            await service.stop()
        (artifacts/'results.json').write_text(json.dumps(results,indent=2)+'\n',encoding='utf-8')
        print(json.dumps(results,indent=2))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary',type=Path,help='Use a freshly built CLI; otherwise build from this checkout')
    parser.add_argument('--browser',default=os.environ.get('PAIRROOM_BROWSER_EXECUTABLE'))
    parser.add_argument('--artifacts',type=Path,default=ROOT/'.browser-results/service')
    args=parser.parse_args()
    try:
        asyncio.run(verify(args.binary,args.browser,args.artifacts))
    except Exception as error:
        # Browser navigation exceptions can echo the one-time bootstrap URL.
        # Do not place usable auth material in CI logs or exported evidence.
        message = re.sub(r'(?i)(token[=\%3D]+)[A-Za-z0-9_+/%=-]+', r'\1[redacted]', str(error))
        raise SystemExit(message) from None
