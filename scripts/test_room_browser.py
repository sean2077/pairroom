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


def snapshot_fixture() -> dict:
    participants = {}
    for actor, name, role in [("claude", "Claude Code", "driver"), ("codex", "Codex", "reviewer")]:
        participants[actor] = {
            "id": actor, "display_name": name, "mention_handle": "@" + actor,
            "role": role, "state": "idle", "session_id": "fixture-" + actor,
            "model": "deterministic-fixture", "runtime_kind": actor,
            "runtime": {"available": True, "command": "fixture", "protocol": "browser-fixture", "capabilities": []},
            "workspace": {"kind": "driver-live" if role == "driver" else "reviewer-snapshot",
                          "path": "/workspace/example", "read_only": role == "reviewer", "read_only_enforced": role == "reviewer"},
        }
    return {
        "meta": {"id": "browser-fixture", "name": "Example workspace", "repo": "/workspace/example"},
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
