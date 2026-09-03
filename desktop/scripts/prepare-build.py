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
    contents = plist_path.read_bytes()
    # Wails beta.16 emits a DOCTYPE-first XML plist on a clean checkout.
    # Apple's tools accept it, but Python's plistlib requires the XML
    # declaration before the DOCTYPE. Add it only for that generated form;
    # plistlib can already read declaration-first XML and binary plists.
    if contents.lstrip().startswith(b"<!DOCTYPE plist"):
        contents = b'<?xml version="1.0" encoding="UTF-8"?>\n' + contents.lstrip()
    payload = plistlib.loads(contents)
    transport = payload.setdefault("NSAppTransportSecurity", {})
    transport["NSAllowsLocalNetworking"] = True
    with plist_path.open("wb") as target:
        plistlib.dump(payload, target, fmt=plistlib.FMT_XML, sort_keys=False)


def include_pairroom_in_linux_package() -> None:
    """Keep the generated nfpm package shipping the PairRoom CLI next to the host."""
    path = ROOT / "build" / "linux" / "nfpm" / "nfpm.yaml"
    if not path.is_file():
        return
    text = path.read_text(encoding="utf-8")
    if 'dst: "/usr/local/bin/pairroom"' in text:
        return
    needle = '  - src: "./bin/PairRoom"\n    dst: "/usr/local/bin/PairRoom"\n'
    if needle not in text:
        raise SystemExit(f"{path} does not contain the PairRoom host binary entry")
    path.write_text(
        text.replace(
            needle,
            needle + '  - src: "./bin/pairroom"\n    dst: "/usr/local/bin/pairroom"\n',
            1,
        ),
        encoding="utf-8",
    )


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
    include_pairroom_in_linux_package()
    print(f"prepared Wails v3 build assets for PairRoom {version}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
