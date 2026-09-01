#!/usr/bin/env python3
from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import shutil


ROOT = pathlib.Path(__file__).resolve().parents[2]
EXPECTED = {
    "x86_64-unknown-linux-gnu": (("appimage", "*.AppImage"), ("deb", "*.deb")),
    "aarch64-unknown-linux-gnu": (("appimage", "*.AppImage"), ("deb", "*.deb")),
    "x86_64-pc-windows-msvc": (("msi", "*.msi"), ("nsis", "*.exe")),
    "aarch64-pc-windows-msvc": (("msi", "*.msi"), ("nsis", "*.exe")),
    "x86_64-apple-darwin": (("dmg", "*.dmg"),),
    "aarch64-apple-darwin": (("dmg", "*.dmg"),),
}


def digest(path: pathlib.Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Collect only final PairRoom desktop installers and emit checksums."
    )
    parser.add_argument("--target", required=True, help="Rust target triple")
    parser.add_argument("--label", required=True, help="Artifact label")
    parser.add_argument(
        "--output",
        default="desktop/dist",
        help="Output root, relative to repository root by default",
    )
    args = parser.parse_args()

    expected = EXPECTED.get(args.target)
    if expected is None:
        parser.error(f"unsupported desktop target {args.target!r}")

    bundle_root = (
        ROOT
        / "desktop"
        / "src-tauri"
        / "target"
        / args.target
        / "release"
        / "bundle"
    )
    output_root = pathlib.Path(args.output)
    if not output_root.is_absolute():
        output_root = ROOT / output_root
    destination = output_root.resolve() / args.label
    if destination.exists():
        shutil.rmtree(destination)
    destination.mkdir(parents=True)

    selected: list[pathlib.Path] = []
    for directory, pattern in expected:
        candidates = sorted((bundle_root / directory).glob(pattern))
        if not candidates:
            raise SystemExit(
                f"expected at least one {directory}/{pattern} installer below {bundle_root}"
            )
        for candidate in candidates:
            if not candidate.is_file() or candidate.is_symlink():
                raise SystemExit(f"installer is not a regular file: {candidate}")
            selected.append(candidate)

    names: set[str] = set()
    manifest_packages = []
    checksum_lines = []
    for source in selected:
        if source.name in names:
            raise SystemExit(f"duplicate installer basename: {source.name}")
        names.add(source.name)
        target = destination / source.name
        shutil.copy2(source, target)
        sha256 = digest(target)
        checksum_lines.append(f"{sha256}  {target.name}")
        manifest_packages.append(
            {
                "name": target.name,
                "bytes": target.stat().st_size,
                "sha256": sha256,
            }
        )

    (destination / "SHA256SUMS").write_text(
        "\n".join(checksum_lines) + "\n", encoding="utf-8", newline="\n"
    )
    version = (ROOT / "VERSION").read_text(encoding="utf-8").strip()
    manifest = {
        "product": "PairRoom",
        "version": version,
        "target": args.target,
        "artifact": args.label,
        "packages": manifest_packages,
    }
    (destination / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
        newline="\n",
    )
    print(f"collected {len(selected)} installer(s) in {destination.relative_to(ROOT)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
