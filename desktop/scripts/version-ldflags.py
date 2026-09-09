#!/usr/bin/env python3
"""Print the PairRoom version ldflags for desktop host builds.

The Desktop host embeds the PairRoom Service in-process, so the host binary
must carry the same version metadata as the CLI built by build-cli.py and
`make build`; otherwise the Management UI falls back to the bare semver
without the last tag, commit count, and short SHA. Values follow the same
precedence: COMMIT/BUILD_DATE environment overrides first, then git metadata
from the repository root, then honest development defaults.
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


def version_flags() -> str:
    commit = os.environ.get("COMMIT") or git_output("rev-parse", "HEAD") or "dev"
    last_tag = git_output("describe", "--tags", "--abbrev=0") or "unknown"
    commits = git_output("rev-list", f"{last_tag}..HEAD", "--count") or "unknown"
    build_date = os.environ.get("BUILD_DATE") or git_output(
        "show", "-s", "--format=%cI", commit
    )
    return (
        f"-X '{VERSION_PKG}.Commit={commit}' "
        f"-X '{VERSION_PKG}.BuildDate={build_date}' "
        f"-X '{VERSION_PKG}.LastTag={last_tag}' "
        f"-X '{VERSION_PKG}.CommitsSinceTag={commits}'"
    )


if __name__ == "__main__":
    sys.stdout.write(version_flags())
