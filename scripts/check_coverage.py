#!/usr/bin/env python3
"""Check statement-weighted package floors against a Go coverprofile (offline)."""

import argparse
from decimal import Decimal
import json
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[1]
PREFIX = "github.com/sean2077/pairroom/"
BLOCK = re.compile(r"(.+\.go):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)")


def package_coverage(text: str) -> dict[str, tuple[int, int]]:
    lines = text.splitlines()
    if not lines or lines[0] not in {"mode: set", "mode: count", "mode: atomic"}:
        raise ValueError("missing or unsupported coverprofile mode")
    blocks = {}
    for number, line in enumerate(lines[1:], 2):
        match = BLOCK.fullmatch(line)
        if not match:
            raise ValueError(f"invalid coverprofile line {number}")
        filename, *values = match.groups()
        start_line, start_col, end_line, end_col, statements, count = map(int, values)
        if (min(start_line, start_col, end_line, end_col) < 1
                or (end_line, end_col) < (start_line, start_col)):
            raise ValueError(f"invalid source range at line {number}")
        if not filename.startswith(PREFIX) or "/" not in filename[len(PREFIX):]:
            raise ValueError(f"unexpected source path at line {number}: {filename}")
        key = (filename, start_line, start_col, end_line, end_col)
        previous = blocks.get(key)
        if previous is not None:
            if previous[0] != statements:
                raise ValueError(f"inconsistent repeated block at line {number}")
            count += previous[1]
        blocks[key] = (statements, count)
    if not blocks:
        raise ValueError("empty coverprofile")
    totals = {}
    for (filename, *_), (statements, count) in blocks.items():
        package = filename[len(PREFIX):].rsplit("/", 1)[0]
        covered, total = totals.get(package, (0, 0))
        totals[package] = (covered + (statements if count > 0 else 0), total + statements)
    return totals


def load_floors(text: str) -> dict[str, Decimal]:
    def unique_pairs(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError(f"duplicate floor: {key}")
            result[key] = value
        return result

    values = json.loads(text, parse_float=Decimal, object_pairs_hook=unique_pairs)
    if not isinstance(values, dict) or not values:
        raise ValueError("floors must be a non-empty package-to-percentage object")
    result = {}
    for package, value in values.items():
        if (not package or package.startswith("/") or "\\" in package
                or any(part in {"", ".", ".."} for part in package.split("/"))):
            raise ValueError(f"invalid package floor key: {package!r}")
        if isinstance(value, bool) or not isinstance(value, (int, Decimal)):
            raise ValueError(f"invalid floor for {package}: expected a number")
        floor = Decimal(value)
        if not floor.is_finite() or not 0 <= floor <= 100:
            raise ValueError(f"invalid floor for {package}: expected 0..100")
        result[package] = floor
    return result


def check(totals: dict[str, tuple[int, int]], floors: dict[str, Decimal]) -> bool:
    passed = True
    for package, floor in sorted(floors.items()):
        covered, total = totals.get(package, (0, 0))
        if total == 0:
            print(f"FAIL {package}: missing statement coverage", file=sys.stderr)
            passed = False
            continue
        # Compare unrounded, statement-weighted values. A printed rounded-up
        # percentage must not turn a below-floor result into a pass.
        ok = Decimal(covered * 100) >= floor * total
        actual = Decimal(covered * 100) / total
        print(f"{'PASS' if ok else 'FAIL'} {package}: {actual:.3f}% >= {floor}% "
              f"({covered}/{total} statements)")
        passed = passed and ok
    return passed


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("profile", nargs="?", type=Path, default=ROOT / ".coverage")
    parser.add_argument("--floors", type=Path, default=ROOT / "scripts/coverage-floors.json")
    args = parser.parse_args(argv)
    try:
        totals = package_coverage(args.profile.read_text(encoding="utf-8"))
        floors = load_floors(args.floors.read_text(encoding="utf-8"))
        return 0 if check(totals, floors) else 1
    except (OSError, ValueError) as exc:
        print(f"coverage check failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
