#!/usr/bin/env python3
from __future__ import annotations

import re
import sys
from pathlib import Path
from urllib.parse import unquote

ROOT = Path(__file__).resolve().parents[1]
CURATED = {
    "README.md",
    "GETTING_STARTED.md",
    "CONCEPTS.md",
    "CONFIGURATION.md",
    "CLI_REFERENCE.md",
    "API_REFERENCE.md",
    "ARCHITECTURE.md",
    "STORAGE.md",
    "OPERATIONS.md",
    "TROUBLESHOOTING.md",
    "UPGRADING.md",
    "PROTOCOL.md",
}
ERRORS: list[str] = []


def error(message: str) -> None:
    ERRORS.append(message)


def extract_flags() -> list[str]:
    pattern = re.compile(r'\.(?:String|Bool|Int|Int64|Uint|Duration|Float64|StringVar|BoolVar|IntVar|DurationVar)\(\s*"([^"]+)"')
    values: set[str] = set()
    for source in (ROOT / "cmd" / "pairroom").glob("*.go"):
        values.update(pattern.findall(source.read_text(encoding="utf-8")))
    return sorted(values)


def extract_routes(root: Path = ROOT) -> list[str]:
    """Inventory registered production routes, not arbitrary URLs in tests.

    Preserve Go 1.22 method and wildcard patterns. Resolve simple string
    constants used in registrations; fail visibly if a new expression needs
    support rather than silently dropping an API route from the contract.
    """
    sources = [source for base in (root / "internal/server", root / "internal/service")
               if base.exists() for source in base.rglob("*.go")
               if not source.name.endswith("_test.go")]
    texts = [(source, source.read_text(encoding="utf-8")) for source in sources]
    constants = dict(re.findall(r'\b(\w+)\s*=\s*"([^"\n]*)"', "\n".join(text for _, text in texts)))
    registrations = re.compile(r'\bHandle(?:Func)?\(\s*([^,\n]+),')
    values: set[str] = set()
    for source, text in texts:
        for expression in registrations.findall(text):
            parts: list[str] = []
            for token in expression.split("+"):
                token = token.strip()
                if re.fullmatch(r'"[^"\n]*"|`[^`\n]*`', token):
                    parts.append(token[1:-1])
                elif token in constants:
                    parts.append(constants[token])
                else:
                    raise ValueError(f"{source.relative_to(root)}: unsupported route expression {expression!r}")
            pattern = "".join(parts)
            path = pattern.split(" ")[-1]
            if path.startswith(("/api/", "/events")):
                values.add(pattern)
    return sorted(values)


def extract_config_fields() -> list[str]:
    pattern = re.compile(r'json:"([a-zA-Z0-9_]+)(?:,[^"]*)?"')
    values: set[str] = set()
    sources = list((ROOT / "internal" / "config").glob("*.go")) + [ROOT / "internal" / "model" / "agent_selection.go"]
    for source in sources:
        values.update(v for v in pattern.findall(source.read_text(encoding="utf-8")) if v != "-")
    return sorted(values)


def generated_values(path: Path, marker: str, prefix: str = "") -> list[str]:
    text = path.read_text(encoding="utf-8")
    match = re.search(
        rf'<!-- generated:{re.escape(marker)} -->(.*?)<!-- /generated:{re.escape(marker)} -->',
        text,
        flags=re.S,
    )
    if not match:
        error(f"{path.relative_to(ROOT)}: missing generated marker {marker}")
        return []
    return sorted(set(re.findall(r'`' + re.escape(prefix) + r'([^`]+)`', match.group(1))))


def main() -> None:
    ERRORS.clear()
    actual_docs = {p.name for p in (ROOT / "docs").glob("*.md")}
    if actual_docs != CURATED:
        error(f"docs inventory differs: missing={sorted(CURATED-actual_docs)} unexpected={sorted(actual_docs-CURATED)}")

    managed = [ROOT / "README.md", ROOT / "README.zh-CN.md", ROOT / "CONTRIBUTING.md", *(ROOT / "docs").glob("*.md")]
    link_pattern = re.compile(r'(?<!!)\[[^\]]*\]\(([^)]+)\)')
    for path in managed:
        text = path.read_text(encoding="utf-8")
        for raw in link_pattern.findall(text):
            target = raw.strip().split()[0].strip("<>")
            if not target or target.startswith(("http://", "https://", "mailto:", "#")):
                continue
            target = unquote(target.split("#", 1)[0])
            resolved = (path.parent / target).resolve()
            try:
                resolved.relative_to(ROOT.resolve())
            except ValueError:
                error(f"{path.relative_to(ROOT)}: link escapes repository: {raw}")
                continue
            if not resolved.exists():
                error(f"{path.relative_to(ROOT)}: broken link: {raw}")

    source_ref = re.compile(r'`((?:cmd|internal|docs|scripts|examples|\.github)/[^`\n]+)`')
    for path in managed:
        text = path.read_text(encoding="utf-8")
        for raw in source_ref.findall(text):
            candidate = raw.rstrip(".,;:)")
            candidate = candidate.split("#", 1)[0]
            candidate = re.sub(r':\d+(?:-\d+)?$', '', candidate)
            if any(ch in candidate for ch in "*{}<>"):
                continue
            if not (ROOT / candidate).exists():
                error(f"{path.relative_to(ROOT)}: nonexistent source path `{raw}`")

    if generated_values(ROOT / "docs" / "CLI_REFERENCE.md", "flags", "--") != extract_flags():
        error("docs/CLI_REFERENCE.md: generated flag inventory is stale")
    if generated_values(ROOT / "docs" / "API_REFERENCE.md", "routes") != extract_routes():
        error("docs/API_REFERENCE.md: generated route inventory is stale")
    if generated_values(ROOT / "docs" / "CONFIGURATION.md", "config-fields") != extract_config_fields():
        error("docs/CONFIGURATION.md: generated config field inventory is stale")

    main_source = (ROOT / "cmd" / "pairroom" / "main.go").read_text(encoding="utf-8")
    cli_doc = (ROOT / "docs" / "CLI_REFERENCE.md").read_text(encoding="utf-8")
    for command in ("daemon", "service", "serve", "doctor", "providers", "verify", "backup", "restore", "diagnostics", "protocol", "version"):
        if f'"{command}"' not in main_source:
            error(f"expected top-level command missing from source: {command}")
        if f'`pairroom {command}`' not in cli_doc:
            error(f"CLI reference missing top-level command: {command}")

    protocol_source = (ROOT / "cmd" / "pairroom" / "protocol.go").read_text(encoding="utf-8")
    if re.search(r'legacy[^\n]*(?:manual|mentions|roundtable)', protocol_source, re.I):
        error("protocol help still advertises removed routing compatibility")

    for path in (ROOT / "README.md", ROOT / "README.zh-CN.md", ROOT / "docs" / "README.md"):
        if re.search(r'\bv\d+\.\d+(?:\.\d+)?\b', path.read_text(encoding="utf-8")):
            error(f"{path.relative_to(ROOT)}: hard-coded current release")

    if ERRORS:
        print("documentation checks failed:", file=sys.stderr)
        for item in ERRORS:
            print(f"- {item}", file=sys.stderr)
        raise SystemExit(1)
    print(f"documentation checks passed ({len(managed)} maintained Markdown files)")


if __name__ == "__main__":
    main()
