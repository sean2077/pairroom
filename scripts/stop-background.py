#!/usr/bin/env python3
"""Stop an installed PairRoom daemon so a foreground Service can own the data root."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def run_pairroom(*args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["go", "run", "./cmd/pairroom", *args],
        cwd=ROOT,
        text=True,
        capture_output=True,
    )


def main() -> int:
    status = run_pairroom("daemon", "status")
    output = (status.stdout or "") + (status.stderr or "")
    if status.returncode != 0 and "not installed" not in output:
        sys.stderr.write(output)
        return status.returncode or 1
    if "not installed" in output:
        print("no installed pairroom daemon")
        return 0
    print(status.stdout, end="")
    if "status:   running" not in status.stdout:
        print("pairroom daemon is not running")
        return 0
    stopped = run_pairroom("daemon", "stop")
    sys.stdout.write(stopped.stdout or "")
    sys.stderr.write(stopped.stderr or "")
    return stopped.returncode


if __name__ == "__main__":
    raise SystemExit(main())
