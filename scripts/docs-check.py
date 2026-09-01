#!/usr/bin/env python3
"""Repository-local documentation drift checks.

The checker intentionally validates inventories that are cheap to derive from source:
maintained pages, local links, CLI commands/flags, HTTP routes, input config fields,
and a few retired compatibility claims. It does not try to lint prose style.
"""
from __future__ import annotations

import re
import sys
from pathlib import Path
from urllib.parse import unquote

ROOT = Path(__file__).resolve().parents[1]
DOCS = ROOT / "docs"
REQUIRED = {
    "README.md",
    "GETTING_STARTED.md",
    "USER_GUIDE.md",
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
    "RUNTIME_COMPATIBILITY.md",
    "PRIVACY.md",
    "RELEASING.md",
}
RETIRED = {
    "CC_CONNECT_UX_RESEARCH.md",
    "DEVELOPMENT.md",
    "FLEXIBLE_WORKFLOWS_AND_PROVIDERS.md",
    "MANAGEMENT_SHELL.md",
    "MULTI_ROOM_SERVICE.md",
    "PRODUCT_PLAN.md",
    "PULL_REQUEST.md",
    "RELEASE_CHECKLIST.md",
    "RELEASE_NOTES_v1.0.0.md",
    "RICH_CONVERSATION.md",
    "VALIDATION.md",
}
ERRORS: list[str] = []


def fail(message: str) -> None:
    ERRORS.append(message)


actual = {path.name for path in DOCS.glob("*.md")}
if missing := REQUIRED - actual:
    fail(f"required docs are missing: {sorted(missing)}")
for name in RETIRED:
    if (DOCS / name).exists():
        fail(f"retired document returned: docs/{name}")
for path in (DOCS / "images", DOCS / "validation"):
    if path.exists():
        fail(f"retired historical asset directory returned: {path.relative_to(ROOT)}")

markdown = [path for path in ROOT.rglob("*.md") if ".git" not in path.parts]


# Markdown shape: every maintained document has one H1 and balanced fences.
def markdown_shape(path: Path) -> None:
    text = path.read_text(encoding="utf-8")
    in_fence = False
    fence_char = ""
    fence_len = 0
    h1 = 0
    for line_no, line in enumerate(text.splitlines(), start=1):
        match = re.match(r"^[ ]{0,3}(`{3,}|~{3,})", line)
        if match:
            marker = match.group(1)
            if not in_fence:
                in_fence = True
                fence_char = marker[0]
                fence_len = len(marker)
            elif marker[0] == fence_char and len(marker) >= fence_len and not line[len(marker):].strip():
                in_fence = False
            continue
        if not in_fence and re.match(r"^# [^#]", line):
            h1 += 1
    if in_fence:
        fail(f"{path.relative_to(ROOT)}: unclosed fenced code block")
    if h1 != 1:
        fail(f"{path.relative_to(ROOT)}: expected exactly one level-one heading; got {h1}")

for path in [ROOT / "README.md", ROOT / "CONTRIBUTING.md", ROOT / "SECURITY.md", ROOT / "SUPPORT.md", *sorted(DOCS.glob("*.md"))]:
    markdown_shape(path)
link_pattern = re.compile(r"(?<!!)\[[^\]]*\]\(([^)]+)\)|!\[[^\]]*\]\(([^)]+)\)")
for path in markdown:
    text = path.read_text(encoding="utf-8")
    for groups in link_pattern.findall(text):
        raw = next((value for value in groups if value), "")
        target = raw.strip().split()[0].strip("<>")
        if not target or target.startswith(("http://", "https://", "mailto:", "#")):
            continue
        target = unquote(target.split("#", 1)[0])
        resolved = (path.parent / target).resolve()
        try:
            resolved.relative_to(ROOT.resolve())
        except ValueError:
            fail(f"{path.relative_to(ROOT)}: link escapes repository: {raw}")
            continue
        if not resolved.exists():
            fail(f"{path.relative_to(ROOT)}: broken link: {raw}")

# Concrete backticked repository files are maintenance contracts; catch stale paths.
source_ref = re.compile(r"`((?:cmd|internal|docs|scripts|examples|\.github)/[^`\n]+)`")
known_suffixes = {".go", ".md", ".json", ".toml", ".yaml", ".yml", ".sh", ".py", ".js", ".css", ".html"}
for path in markdown:
    text = path.read_text(encoding="utf-8")
    for raw in source_ref.findall(text):
        candidate = raw.rstrip(".,;:)").split("#", 1)[0]
        candidate = re.sub(r":\d+(?:-\d+)?$", "", candidate)
        if any(char in candidate for char in "*{}<>"):
            continue
        target = Path(candidate)
        if candidate.endswith("/"):
            target = Path(candidate.rstrip("/"))
        elif target.suffix not in known_suffixes:
            continue  # package/symbol references, not concrete files
        if not (ROOT / target).exists():
            fail(f"{path.relative_to(ROOT)}: nonexistent source path `{raw}`")

index = (DOCS / "README.md").read_text(encoding="utf-8")
for name in sorted(actual - {"README.md"}):
    if f"({name})" not in index:
        fail(f"docs/README.md does not list docs page {name}")
for name in ("README.md", "CONTRIBUTING.md", "SECURITY.md", "SUPPORT.md", "HISTORY_PROVENANCE.md"):
    if f"../{name}" not in index:
        fail(f"docs/README.md does not list root document {name}")

