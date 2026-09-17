#!/usr/bin/env python3
"""Package the built Windows host and CLI with Inno Setup 7 (no implicit downloads)."""
from __future__ import annotations

import argparse
import os
from pathlib import Path
import re
import shutil
import struct
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
MACHINES = {"amd64": 0x8664, "arm64": 0xAA64}


def release_version(root: Path) -> str:
    version = (root.parent / "VERSION").read_text(encoding="utf-8").strip()
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError(f"invalid release VERSION: {version!r}")
    if any(int(part) > 65535 for part in version.split(".")):
        raise ValueError("VERSION exceeds Windows version field limits")
    config = (root / "build/config.yml").read_text(encoding="utf-8")
    match = re.search(r'(?m)^\s*version:\s*"([^"]+)"\s*$', config)
    if match is None or match.group(1) != version:
        raise ValueError("desktop/build/config.yml version must match VERSION")
    return version


def require_file(path: Path) -> None:
    if not path.is_file() or path.stat().st_size == 0:
        raise ValueError(f"missing or empty packaging input: {path}")


def check_machine(path: Path, arch: str) -> None:
    """Do not label stale output from another architecture as this package."""
    require_file(path)
    with path.open("rb") as source:
        header = source.read(64)
        if len(header) != 64 or header[:2] != b"MZ":
            raise ValueError(f"not a PE executable: {path}")
        offset = struct.unpack_from("<I", header, 60)[0]
        source.seek(offset)
        pe = source.read(6)
    if len(pe) != 6 or pe[:4] != b"PE\0\0":
        raise ValueError(f"invalid PE header: {path}")
    if struct.unpack_from("<H", pe, 4)[0] != MACHINES[arch]:
        raise ValueError(f"{path} is not a Windows {arch} executable")


def find_compiler() -> str:
    override = os.environ.get("PAIRROOM_ISCC")
    if override:
        executable = shutil.which(override)
        if not executable:
            raise ValueError(f"PAIRROOM_ISCC does not name an executable: {override}")
        return executable
    for name in ("ISCC.exe", "iscc"):
        executable = shutil.which(name)
        if executable:
            return executable
    for variable in ("ProgramFiles", "ProgramFiles(x86)"):
        if os.environ.get(variable):
            path = Path(os.environ[variable]) / "Inno Setup 7/ISCC.exe"
            if path.is_file():
                return str(path)
    raise ValueError("Inno Setup 7 not found; install it and set PAIRROOM_ISCC to ISCC.exe")


def verify_bootstrapper(path: Path) -> None:
    # Wails downloads Microsoft's Evergreen bootstrapper. Authenticate the final
    # downloaded bytes before embedding them; never execute a substituted file.
    env = dict(os.environ, PAIRROOM_WEBVIEW_BOOTSTRAPPER=str(path))
    subprocess.run(
        ["powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
         "$ErrorActionPreference = 'Stop'; "
         "$s = Get-AuthenticodeSignature -LiteralPath $env:PAIRROOM_WEBVIEW_BOOTSTRAPPER; "
         "if ($s.Status -ne 'Valid' -or "
         "$s.SignerCertificate.Subject -notmatch 'O=Microsoft Corporation(?:,|$)') "
         "{ throw 'WebView2 bootstrapper must have a valid Microsoft signature' }"],
        env=env, check=True,
    )


def package(root: Path, arch: str, compiler: str) -> Path:
    if arch not in MACHINES:
        raise ValueError(f"unsupported Windows architecture: {arch}")
    version = release_version(root)
    for relative in ("bin/PairRoom.exe", "bin/cli/pairroom.exe"):
        check_machine(root / relative, arch)
    script = root / "build/windows/inno/PairRoom.iss"
    bootstrapper = script.parent / "MicrosoftEdgeWebview2Setup.exe"
    for path in (script, bootstrapper, root / "build/windows/icon.ico", root.parent / "LICENSE"):
        require_file(path)
    verify_bootstrapper(bootstrapper)
    output = root / "bin" / f"PairRoom-{arch}-installer.exe"
    # A failed compiler must never leave last release's installer for collection.
    output.unlink(missing_ok=True)
    try:
        subprocess.run(
            [compiler, "/Qp", f"/DPairRoomVersion={version}", f"/DPairRoomArch={arch}",
             f"/O{output.parent}", f"/F{output.stem}", str(script)],
            cwd=root, check=True,
        )
        require_file(output)
    except (OSError, ValueError, subprocess.CalledProcessError):
        output.unlink(missing_ok=True)
        raise
    return output


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--arch", required=True, choices=sorted(MACHINES))
    args = parser.parse_args()
    try:
        print(f"packaged {package(ROOT, args.arch, find_compiler())}")
        return 0
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        print(f"package-windows: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
