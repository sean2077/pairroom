#!/usr/bin/env python3
from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import shutil
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[1]
REPOSITORY = ROOT.parent


def digest(path: pathlib.Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def collect(platform: str) -> list[pathlib.Path]:
    binary_dir = ROOT / "bin"
    if platform == "linux":
        candidates = [*binary_dir.glob("*.AppImage"), *binary_dir.glob("*.deb")]
    elif platform == "windows":
        candidates = [
            path
            for path in binary_dir.glob("*.exe")
            if path.name.lower() != "pairroom.exe"
        ]
        portable = binary_dir / "PairRoom.exe"
        if portable.exists():
            candidates.append(portable)
    elif platform == "darwin":
        app = binary_dir / "PairRoom.app"
        if not app.is_dir():
            raise SystemExit(f"expected macOS bundle: {app}")
        archive = binary_dir / "PairRoom.app.zip"
        subprocess.run(
            ["ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", str(app), str(archive)],
            check=True,
        )
        candidates = [archive]
    else:
        raise SystemExit(f"unsupported platform {platform!r}")
    result = sorted({path.resolve() for path in candidates if path.is_file()})
    if not result:
        raise SystemExit(f"no final {platform} desktop packages found in {binary_dir}")
    return result


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--platform", required=True, choices=["linux", "windows", "darwin"])
    parser.add_argument("--arch", required=True)
    parser.add_argument("--label", required=True)
    args = parser.parse_args()

    destination = ROOT / "dist" / args.label
    if destination.exists():
        shutil.rmtree(destination)
    destination.mkdir(parents=True)

    packages = []
    checksum_lines = []
    for source in collect(args.platform):
        target = destination / source.name
        shutil.copy2(source, target)
        sha256 = digest(target)
        checksum_lines.append(f"{sha256}  {target.name}")
        packages.append(
            {"name": target.name, "bytes": target.stat().st_size, "sha256": sha256}
        )

    (destination / "SHA256SUMS").write_text(
        "\n".join(checksum_lines) + "\n", encoding="utf-8", newline="\n"
    )
    manifest = {
        "product": "PairRoom",
        "framework": "Wails v3",
        "version": (REPOSITORY / "VERSION").read_text(encoding="utf-8").strip(),
        "platform": args.platform,
        "arch": args.arch,
        "artifact": args.label,
        "packages": packages,
    }
    (destination / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
        newline="\n",
    )
    print(f"collected {len(packages)} package(s) in {destination.relative_to(REPOSITORY)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
