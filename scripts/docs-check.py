#!/usr/bin/env python3
"""Check repository documentation without a network or Markdown dependency."""
from __future__ import annotations

import re
import subprocess
import sys
from html import unescape
from pathlib import Path
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[1]
CURATED = {
    "README.md", "WHY_PAIRROOM.md", "ALTERNATIVES.md", "GETTING_STARTED.md",
    "CONCEPTS.md", "CONFIGURATION.md", "CLI_REFERENCE.md", "API_REFERENCE.md",
    "ARCHITECTURE.md", "STORAGE.md", "OPERATIONS.md", "TROUBLESHOOTING.md",
    "UPGRADING.md", "PROTOCOL.md",
}
ERRORS: list[str] = []


def error(message: str) -> None:
    ERRORS.append(message)


def markdown_files(root: Path) -> list[Path]:
    """Include tracked and new docs; ignore deleted files and build/worktree noise."""
    if (root / ".git").exists():
        result = subprocess.run(
            ["git", "-C", str(root), "ls-files", "-z", "--cached", "--others",
             "--exclude-standard", "--", "*.md"],
            check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        paths = {root / name.decode("utf-8") for name in result.stdout.split(b"\0") if name}
    else:
        # Release source archives have no Git metadata. Do not traverse generated
        # outputs or nested development environments in that case either.
        excluded = {".git", ".worktrees", "node_modules", ".venv", ".browser-venv",
                    ".browser-results", "__pycache__", "dist", "bin", "vendor"}
        paths = {p for p in root.rglob("*.md")
                 if not excluded.intersection(p.relative_to(root).parts)}
    return sorted(p for p in paths if p.is_file() or p.is_symlink())


def prose(text: str) -> str:
    """Remove fenced examples and comments, not inline code inside link labels."""
    lines: list[str] = []
    fence = ""
    size = 0
    for line in text.splitlines():
        match = re.match(r"^ {0,3}(`{3,}|~{3,})(.*)$", line)
        if match:
            run, rest = match.groups()
            if not fence:
                fence, size = run[0], len(run)
                continue
            if run[0] == fence and len(run) >= size and not rest.strip():
                fence = ""
                continue
        if not fence:
            lines.append(line)
    return re.sub(r"<!--.*?-->", "", "\n".join(lines), flags=re.S)


def link_targets(text: str) -> list[str]:
    """Local inline/reference links and Markdown/HTML images (not remote checks)."""
    text = prose(text)
    # Strip code spans before finding links so an example `[x](missing.md)`
    # does not become a maintained link. Real link labels may contain code.
    text = re.sub(r"(`+)(.+?)\1", "", text)
    destination = r'(<[^>\n]+>|[^\s)]+)(?:\s+[\'\"][^\n]*?[\'\"])?'
    targets = re.findall(r'!?\[[^\]\n]*\]\(\s*' + destination + r'\s*\)', text)
    targets += re.findall(r'^ {0,3}\[[^\]\n]+\]:\s*(<[^>\n]+>|\S+)', text, re.M)
    targets += [m[1] for m in re.findall(
        r'<(?:img|a)\b[^>]*?\b(?:src|href)\s*=\s*([\'\"])(.*?)\1', text, re.I)]
    return [unescape(target.strip("<>")) for target in targets]


def check_links(path: Path, root: Path) -> list[str]:
    failures: list[str] = []
    label = path.relative_to(root)
    try:
        path.resolve().relative_to(root.resolve())
    except ValueError:
        return [f"{label}: document symlink escapes repository"]
    if not path.exists():
        return [f"{label}: broken document symlink"]
    for target in link_targets(path.read_text(encoding="utf-8")):
        try:
            parsed = urlsplit(target)
        except ValueError:
            failures.append(f"{label}: malformed link: {target}")
            continue
        if parsed.scheme or parsed.netloc or not parsed.path:
            continue
        resolved = (path.parent / unquote(parsed.path)).resolve()
        try:
            resolved.relative_to(root.resolve())
        except ValueError:
            failures.append(f"{label}: link escapes repository: {target}")
            continue
        if not resolved.exists():
            failures.append(f"{label}: broken link or image: {target}")
    return failures


def extract_flags() -> list[str]:
    pattern = re.compile(r'\.(?:String|Bool|Int|Int64|Uint|Duration|Float64|StringVar|BoolVar|IntVar|DurationVar)\(\s*"([^"]+)"')
    values: set[str] = set()
    for source in (ROOT / "cmd" / "pairroom").glob("*.go"):
        values.update(pattern.findall(source.read_text(encoding="utf-8")))
    return sorted(values)


def extract_routes(root: Path = ROOT) -> list[str]:
    """Inventory production registrations, retaining Go methods and wildcards."""
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
            if pattern.split(" ")[-1].startswith(("/api/", "/events")):
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
    match = re.search(
        rf'<!-- generated:{re.escape(marker)} -->(.*?)<!-- /generated:{re.escape(marker)} -->',
        path.read_text(encoding="utf-8"), flags=re.S,
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
    managed = markdown_files(ROOT)
    for path in managed:
        ERRORS.extend(check_links(path, ROOT))

    # Backticked paths in the public reference set are repository-relative.
    # Elsewhere prose can describe a different cwd; validate actual links there.
    references = [ROOT / "README.md", ROOT / "README.zh-CN.md", ROOT / "CONTRIBUTING.md",
                  *(ROOT / "docs").glob("*.md")]
    source_ref = re.compile(r'`((?:cmd|internal|docs|scripts|examples|\.github)/[^`\n]+)`')
    for path in references:
        for raw in source_ref.findall(path.read_text(encoding="utf-8")):
            candidate = raw.rstrip(".,;:)").split("#", 1)[0]
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
    print(f"documentation checks passed ({len(managed)} repository Markdown files)")
    print("Covered: " + ", ".join(str(p.relative_to(ROOT)) for p in managed))


if __name__ == "__main__":
    main()
