#!/usr/bin/env python
"""Parse edited paths (or the hook cwd) from a Claude/Codex hook payload."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys


MAX_PAYLOAD_BYTES = 16 * 1024 * 1024


class PayloadError(ValueError):
    """The hook payload cannot be parsed safely."""


def payload() -> dict[object, object]:
    raw = sys.stdin.buffer.read(MAX_PAYLOAD_BYTES + 1)
    if len(raw) > MAX_PAYLOAD_BYTES:
        raise PayloadError(f"payload exceeds {MAX_PAYLOAD_BYTES} bytes")
    try:
        text = raw.decode("utf-8")
        value = json.loads(text)
    except (UnicodeDecodeError, ValueError) as exc:
        raise PayloadError(f"invalid UTF-8 JSON payload: {exc}") from exc
    if not isinstance(value, dict):
        raise PayloadError("payload top level must be a JSON object")
    return value


def paths(data: dict[object, object]) -> list[str]:
    tool_input = data.get("tool_input") or {}
    if not isinstance(tool_input, dict):
        tool_input = {"input": str(tool_input)}

    result: list[str] = []
    for key in ("file_path", "notebook_path", "path"):
        value = tool_input.get(key)
        if isinstance(value, str) and value:
            result.append(value)

    patch = tool_input.get("patch") or tool_input.get("input") or data.get("input")
    if isinstance(patch, str):
        for line in patch.splitlines():
            match = re.match(r"^\*\*\* (?:Add|Update|Delete) File: (.+)$", line)
            if match:
                result.append(match.group(1).strip())

    seen: set[str] = set()
    return [path for path in result if not (path in seen or seen.add(path))]


def record_value(label: str, value: str) -> str:
    if any(separator in value for separator in ("\0", "\r", "\n", "\t")):
        raise PayloadError(f"{label} contains an unsupported record separator")
    return value


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cwd", action="store_true", help="print only the payload cwd")
    parser.add_argument(
        "--records",
        action="store_true",
        help="print one typed cwd record followed by typed path records",
    )
    args = parser.parse_args()
    try:
        data = payload()
        cwd = data.get("cwd")
        if not isinstance(cwd, str) or not cwd:
            cwd = os.getcwd()
        cwd = record_value("cwd", cwd)
        edited_paths = [record_value("path", path) for path in paths(data)]
        if args.cwd:
            print(cwd)
            return 0
        if args.records:
            print(f"cwd\t{cwd}")
            for path in edited_paths:
                print(f"path\t{path}")
            return 0
        for path in edited_paths:
            print(path)
    except PayloadError as exc:
        print(f"hook-paths: {exc}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
