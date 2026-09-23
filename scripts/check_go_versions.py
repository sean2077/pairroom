#!/usr/bin/env python3
"""Check module minimums and CLI/provenance toolchain consistency, without dependencies."""

import argparse
import json
from pathlib import Path
import re
import subprocess
import sys
from typing import Sequence


class VersionError(ValueError):
    """A toolchain or its release metadata violates the build contract."""


def release_version(value: str) -> tuple[int, int, int]:
    match = re.fullmatch(r"go(\d+)\.(\d+)(?:\.(\d+))?", value)
    if not match:
        raise VersionError(f"expected a stable Go version, got {value!r}")
    return tuple(int(part or 0) for part in match.groups())


def run_go(arguments: Sequence[str], *, cwd: Path | None = None) -> str:
    # Preserve GOTOOLCHAIN: auto may upgrade an old local installation; local
    # must fail rather than silently bypassing the module's minimum version.
    try:
        result = subprocess.run(
            ["go", *arguments], cwd=cwd, check=True, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
    except FileNotFoundError as exc:
        raise VersionError("Go is required on PATH") from exc
    except subprocess.CalledProcessError as exc:
        detail = (exc.stderr or exc.stdout or str(exc)).strip()
        raise VersionError(f"go {' '.join(arguments)} failed: {detail}") from exc
    return result.stdout.strip()


def module_minimum(path: Path) -> tuple[str, tuple[int, int, int]]:
    directives = []
    for line in path.read_text(encoding="utf-8").splitlines():
        fields = line.partition("//")[0].split()
        if not fields:
            continue
        if fields[0] == "toolchain":
            raise VersionError(f"{path}: do not pin a toolchain; CI selects stable")
        if fields[0] == "go":
            if len(fields) != 2:
                raise VersionError(f"{path}: malformed go directive")
            directives.append(fields[1])
    if len(directives) != 1:
        raise VersionError(f"{path}: expected exactly one go directive")
    value = directives[0]
    return value, release_version("go" + value)


def check_minimum(path: Path) -> None:
    path = path.resolve()
    required, minimum = module_minimum(path)
    output = run_go(["version"], cwd=path.parent)
    print(output, flush=True)
    fields = output.split()
    if len(fields) != 4 or fields[:2] != ["go", "version"]:
        raise VersionError(f"unexpected go version output: {output!r}")
    actual = fields[2]
    if release_version(actual) < minimum:
        raise VersionError(
            f"{path}: {actual} is below Go {required}; install the latest stable "
            "Go or use GOTOOLCHAIN=auto"
        )
    print(f"{path.name}: {actual} satisfies minimum Go {required}")


def check_artifacts(provenance: Path, binaries: Sequence[Path]) -> None:
    if not binaries:
        raise VersionError("at least one CLI artifact is required")
    metadata = json.loads(provenance.read_text(encoding="utf-8"))
    expected = metadata.get("go_version") if isinstance(metadata, dict) else None
    if not isinstance(expected, str):
        raise VersionError(f"{provenance}: missing string go_version")
    release_version(expected)
    for binary in binaries:
        path = binary.resolve()
        if not path.is_file():
            raise VersionError(f"missing CLI artifact: {binary}")
        output = run_go(["version", str(path)])
        # Split from the right: absolute Windows paths and filenames may have
        # spaces or colons. Do not execute a cross-compiled CLI to inspect it.
        prefix, separator, actual = output.rpartition(": ")
        if not separator or prefix != str(path):
            raise VersionError(f"unexpected go version output for {binary}: {output!r}")
        release_version(actual)
        if actual != expected:
            raise VersionError(
                f"{binary}: built with {actual}, provenance requires {expected}; "
                "all CLI artifacts must use the same Go toolchain"
            )
        print(f"{binary.name}: {actual} matches provenance")


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    minimum = commands.add_parser("minimum", help="check actual Go against go.mod")
    minimum.add_argument("modules", nargs="*", type=Path, default=[Path("go.mod")])
    artifacts = commands.add_parser("artifacts", help="compare CLI build versions to provenance")
    artifacts.add_argument("--provenance", required=True, type=Path)
    artifacts.add_argument("binaries", nargs="+", type=Path)
    args = parser.parse_args(argv)
    try:
        if args.command == "minimum":
            for module in args.modules:
                check_minimum(module)
        else:
            check_artifacts(args.provenance, args.binaries)
    except (OSError, ValueError) as exc:
        print(f"Go version check failed: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