# Top-level commands are derived from the dispatcher.
main = (ROOT / "cmd/pairroom/main.go").read_text(encoding="utf-8")
dispatch = re.search(r"func run\(args \[\]string\) error \{(.*?)\n\}", main, re.S)
if not dispatch:
    fail("cannot find top-level CLI dispatcher")
else:
    commands = set(re.findall(r'case "([a-z][a-z0-9-]*)"', dispatch.group(1))) - {"help"}
    cli = (DOCS / "CLI_REFERENCE.md").read_text(encoding="utf-8")
    for command in sorted(commands):
        if f"`pairroom {command}`" not in cli:
            fail(f"CLI reference missing top-level command: {command}")

# All declared flags must appear at least once in the CLI reference.
flag_pattern = re.compile(r'\.(?:String|Bool|Int|Int64|Uint|Duration|Float64|StringVar|BoolVar|IntVar|DurationVar)\(\s*"([^"]+)"')
flags: set[str] = set()
for path in (ROOT / "cmd/pairroom").glob("*.go"):
    flags.update(flag_pattern.findall(path.read_text(encoding="utf-8")))
cli = (DOCS / "CLI_REFERENCE.md").read_text(encoding="utf-8")
for flag in sorted(flags):
    if f"--{flag}" not in cli:
        fail(f"CLI reference missing flag: --{flag}")

# Route inventory is derived from both HTTP muxes.
route_pattern = re.compile(r'mux\.HandleFunc\("([A-Z]+) ([^"]+)"')
api = (DOCS / "API_REFERENCE.md").read_text(encoding="utf-8")
documented_routes = set(re.findall(r"\|\s*`([A-Z]+)`\s*\|\s*`(/api/v1/[^`]+)`\s*\|", api))
source_routes: set[tuple[str, str]] = set()
for path in (ROOT / "internal/server/server.go", ROOT / "internal/service/management.go"):
    for method, route in route_pattern.findall(path.read_text(encoding="utf-8")):
        source_routes.add((method, route))
for method, route in sorted(source_routes - documented_routes):
    fail(f"API reference missing route: {method} {route}")
for method, route in sorted(documented_routes - source_routes):
    fail(f"API reference contains stale route: {method} {route}")

# Validate input configuration tags, excluding output-only summaries.
config_source = (ROOT / "internal/config/config.go").read_text(encoding="utf-8")
input_region = config_source.split("type ProviderSummary", 1)[0] + config_source.split("type File struct", 1)[1].split("func Defaults", 1)[0]
config_fields = set(re.findall(r'json:"([a-zA-Z0-9_]+)(?:,[^"]*)?"', input_region)) - {"-"}
config_doc = (DOCS / "CONFIGURATION.md").read_text(encoding="utf-8")
for field in sorted(config_fields):
    if f"`{field}`" not in config_doc and f".{field}`" not in config_doc:
        fail(f"configuration reference missing JSON field: {field}")



# Keep exact user-visible attachment limits synchronized with source constants.
attachment_source = (ROOT / "internal/attachment/store.go").read_text(encoding="utf-8")
attachment_doc = (DOCS / "USER_GUIDE.md").read_text(encoding="utf-8")
attachment_expectations = {
    r"MaxImageBytes\s+int64\s*=\s*5\s*<<\s*20": "`5 MiB`",
    r"MaxImagesPerMessage\s*=\s*8": "`8`",
    r"MaxTotalImageBytes\s+int64\s*=\s*20\s*<<\s*20": "`20 MiB`",
    r"MaxImageDimension\s*=\s*8_000": "`8000 px`",
    r"MaxImagePixels\s+int64\s*=\s*64_000_000": "`64,000,000`",
}
for source_pattern, documented in attachment_expectations.items():
    if not re.search(source_pattern, attachment_source):
        fail(f"cannot find expected attachment limit in source: {source_pattern}")
    if documented not in attachment_doc:
        fail(f"docs/USER_GUIDE.md missing attachment limit: {documented}")

# Avoid reintroducing command groups and path claims that do not exist.
if re.search(r"(?m)^\| `pairroom (?:project|room)` \|", cli):
    fail("CLI reference documents nonexistent pairroom project/room commands")
if "Room `--data-dir` 要求绝对路径" in cli:
    fail("CLI reference incorrectly claims --data-dir requires an absolute path")

protocol = (ROOT / "cmd/pairroom/protocol.go").read_text(encoding="utf-8")
if "legacy values are accepted as aliases" in protocol:
    fail("protocol help advertises removed routing compatibility")

for path in (ROOT / "README.md", DOCS / "README.md"):
    if re.search(r"\bv\d+\.\d+(?:\.\d+)?\b", path.read_text(encoding="utf-8")):
        fail(f"{path.relative_to(ROOT)} hard-codes a release version")

for path in markdown:
    text = path.read_text(encoding="utf-8")
    for name in RETIRED:
        if name in text:
            fail(f"{path.relative_to(ROOT)} still references retired {name}")

if ERRORS:
    print("documentation checks failed:", file=sys.stderr)
    for item in ERRORS:
        print(f"- {item}", file=sys.stderr)
    raise SystemExit(1)
print(f"documentation checks passed ({len(markdown)} Markdown files, {len(actual)} maintained docs pages)")
