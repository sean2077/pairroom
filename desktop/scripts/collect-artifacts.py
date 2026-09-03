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


def require_packages(paths: list[pathlib.Path], description: str) -> list[pathlib.Path]:
    result = sorted({path.resolve() for path in paths if path.is_file()})
    if not result:
        raise SystemExit(f"expected {description}, but none were produced")
    return result


def release_filename(version: str, platform: str, arch: str, source: pathlib.Path) -> str:
    version = version.lstrip("v")
    name = source.name
    if platform == "windows":
        # Windows CLI is also .exe, so the setup marker is required.
        return f"pairroom-desktop-v{version}-windows-{arch}-setup.exe"
    if platform == "linux":
        if name.endswith(".AppImage"):
            return f"pairroom-desktop-v{version}-linux-{arch}.AppImage"
        if name.endswith(".deb"):
            return f"pairroom-desktop-v{version}-linux-{arch}.deb"
        raise SystemExit(f"unrecognized Linux package name: {name}")
    if platform == "darwin":
        return f"pairroom-desktop-v{version}-darwin-{arch}.app.zip"
    raise SystemExit(f"unsupported platform {platform!r}")


def collect(platform: str) -> list[pathlib.Path]:
    binary_dir = ROOT / "bin"
    if platform == "linux":
        appimages = require_packages(
            list(binary_dir.glob("*.AppImage")), "a Linux AppImage"
        )
        debs = require_packages(list(binary_dir.glob("*.deb")), "a Debian package")
        cli = binary_dir / "pairroom"
        if not cli.is_file():
            raise SystemExit(
                f"expected Linux PairRoom CLI next to the packages: {cli}"
            )
        return sorted({*appimages, *debs})
    if platform == "windows":
        installers = require_packages(
            list(binary_dir.glob("*-installer.exe")), "a Windows NSIS installer"
        )
        cli = binary_dir / "cli" / "pairroom.exe"
        if not cli.is_file():
            raise SystemExit(
                "expected Windows PairRoom CLI at desktop/bin/cli/pairroom.exe "
                "(PairRoom.exe and pairroom.exe cannot share one directory on NTFS)"
            )
        packages = sorted({path.resolve() for path in installers})
        for path in packages:
            if path.name.lower() in {"pairroom.exe"}:
                raise SystemExit(
                    "Windows CI must not publish a standalone PairRoom.exe; "
                    "ship the NSIS installer that contains the host and CLI"
                )
        return packages
    if platform == "darwin":
        app = binary_dir / "PairRoom.app"
        if not app.is_dir():
            raise SystemExit(f"expected macOS bundle: {app}")
        cli = app / "Contents" / "MacOS" / "pairroom"
        if not cli.is_file():
            raise SystemExit(
                f"expected PairRoom CLI inside the macOS bundle: {cli}"
            )
        archive = binary_dir / "PairRoom.app.zip"
        subprocess.run(
            [
                "ditto",
                "-c",
                "-k",
                "--sequesterRsrc",
                "--keepParent",
                str(app),
                str(archive),
            ],
            check=True,
        )
        return [archive.resolve()]
    raise SystemExit(f"unsupported platform {platform!r}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--platform", required=True, choices=["linux", "windows", "darwin"]
    )
    parser.add_argument("--arch", required=True)
    parser.add_argument("--label", required=True)
    args = parser.parse_args()

    destination = ROOT / "dist" / args.label
    if destination.exists():
        shutil.rmtree(destination)
    destination.mkdir(parents=True)

    version = (REPOSITORY / "VERSION").read_text(encoding="utf-8").strip()
    packages = []
    checksum_lines = []
    for source in collect(args.platform):
        target = destination / release_filename(version, args.platform, args.arch, source)
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
        "version": version,
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
    print(
        f"collected {len(packages)} package(s) in "
        f"{destination.relative_to(REPOSITORY)}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
