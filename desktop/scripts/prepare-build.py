#!/usr/bin/env python3
from __future__ import annotations

import pathlib
import plistlib
import re
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[1]
REPOSITORY = ROOT.parent


def allow_macos_local_networking() -> None:
    """Keep the production WKWebView allowed to reach PairRoom's loopback HTTP service."""
    plist_path = ROOT / "build" / "darwin" / "Info.plist"
    if not plist_path.is_file():
        raise SystemExit(f"Wails did not generate the macOS plist: {plist_path}")
    with plist_path.open("rb") as source:
        payload = plistlib.load(source)
    transport = payload.setdefault("NSAppTransportSecurity", {})
    transport["NSAllowsLocalNetworking"] = True
    with plist_path.open("wb") as target:
        plistlib.dump(payload, target, fmt=plistlib.FMT_XML, sort_keys=False)


def main() -> int:
    version = (REPOSITORY / "VERSION").read_text(encoding="utf-8").strip()
    config = (ROOT / "build" / "config.yml").read_text(encoding="utf-8")
    match = re.search(r'(?m)^\s*version:\s*"([^"]+)"\s*$', config)
    if match is None or match.group(1) != version:
        raise SystemExit(
            f"desktop build version {match.group(1) if match else '<missing>'} "
            f"does not match VERSION {version}"
        )
    subprocess.run(
        [
            "wails3",
            "update",
            "build-assets",
            "-name",
            "PairRoom",
            "-binaryname",
            "PairRoom",
            "-config",
            "config.yml",
            "-dir",
            ".",
        ],
        cwd=ROOT / "build",
        check=True,
    )
    allow_macos_local_networking()
    print(f"prepared Wails v3 build assets for PairRoom {version}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
