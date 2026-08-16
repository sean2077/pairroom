from __future__ import annotations

import json
import os
import threading
from datetime import datetime, timedelta, timezone
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

from playwright.sync_api import sync_playwright

ROOT = Path(__file__).resolve().parents[1]
ASSETS = ROOT / "internal" / "service" / "assets"
SHOTS = ROOT / "artifacts" / "screenshots"
NOW = datetime.now(timezone.utc)


def iso(delta: timedelta = timedelta()) -> str:
    return (NOW + delta).isoformat().replace("+00:00", "Z")


PROJECTS = [
    {"id": "project-pairroom", "root": "/Users/sean/src/pairroom", "available": True, "created_at": iso(timedelta(days=-30))},
    {"id": "project-tooling", "root": "/Users/sean/src/agent-tooling", "available": True, "created_at": iso(timedelta(days=-14))},
    {"id": "project-missing", "root": "/Volumes/archive/legacy-app", "available": False, "diagnostic": "Git worktree path is currently unavailable", "created_at": iso(timedelta(days=-90))},
]


def binding(mode: str, session: str = "", pending: bool = False):
    return {"agent": "", "mode": mode, "session_id": session, "pending": pending, "bound_at": iso(timedelta(days=-2))}


ROOMS = [
    {"id": "room-auth-review", "project_id": "project-pairroom", "name": "Auth 重构审查", "data_dir": "/data/rooms/auth", "lifecycle": "active", "bindings": {"claude": binding("existing", "claude-45dd-review"), "codex": binding("existing", "codex-88ac-review")}, "transcript_boundary_notice": "Existing bindings resume vendor context without importing prior transcript.", "created_at": iso(timedelta(days=-2)), "updated_at": iso(timedelta(minutes=-1))},
    {"id": "room-release", "project_id": "project-pairroom", "name": "v1.1 Release Readiness", "data_dir": "/data/rooms/release", "lifecycle": "active", "bindings": {"claude": binding("new", pending=True), "codex": binding("new", pending=True)}, "transcript_boundary_notice": "Existing bindings resume vendor context without importing prior transcript.", "created_at": iso(timedelta(hours=-6)), "updated_at": iso(timedelta(minutes=-4))},
    {"id": "room-design", "project_id": "project-pairroom", "name": "Management Shell 设计", "data_dir": "/data/rooms/design", "lifecycle": "active", "bindings": {"claude": binding("existing", "claude-design"), "codex": binding("existing", "codex-design")}, "transcript_boundary_notice": "Existing bindings resume vendor context without importing prior transcript.", "created_at": iso(timedelta(days=-4)), "updated_at": iso(timedelta(hours=-1))},
    {"id": "room-failed", "project_id": "project-tooling", "name": "Runtime cleanup investigation", "data_dir": "/data/rooms/failed", "lifecycle": "active", "bindings": {"claude": binding("existing", "claude-runtime"), "codex": binding("existing", "codex-runtime")}, "transcript_boundary_notice": "Existing bindings resume vendor context without importing prior transcript.", "created_at": iso(timedelta(days=-1)), "updated_at": iso(timedelta(minutes=-30))},
    {"id": "room-legacy", "project_id": "project-tooling", "name": "Imported legacy room", "data_dir": "/data/rooms/legacy", "lifecycle": "active", "legacy": True, "bindings": {"claude": {"agent": "claude", "mode": "existing", "pending": True}, "codex": binding("existing", "codex-legacy")}, "transcript_boundary_notice": "Existing bindings resume vendor context without importing prior transcript.", "created_at": iso(timedelta(days=-60)), "updated_at": iso(timedelta(days=-12))},
    {"id": "room-archive", "project_id": "project-missing", "name": "Archived migration", "data_dir": "/data/rooms/archive", "lifecycle": "archived", "bindings": {"claude": binding("existing", "claude-archive"), "codex": binding("existing", "codex-archive")}, "transcript_boundary_notice": "Existing bindings resume vendor context without importing prior transcript.", "created_at": iso(timedelta(days=-80)), "updated_at": iso(timedelta(days=-20))},
]

RUNTIMES = [
    {"room_id": "room-auth-review", "phase": "active", "busy": True, "occupies_capacity": True, "url": "http://127.0.0.1:43101/#token=redacted", "last_used_at": iso(timedelta(seconds=-12))},
    {"room_id": "room-release", "phase": "queued", "queue_position": 1, "busy": False, "occupies_capacity": False, "queued_at": iso(timedelta(minutes=-3))},
    {"room_id": "room-design", "phase": "suspended", "busy": False, "occupies_capacity": False, "last_used_at": iso(timedelta(hours=-1))},
    {"room_id": "room-failed", "phase": "failed", "busy": False, "occupies_capacity": True, "last_used_at": iso(timedelta(minutes=-31)), "last_error": "vendor process close state is uncertain; capacity retained until controlled service restart"},
    {"room_id": "room-legacy", "phase": "suspended", "busy": False, "occupies_capacity": False},
    {"room_id": "room-archive", "phase": "suspended", "busy": False, "occupies_capacity": False},
]

