#!/usr/bin/env python
"""Bump every synchronized version surface in one fail-closed step.

Rewrites the root VERSION file, internal/version.Current, the Desktop
(Wails) build config and the canonical CHANGELOG heading so a release can
never ship with drifted version surfaces again. The script edits files
only; committing, gating (`make release`) and tagging stay explicit
operator steps. Unreleased changelog bullets are moved into the new
version section. Any surface that does not match its expected shape fails
the whole bump before another surface is written.
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
import tempfile
from datetime import date
from pathlib import Path

STRICT_VERSION_RE = re.compile(r"^\d+\.\d+\.\d+$")
GO_CURRENT_RE = re.compile(r'(?m)^(\s*Current\s*=\s*")(\d+\.\d+\.\d+)("\s*)$')
DESKTOP_VERSION_RE = re.compile(r'(?m)^(\s*version:\s*")(\d+\.\d+\.\d+)("\s*)$')
UNRELEASED_HEADING = "## [Unreleased]"


class BumpError(ValueError):
    """The bump cannot be applied fail-closed; nothing was written."""


def default_root() -> Path:
    return Path(__file__).resolve().parent.parent


def read_text(path: Path) -> str:
    if not path.is_file():
        raise BumpError(f"missing version surface: {path}")
    return path.read_text(encoding="utf-8")


def substitute_once(pattern: re.Pattern, text: str, new_version: str, path: Path) -> tuple[str, str]:
    matches = pattern.findall(text)
    if len(matches) != 1:
        raise BumpError(f"{path}: expected exactly one version match, found {len(matches)}")
    old = matches[0][1] if isinstance(matches[0], tuple) else matches[0]
    replaced = pattern.sub(lambda m: m.group(1) + new_version + m.group(3), text, count=1)
    return replaced, old


def plan_changelog(text: str, new_version: str, day: str) -> tuple[str, str, bool]:
    heading = f"## [v{new_version}]"
    if any(line.startswith(heading) for line in text.splitlines()):
        raise BumpError(f"CHANGELOG.md already contains {heading}")
    if UNRELEASED_HEADING not in text:
        raise BumpError(f"CHANGELOG.md has no '{UNRELEASED_HEADING}' section")
    lines = text.splitlines(keepends=True)
    start = next(i for i, line in enumerate(lines) if line.rstrip("\r\n") == UNRELEASED_HEADING)
    end = len(lines)
    for i in range(start + 1, len(lines)):
        if lines[i].startswith("## "):
            end = i
            break
    body = "".join(lines[start + 1 : end]).strip("\r\n")
    moved = bool(body.strip())
    section = f"{heading} — {day}\n"
    if moved:
        section += f"\n{body}\n"
    rebuilt = "".join(lines[: start + 1]) + "\n" + section + "\n" + "".join(lines[end:])
    return rebuilt, body, moved


def apply_bump(root: Path, new_version: str, day: str) -> list[str]:
    report: list[str] = []

    version_file = root / "VERSION"
    old_root = read_text(version_file).strip()
    go_file = root / "internal" / "version" / "version.go"
    go_text, go_old = substitute_once(GO_CURRENT_RE, read_text(go_file), new_version, go_file)
    desktop_file = root / "desktop" / "build" / "config.yml"
    desktop_text, desktop_old = substitute_once(DESKTOP_VERSION_RE, read_text(desktop_file), new_version, desktop_file)
    changelog_file = root / "CHANGELOG.md"
    changelog_text, _, moved = plan_changelog(read_text(changelog_file), new_version, day)

    # Every surface validated; only now write.
    version_file.write_text(new_version + "\n", encoding="utf-8")
    go_file.write_text(go_text, encoding="utf-8")
    desktop_file.write_text(desktop_text, encoding="utf-8")
    changelog_file.write_text(changelog_text, encoding="utf-8")

    report.append(f"VERSION: {old_root} -> {new_version}")
    report.append(f"internal/version/version.go Current: {go_old} -> {new_version}")
    report.append(f"desktop/build/config.yml info.version: {desktop_old} -> {new_version}")
    report.append(f"CHANGELOG.md: added '## [v{new_version}] — {day}'" + (" (moved Unreleased bullets)" if moved else " (empty; fill release notes before tagging)"))
    return report


def verify(root: Path, new_version: str) -> None:
    go = subprocess.run(["go", "test", "./internal/version/..."], cwd=root)
    if go.returncode != 0:
        raise BumpError("go test ./internal/version/... failed after bump")
    with tempfile.TemporaryDirectory() as tmp:
        notes = Path(tmp) / "notes.md"
        extract = subprocess.run(
            [sys.executable, str(root / "scripts" / "extract-changelog.py"), "--changelog", str(root / "CHANGELOG.md"), "--tag", f"v{new_version}", "--output", str(notes)],
            cwd=root,
        )
        if extract.returncode != 0:
            raise BumpError(f"extract-changelog.py could not extract v{new_version} notes")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("new_version", help="strict X.Y.Z version to bump to")
    parser.add_argument("--root", default=None, help="repository root (default: this repository)")
    parser.add_argument("--date", default=None, help="changelog heading date YYYY-MM-DD (default: today from the environment)")
    parser.add_argument("--skip-verify", action="store_true", help="skip the post-bump go test and changelog extraction checks")
    args = parser.parse_args(argv)

    if not STRICT_VERSION_RE.match(args.new_version):
        print(f"error: {args.new_version!r} is not a strict X.Y.Z version", file=sys.stderr)
        return 2
    day = args.date or date.today().isoformat()
    if not re.match(r"^\d{4}-\d{2}-\d{2}$", day):
        print(f"error: --date {day!r} is not YYYY-MM-DD", file=sys.stderr)
        return 2
    root = Path(args.root).resolve() if args.root else default_root()
    try:
        for line in apply_bump(root, args.new_version, day):
            print(line)
        if not args.skip_verify:
            verify(root, args.new_version)
    except BumpError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    print()
    print("Next operator steps (this script never commits or tags):")
    print(f'  git add VERSION internal/version/version.go desktop/build/config.yml CHANGELOG.md && git commit -m "release: v{args.new_version}"')
    print("  make release")
    print(f'  git tag -a v{args.new_version} -m "v{args.new_version}" && git push origin main v{args.new_version}')
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
