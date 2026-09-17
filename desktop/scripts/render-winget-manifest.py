#!/usr/bin/env python3
"""Render the winget-pkgs manifest for the Windows desktop installer.

Deterministic and offline: the desktop workflow submits the rendered files
through desktop/scripts/submit-winget.sh after the installer is attached to
the GitHub Release. Placeholders live in desktop/build/winget/templates/ and
are guarded by desktop/scripts/test_winget_manifest.py.
"""
from __future__ import annotations

import argparse
import datetime
import hashlib
import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parents[1]
REPOSITORY = ROOT.parent
TEMPLATE_DIR = ROOT / "build" / "winget" / "templates"
PACKAGE_IDENTIFIER = "sean2077.PairRoom"
REPO_URL = "https://github.com/sean2077/pairroom"
MANIFEST_FILES = (
    f"{PACKAGE_IDENTIFIER}.yaml",
    f"{PACKAGE_IDENTIFIER}.locale.en-US.yaml",
    f"{PACKAGE_IDENTIFIER}.installer.yaml",
)
VERSION_PATTERN = re.compile(r"^\d+\.\d+\.\d+$")
DATE_PATTERN = re.compile(r"^\d{4}-\d{2}-\d{2}$")


def digest(path: pathlib.Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def installer_filename(version: str) -> str:
    return f"pairroom-desktop-v{version}-windows-amd64-setup.exe"


def installer_url(version: str) -> str:
    return f"{REPO_URL}/releases/download/v{version}/{installer_filename(version)}"


def render(
    version: str,
    installer_sha256: str,
    release_date: str,
    output_dir: pathlib.Path,
) -> list[pathlib.Path]:
    if not VERSION_PATTERN.match(version):
        raise SystemExit(f"version must be X.Y.Z for winget: {version!r}")
    if not DATE_PATTERN.match(release_date):
        raise SystemExit(f"release date must be YYYY-MM-DD: {release_date!r}")
    substitutions = {
        "{{PACKAGE_VERSION}}": version,
        "{{INSTALLER_SHA256}}": installer_sha256.upper(),
        "{{RELEASE_DATE}}": release_date,
        "{{INSTALLER_URL}}": installer_url(version),
    }
    output_dir.mkdir(parents=True, exist_ok=True)
    rendered = []
    for name in MANIFEST_FILES:
        template = TEMPLATE_DIR / name
        if not template.is_file():
            raise SystemExit(f"missing winget manifest template: {template}")
        text = template.read_text(encoding="utf-8")
        for placeholder, value in substitutions.items():
            text = text.replace(placeholder, value)
        if "{{" in text or "}}" in text:
            raise SystemExit(f"unresolved placeholder remains in {name}")
        target = output_dir / name
        target.write_text(text, encoding="utf-8", newline="\n")
        rendered.append(target)
    return rendered


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, help="package version without the v prefix")
    parser.add_argument(
        "--installer",
        required=True,
        type=pathlib.Path,
        help="path to the published pairroom-desktop-vX.Y.Z-windows-amd64-setup.exe",
    )
    parser.add_argument(
        "--release-date",
        default=datetime.datetime.now(datetime.timezone.utc).date().isoformat(),
        help="ReleaseDate for the installer manifest (UTC today by default)",
    )
    parser.add_argument(
        "--output-dir", required=True, type=pathlib.Path, help="directory for rendered manifests"
    )
    args = parser.parse_args()

    expected_version = (REPOSITORY / "VERSION").read_text(encoding="utf-8").strip()
    if args.version != expected_version:
        raise SystemExit(
            f"--version {args.version} does not match repository VERSION {expected_version}"
        )
    installer = args.installer
    if not installer.is_file():
        raise SystemExit(f"installer not found: {installer}")
    if installer.name != installer_filename(args.version):
        raise SystemExit(
            f"installer must be named {installer_filename(args.version)}, got {installer.name}"
        )

    rendered = render(
        version=args.version,
        installer_sha256=digest(installer),
        release_date=args.release_date,
        output_dir=args.output_dir,
    )
    for path in rendered:
        print(f"rendered {path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
