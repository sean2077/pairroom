#!/usr/bin/env python3
"""Build the PairRoom CLI next to the desktop host.

The bundled CLI supports explicit daemon administration and diagnostics.
Desktop startup never installs a daemon. This script builds the root-module
CLI into desktop/bin using the same version ldflags as `make build`.
"""
from __future__ import annotations

import importlib.util
import os
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
REPOSITORY = ROOT.parent


def _version_ldflags_module():
    path = pathlib.Path(__file__).resolve().parent / "version-ldflags.py"
    spec = importlib.util.spec_from_file_location("pairroom_version_ldflags", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


# The bundled CLI and the desktop host must never drift on version metadata.
VERSION_FLAGS = _version_ldflags_module().version_flags()


def cli_destination() -> pathlib.Path:
    goos = os.environ.get("GOOS") or subprocess.check_output(
        ["go", "env", "GOOS"], cwd=REPOSITORY, text=True
    ).strip()
    if goos == "windows":
        # NTFS is case-insensitive: desktop/bin/pairroom.exe would overwrite
        # PairRoom.exe. Keep the Windows CLI in a subdirectory.
        return ROOT / "bin" / "cli" / "pairroom.exe"
    if goos == "darwin":
        # Default macOS volumes are also case-insensitive: do not replace PairRoom.
        return ROOT / "bin" / "cli" / "pairroom"
    return ROOT / "bin" / "pairroom"


def main() -> int:
    destination = cli_destination()
    destination.parent.mkdir(parents=True, exist_ok=True)
    ldflags = f"-s -w {VERSION_FLAGS}"
    env = os.environ.copy()
    env["CGO_ENABLED"] = env.get("CGO_ENABLED", "0")
    subprocess.run(
        [
            "go",
            "build",
            "-buildvcs=false",
            "-trimpath",
            f"-ldflags={ldflags}",
            "-o",
            str(destination),
            "./cmd/pairroom",
        ],
        cwd=REPOSITORY,
        check=True,
        env=env,
    )
    print(f"built PairRoom CLI {destination.relative_to(REPOSITORY)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