SNAPSHOT = {
    "version": "1.0.0-dev",
    "commit": "95259bd+ux",
    "build_date": "2026-08-16T00:00:00Z",
    "data_root": "/Users/sean/.pairroom/service",
    "generated_at": iso(),
    "projects": PROJECTS,
    "rooms": ROOMS,
    "runtimes": RUNTIMES,
    "runtime_policy": {"limit": 2, "idle_timeout_seconds": 900, "poll_interval_milliseconds": 500, "close_timeout_seconds": 10},
    "summary": {"projects": 3, "unavailable_projects": 1, "rooms": 6, "active_rooms": 5, "archived_rooms": 1, "pending_bindings": 1, "runtime_capacity_used": 2, "active_runtimes": 1, "busy_runtimes": 1, "queued_runtimes": 1, "failed_runtimes": 1, "attention_items": 3},
    "capabilities": {"legacy_import": True, "runtime_suspend": True, "runtime_policy_mutation": False, "project_removal": False, "room_deletion": False, "server_path_browser": False},
    "healthy": True,
}


class Handler(BaseHTTPRequestHandler):
    server_version = "PairRoomVisualFixture/1.0"

    def log_message(self, *args):
        return

    def _headers(self, status=200, content_type="application/json; charset=utf-8"):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Cache-Control", "no-store")
        self.end_headers()

    def do_GET(self):
        path = urlparse(self.path).path
        if path == "/api/v1/service":
            payload = dict(SNAPSHOT)
            payload["generated_at"] = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
            body = json.dumps(payload, ensure_ascii=False).encode()
            self._headers()
            self.wfile.write(body)
            return
        if path == "/":
            path = "/index.html"
        asset = ASSETS / path.lstrip("/")
        if not asset.is_file() or asset.parent != ASSETS:
            self._headers(404, "text/plain; charset=utf-8")
            self.wfile.write(b"not found")
            return
        mime = {".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8"}.get(asset.suffix, "application/octet-stream")
        self._headers(200, mime)
        self.wfile.write(asset.read_bytes())

    def do_POST(self):
        # Visual smoke only: report a successful stable response so action wiring
        # can be exercised without mutating the fixture.
        path = urlparse(self.path).path
        length = int(self.headers.get("Content-Length", "0"))
        if length:
            self.rfile.read(length)
        if path.endswith("/activate"):
            body = {"room_id": path.split("/")[-2], "phase": "queued", "queue_position": 1, "busy": False, "occupies_capacity": False}
        elif path.endswith("/suspend"):
            body = {"room_id": path.split("/")[-2], "phase": "suspended", "busy": False, "occupies_capacity": False}
        else:
            body = {"ok": True}
        self._headers(200)
        self.wfile.write(json.dumps(body).encode())

    def do_PATCH(self):
        self.do_POST()


class Server:
    def __enter__(self):
        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()
        return self.httpd.server_address[1]

    def __exit__(self, *_):
        self.httpd.shutdown()
        self.thread.join(timeout=2)


def assert_no_horizontal_overflow(page, label: str):
    overflow = page.evaluate("document.documentElement.scrollWidth - document.documentElement.clientWidth")
    if overflow > 1:
        raise AssertionError(f"{label}: horizontal overflow {overflow}px")


def fixture_html() -> str:
    html = (ASSETS / "index.html").read_text(encoding="utf-8")
    css = (ASSETS / "management.css").read_text(encoding="utf-8")
    js = (ASSETS / "management.js").read_text(encoding="utf-8")
    snapshot_json = json.dumps(SNAPSHOT, ensure_ascii=False)
    prelude = f"""<script>
      location.hash = 'token=visual-secret';
      history.replaceState = () => {{}};
      const __snapshot = JSON.parse({json.dumps(snapshot_json)});
      window.fetch = async (input, init = {{}}) => {{
        const path = String(input);
        const method = String(init.method || 'GET').toUpperCase();
        let payload = __snapshot;
        if (method !== 'GET') {{
          if (path.endsWith('/activate')) payload = {{room_id: path.split('/').at(-2), phase: 'queued', queue_position: 1, busy: false, occupies_capacity: false}};
          else if (path.endsWith('/suspend')) payload = {{room_id: path.split('/').at(-2), phase: 'suspended', busy: false, occupies_capacity: false}};
          else payload = {{ok: true}};
        }}
        return new Response(JSON.stringify(payload), {{status: 200, headers: {{'Content-Type': 'application/json'}}}});
      }};
    </script>"""
    html = html.replace('<link rel="stylesheet" href="/management.css">', f"<style>{css}</style>")
    html = html.replace('<script src="/management.js" defer></script>', prelude + f"<script>{js}</script>")
    return html


