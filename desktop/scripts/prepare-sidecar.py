#!/usr/bin/env python3
from __future__ import annotations

import argparse
import os
import pathlib
import subprocess
import sys
from dataclasses import dataclass


ROOT = pathlib.Path(__file__).resolve().parents[2]
BINARIES = ROOT / "desktop" / "src-tauri" / "binaries"
VERSION_PACKAGE = "github.com/sean2077/pairroom/internal/version"


@dataclass(frozen=True)
class Target:
    goos: str
    goarch: str
    extension: str = ""


TARGETS = {
    "x86_64-unknown-linux-gnu": Target("linux", "amd64"),
    "aarch64-unknown-linux-gnu": Target("linux", "arm64"),
    "x86_64-pc-windows-msvc": Target("windows", "amd64", ".exe"),
    "aarch64-pc-windows-msvc": Target("windows", "arm64", ".exe"),
    "x86_64-apple-darwin": Target("darwin", "amd64"),
    "aarch64-apple-darwin": Target("darwin", "arm64"),
}


def output(*args: str, fallback: str) -> str:
    try:
        value = subprocess.check_output(args, cwd=ROOT, text=True, stderr=subprocess.DEVNULL)
    except (OSError, subprocess.CalledProcessError):
        return fallback
    return value.strip() or fallback


def host_target() -> str:
    return output("rustc", "--print", "host-tuple", fallback="")


def main() -> int:
    parser = argparse.ArgumentParser(description="Build PairRoom's target-specific Tauri sidecar.")
    parser.add_argument("--target", default="", help="Rust target triple; defaults to rustc host tuple")
    args = parser.parse_args()

    triple = args.target or host_target()
    target = TARGETS.get(triple)
    if target is None:
        parser.error(f"unsupported desktop target {triple!r}; supported: {', '.join(sorted(TARGETS))}")

    version = (ROOT / "VERSION").read_text(encoding="utf-8").strip()
    commit = output("git", "rev-parse", "HEAD", fallback="dev")
    build_date = output("git", "show", "-s", "--format=%cI", commit, fallback="unknown")
    last_tag = output("git", "describe", "--tags", "--abbrev=0", fallback="unknown")
    commits_since_tag = (
        output("git", "rev-list", f"{last_tag}..HEAD", "--count", fallback="unknown")
        if last_tag != "unknown"
        else "unknown"
    )
    ldflags = " ".join(
        [
            "-s",
            "-w",
            f"-X {VERSION_PACKAGE}.Commit={commit}",
            f"-X {VERSION_PACKAGE}.BuildDate={build_date}",
            f"-X {VERSION_PACKAGE}.LastTag={last_tag}",
            f"-X {VERSION_PACKAGE}.CommitsSinceTag={commits_since_tag}",
        ]
    )

    BINARIES.mkdir(parents=True, exist_ok=True)
    destination = BINARIES / f"pairroom-sidecar-{triple}{target.extension}"
    env = os.environ.copy()
    env.update({"CGO_ENABLED": "0", "GOOS": target.goos, "GOARCH": target.goarch})
    command = [
        "go",
        "build",
        "-buildvcs=false",
        "-trimpath",
        "-ldflags",
        ldflags,
        "-o",
        str(destination),
        "./cmd/pairroom",
    ]
    print(f"building PairRoom {version} sidecar for {triple}")
    subprocess.run(command, cwd=ROOT, env=env, check=True)
    if target.goos != "windows":
        destination.chmod(0o755)
    print(destination.relative_to(ROOT))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except subprocess.CalledProcessError as exc:
        print(f"sidecar build failed with exit code {exc.returncode}", file=sys.stderr)
        raise SystemExit(exc.returncode)
