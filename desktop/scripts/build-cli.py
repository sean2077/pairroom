#!/usr/bin/env python3
"""Build the PairRoom CLI next to the desktop host.

A desktop package without pairroom cannot own the default data root: the
desktop host must reuse or install `pairroom daemon`. This script builds the
root-module CLI into desktop/bin using the same version ldflags as `make build`.
"""
from __future__ import annotations

import os
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
REPOSITORY = ROOT.parent
VERSION_PKG = "github.com/sean2077/pairroom/internal/version"


def git_output(*args: str) -> str:
    result = subprocess.run(
        ["git", *args],
        cwd=REPOSITORY,
        check=False,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        return ""
    return result.stdout.strip()


def cli_destination() -> pathlib.Path:
    goos = os.environ.get("GOOS") or subprocess.check_output(
        ["go", "env", "GOOS"], cwd=REPOSITORY, text=True
    ).strip()
    if goos == "windows":
        # NTFS is case-insensitive: desktop/bin/pairroom.exe would overwrite
        # PairRoom.exe. Keep the Windows CLI in a subdirectory.
        return ROOT / "bin" / "cli" / "pairroom.exe"
    return ROOT / "bin" / "pairroom"


def main() -> int:
    destination = cli_destination()
    destination.parent.mkdir(parents=True, exist_ok=True)
    commit = os.environ.get("COMMIT") or git_output("rev-parse", "HEAD") or "dev"
    last_tag = git_output("describe", "--tags", "--abbrev=0") or "unknown"
    commits = git_output("rev-list", f"{last_tag}..HEAD", "--count") or "unknown"
    build_date = os.environ.get("BUILD_DATE") or git_output(
        "show", "-s", "--format=%cI", commit
    )
    ldflags = (
        f"-s -w "
        f"-X '{VERSION_PKG}.Commit={commit}' "
        f"-X '{VERSION_PKG}.BuildDate={build_date}' "
        f"-X '{VERSION_PKG}.LastTag={last_tag}' "
        f"-X '{VERSION_PKG}.CommitsSinceTag={commits}'"
    )
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