def run():
    SHOTS.mkdir(parents=True, exist_ok=True)
    errors: list[str] = []
    fixture = fixture_html()
    with sync_playwright() as p:
        launch_options = {"headless": True, "args": ["--no-sandbox"]}
        chromium = os.environ.get("PAIRROOM_CHROMIUM", "").strip()
        if chromium:
            launch_options["executable_path"] = chromium
        browser = p.chromium.launch(**launch_options)
        context = browser.new_context(viewport={"width": 1440, "height": 1000}, device_scale_factor=1)
        page = context.new_page()
        page.on("console", lambda msg: errors.append(f"console:{msg.type}:{msg.text}") if msg.type == "error" else None)
        page.on("pageerror", lambda exc: errors.append(f"pageerror:{exc}"))
        page.set_content(fixture, wait_until="load")
        page.wait_for_selector("text=Auth 重构审查")
        assert page.locator("#connection-banner").is_hidden()
        assert_no_horizontal_overflow(page, "overview-desktop")
        page.screenshot(path=str(SHOTS / "overview-desktop.png"), full_page=True)

        routes = {
            "projects": "#/projects",
            "project-detail": "#/projects/project-pairroom",
            "runtimes": "#/runtimes",
            "settings-runtime": "#/settings",
        }
        for label, route in routes.items():
            page.evaluate("route => location.hash = route", route)
            page.wait_for_timeout(250)
            if label == "settings-runtime":
                page.get_by_role("button", name="Runtime 策略").click()
                page.wait_for_timeout(100)
            assert_no_horizontal_overflow(page, label)
            page.screenshot(path=str(SHOTS / f"{label}-desktop.png"), full_page=True)

        page.get_by_role("button", name="Daemon 运维").click()
        page.wait_for_timeout(100)
        assert page.get_by_text("pairroom daemon status", exact=False).count() >= 1
        assert page.get_by_text("--recover-stale-lock", exact=False).count() >= 1
        assert_no_horizontal_overflow(page, "settings-daemon")
        page.screenshot(path=str(SHOTS / "settings-daemon-desktop.png"), full_page=True)

        # Search + modal wiring smoke.
        page.evaluate("location.hash = '#/projects'")
        page.locator("#global-search").fill("legacy")
        page.wait_for_timeout(100)
        assert page.get_by_text("agent-tooling", exact=True).count() >= 1
        page.locator("#add-project-button").click()
        page.wait_for_selector("#project-dialog[open]")
        assert page.evaluate("document.activeElement === document.getElementById(\"project-path\")")
        page.locator('[data-close-dialog="project-dialog"]').first.click()

        # Mobile navigation and table adaptation.
        mobile = browser.new_context(viewport={"width": 390, "height": 844}, device_scale_factor=1)
        mpage = mobile.new_page()
        mpage.on("console", lambda msg: errors.append(f"mobile-console:{msg.type}:{msg.text}") if msg.type == "error" else None)
        mpage.on("pageerror", lambda exc: errors.append(f"mobile-pageerror:{exc}"))
        mpage.set_content(fixture, wait_until="load")
        mpage.wait_for_selector("text=Auth 重构审查")
        assert_no_horizontal_overflow(mpage, "overview-mobile")
        mpage.screenshot(path=str(SHOTS / "overview-mobile.png"), full_page=True)
        mpage.locator("#mobile-menu").click()
        assert "sidebar-open" in (mpage.locator("#app").get_attribute("class") or "")
        mpage.screenshot(path=str(SHOTS / "navigation-mobile.png"), full_page=True)
        mpage.get_by_role("link", name="Runtimes").click()
        mpage.wait_for_timeout(200)
        assert_no_horizontal_overflow(mpage, "runtimes-mobile")
        mpage.screenshot(path=str(SHOTS / "runtimes-mobile.png"), full_page=True)
        mobile.close()
        context.close()
        browser.close()

    if errors:
        raise AssertionError("Browser errors:\n" + "\n".join(errors))
    print(f"visual smoke passed; screenshots={SHOTS}")


if __name__ == "__main__":
    run()
