#!/usr/bin/env python3
from __future__ import annotations

import pathlib
import re
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[1]
REPOSITORY = ROOT.parent


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
    print(f"prepared Wails v3 build assets for PairRoom {version}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
