#!/usr/bin/env python3
"""Run make fmt/check on this worktree without an unbounded command line."""

import argparse
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[1]
# Leave room for executable names, quoting and the environment on both Windows
# and POSIX. The estimate includes worst-case argument quoting expansion.
ARGUMENT_BUDGET = 8192


def go_files(root: Path) -> list[str]:
    result = subprocess.run(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go"],
        cwd=root, stdout=subprocess.PIPE, check=True,
    )
    # Git owns discovery: do not traverse ignored sibling worktrees. NUL
    # delimiters preserve spaces/newlines; deleted index entries are skipped.
    return ["./" + name for name in dict.fromkeys(
        os.fsdecode(raw) for raw in result.stdout.split(b"\0") if raw
    ) if (root / name).is_file()]


def batches(files: list[str]):
    batch, size = [], 0
    for name in files:
        cost = 2 * len(os.fsencode(name)) + 3
        if cost > ARGUMENT_BUDGET:
            raise ValueError(f"Go source path exceeds argument budget: {name!r}")
        if batch and size + cost > ARGUMENT_BUDGET:
            yield batch
            batch, size = [], 0
        batch.append(name)
        size += cost
    if batch:
        yield batch


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--check", action="store_true")
    mode.add_argument("--write", action="store_true")
    args = parser.parse_args()
    dirty = False
    try:
        for batch in batches(go_files(ROOT)):
            result = subprocess.run(
                ["gofmt", "-w" if args.write else "-l", *batch], cwd=ROOT,
                stdout=subprocess.PIPE, check=True,
            )
            if result.stdout:
                sys.stdout.buffer.write(result.stdout)
                dirty = True
    except (OSError, subprocess.CalledProcessError, ValueError) as exc:
        print(f"gofmt failed: {exc}", file=sys.stderr)
        return 1
    if args.check and dirty:
        print("Go files are not gofmt-clean", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
