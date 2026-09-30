#!/usr/bin/env python3
"""Offline checks for translation completeness, links, commands, and public packaging."""
import json
import subprocess
import tempfile
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import urlsplit

from build_website import ROOT, PUBLIC_FILES, SCREENS, build


class Page(HTMLParser):
    def __init__(self):
        super().__init__()
        self.keys = set()
        self.ids = set()
        self.references = []

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if "data-i18n" in attrs:
            self.keys.add(attrs["data-i18n"])
        if "id" in attrs:
            assert attrs["id"] not in self.ids, f"Duplicate ID: {attrs['id']}"
            self.ids.add(attrs["id"])
        for attribute in ("href", "src"):
            if attribute in attrs:
                self.references.append(attrs[attribute])


def check():
    page = Page()
    source = (ROOT / "website/index.html").read_text(encoding="utf-8")
    page.feed(source)
    chinese = json.loads((ROOT / "website/zh-CN.json").read_text(encoding="utf-8"))
    assert set(chinese) == page.keys | {"pageTitle", "pageDescription"}, "Translation keys differ from the page"
    assert all(isinstance(value, str) and value.strip() for value in chinese.values()), "Empty translation"
    subprocess.run(["node", "--check", str(ROOT / "website/app.js")], check=True)
    for command in ("winget install PairRoom", "sh install-pairroom.sh", "pairroom version",
                    'pairroom service --mock --data-root "$HOME/.pairroom-demo"'):
        assert command in source and command in (ROOT / "README.md").read_text(encoding="utf-8"), command
    with tempfile.TemporaryDirectory(prefix="pairroom-website-check-") as directory:
        output = Path(directory) / "pairroom"
        build(output)
        expected = set(PUBLIC_FILES) | {".nojekyll", "favicon.png"} | {
            f"images/{screen}{suffix}.png" for screen in SCREENS for suffix in ("", "-zh")}
        actual = {path.relative_to(output).as_posix() for path in output.rglob("*") if path.is_file()}
        assert actual == expected, f"Unexpected deployed files: {actual ^ expected}"
        assert (output / "favicon.png").read_bytes() == (ROOT / "desktop/assets/icon.png").read_bytes(), "Desktop icon drift"
        for reference in page.references:
            url = urlsplit(reference)
            if url.scheme:
                assert url.scheme == "https", f"Insecure or unexpected link: {reference}"
                if url.netloc == "github.com" and url.path.startswith("/sean2077/pairroom/blob/main/"):
                    path = ROOT / url.path.removeprefix("/sean2077/pairroom/blob/main/")
                    assert path.is_file(), f"Missing documentation: {reference}"
                continue
            assert not url.path.startswith("/"), f"Root-relative path breaks project Pages: {reference}"
            assert ".." not in Path(url.path).parts, f"Reference escapes website: {reference}"
            if url.path:
                assert (output / url.path).is_file(), f"Missing public asset: {reference}"
            if url.fragment:
                assert url.fragment in page.ids, f"Broken anchor: {reference}"
    print(f"Website checks passed: {len(page.keys)} translations, public assets, links, installation commands, JavaScript.")


if __name__ == "__main__":
    check()
