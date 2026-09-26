#!/usr/bin/env python3
"""Merge per-platform desktop SHA256SUMS into one published release asset.

`collect-artifacts.py` writes a SHA256SUMS next to each platform's packages.
The publish job downloads every platform artifact and runs this script to
re-verify each package against its own list and write a single
`pairroom-desktop-vX.Y.Z-SHA256SUMS`, which is attached to the GitHub Release
beside the packages it covers.
"""
from __future__ import annotations

import argparse
import hashlib
import pathlib

PLATFORM_SUFFIXES = (
    "windows-amd64-setup.exe",
    "linux-amd64.AppImage",
    "linux-amd64.deb",
    "darwin-arm64.app.zip",
    "darwin-amd64.app.zip",
)


def digest(path: pathlib.Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def output_name(version: str) -> str:
    return f"pairroom-desktop-v{version.lstrip('v')}-SHA256SUMS"


def required_packages(version: str) -> list[str]:
    version = version.lstrip("v")
    return [f"pairroom-desktop-v{version}-{suffix}" for suffix in PLATFORM_SUFFIXES]


def merge(root: pathlib.Path, version: str, require_all: bool = True) -> list[str]:
    entries: dict[str, str] = {}
    lists = sorted(root.rglob("SHA256SUMS"))
    if not lists:
        raise SystemExit(f"no SHA256SUMS found under {root}")
    for sums in lists:
        for line in sums.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            expected, separator, name = line.partition("  ")
            if not separator or len(expected) != 64 or "/" in name or "\\" in name:
                raise SystemExit(f"malformed checksum line in {sums}: {line!r}")
            package = sums.parent / name
            if not package.is_file():
                raise SystemExit(f"{sums} lists {name}, which is missing")
            actual = digest(package)
            if actual != expected:
                raise SystemExit(f"checksum mismatch for {name}: expected {expected}, got {actual}")
            if name in entries:
                raise SystemExit(f"{name} is listed by more than one platform artifact")
            entries[name] = expected
    if require_all:
        missing = [name for name in required_packages(version) if name not in entries]
        if missing:
            raise SystemExit(f"desktop checksums are missing required packages: {missing}")
    return [f"{entries[name]}  {name}" for name in sorted(entries)]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, type=pathlib.Path)
    parser.add_argument("--version", required=True)
    parser.add_argument("--output-dir", required=True, type=pathlib.Path)
    args = parser.parse_args()

    lines = merge(args.root, args.version)
    args.output_dir.mkdir(parents=True, exist_ok=True)
    output = args.output_dir / output_name(args.version)
    output.write_text("\n".join(lines) + "\n", encoding="utf-8", newline="\n")
    print(f"wrote {output} covering {len(lines)} package(s)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
