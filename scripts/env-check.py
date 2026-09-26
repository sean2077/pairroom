#!/usr/bin/env python3
"""Report whether this machine can run `make check`, without changing anything."""
from __future__ import annotations

import argparse
from dataclasses import dataclass
import json
import ntpath
import os
import posixpath
from pathlib import Path, PureWindowsPath
import re
import shutil
import subprocess
import sys
from typing import Mapping, Sequence

sys.dont_write_bytecode = True  # a diagnostic must not leave __pycache__ behind
import check_go_versions  # noqa: E402

ROOT = Path(__file__).resolve().parents[1]
GUIDE = "CONTRIBUTING.md#windows-development"
GO_VARS = ("GOVERSION", "GOTOOLCHAIN", "CGO_ENABLED", "CC", "GOMODCACHE", "GOCACHE")


@dataclass(frozen=True)
class Result:
    status: str  # PASS, WARN or FAIL
    name: str
    detail: str
    hint: str = ""


def go_toolchain(required: str, installed: str | None, effective: str | None,
                 toolchain: str, error: str = "") -> Result:
    name = "Go toolchain"
    if installed is None and effective is None:
        return Result("FAIL", name, f"cannot run go: {error or 'not found on PATH'}",
                      "Install the latest stable Go release and put it on PATH")
    try:
        minimum = check_go_versions.release_version("go" + required)
        local = check_go_versions.release_version(installed) if installed else None
        selected = check_go_versions.release_version(effective) if effective else None
    except check_go_versions.VersionError as exc:
        return Result("WARN", name, f"cannot compare with go.mod minimum {required}: {exc}")
    if selected is None:
        if "toolchain not available" not in error:
            return Result("WARN", name, f"cannot determine the selected Go toolchain: {error}")
        return Result("WARN", name, f"installed {installed} is below go.mod minimum {required}; the next online go command downloads go{required}",
                      "Install the latest stable Go release")
    if selected < minimum:
        return Result("FAIL", name, f"{effective} is below go.mod minimum {required}",
                      "Install the latest stable Go release")
    if local is not None and local < minimum:
        return Result("WARN", name, f"installed {installed} is below go.mod minimum {required}; GOTOOLCHAIN={toolchain} selects {effective}",
                      "Install the latest stable Go release; GOTOOLCHAIN=local would fail")
    return Result("PASS", name, f"{effective} satisfies go.mod minimum {required}")


def go_caches(values: Mapping[str, str] | None, windows: bool, error: str = "") -> Result:
    name = "Go caches"
    hint = (f"Windows profile variables (USERPROFILE, LOCALAPPDATA) or GOPATH are missing; see {GUIDE}"
            if windows else "Check `go env GOMODCACHE GOCACHE`")
    if values is None:
        return Result("FAIL", name, f"go env failed: {error}", hint)
    bad = [f"{key}={values.get(key, '')!r}" for key in ("GOMODCACHE", "GOCACHE")
           if not is_absolute(values.get(key, ""), windows)]
    if bad:
        return Result("FAIL", name, "not absolute paths: " + ", ".join(bad), hint)
    return Result("PASS", name, f"GOMODCACHE={values['GOMODCACHE']}, GOCACHE={values['GOCACHE']}")


def race(cgo_enabled: str, cc: str, cc_path: str | None, windows: bool) -> Result:
    name = "Race detector"
    compiler = ("Install a Go-supported C compiler (for example MSYS2 UCRT64 gcc) and put its bin directory on PATH"
                if windows else "Install a C compiler (gcc or clang) on PATH")
    enable = "Export CGO_ENABLED=1 or run `go env -w CGO_ENABLED=1` (check `go env GOENV` for a persisted 0)"
    if cgo_enabled == "1" and cc_path:
        return Result("PASS", name, f"CGO_ENABLED=1, CC={cc} -> {cc_path}")
    if cgo_enabled == "1":
        return Result("FAIL", name, f"CC={cc} is not on PATH; make race cannot build", compiler)
    if cc_path:
        return Result("FAIL", name, f"make race requires CGO_ENABLED=1 (found {cc_path})", enable)
    return Result("FAIL", name, f"make race requires CGO_ENABLED=1 and a C compiler; CC={cc} is not on PATH",
                  f"{compiler}. {enable}")


def parse_make_version(output: str) -> str | None:
    match = re.match(r"GNU Make (\S+)", output)
    return match.group(1) if match else None


def make_version(version: str | None, windows: bool) -> Result:
    name = "GNU Make"
    if version is None:
        return Result("FAIL", name, "GNU Make not found on PATH", "Install GNU Make 4 or newer")
    match = re.match(r"(\d+)\.(\d+)", version)
    if not match:
        return Result("WARN", name, f"cannot parse version {version!r}")
    if windows and int(match.group(1)) < 4:
        return Result("FAIL", name, f"{version} on Windows truncates long recipes (make check fails with a shell syntax error)",
                      f"Use GNU Make 4 or newer, such as MSYS2 make; see {GUIDE}")
    return Result("PASS", name, version)


def is_windows_apps_alias(path: str) -> bool:
    return "\\microsoft\\windowsapps\\" in path.replace("/", "\\").lower()


def python_command(label: str, command: str, resolved: str | None, current: str,
                   windows: bool, env: Mapping[str, str]) -> Result:
    name = f"Make {label}"
    suggestion = PureWindowsPath(current).as_posix() if windows else current
    if resolved is None:
        return Result("FAIL", name, f"{command} is not on PATH", f"Pass {label}={suggestion} to make")
    if windows and is_windows_apps_alias(resolved):
        # The alias either opens the Store or starts the Python install manager,
        # which installs into the current directory when LOCALAPPDATA is missing.
        status = "WARN" if env.get("LOCALAPPDATA") else "FAIL"
        return Result(status, name, f"{command} resolves to the WindowsApps alias {resolved}",
                      "Without LOCALAPPDATA it deploys a Python/ directory into the working tree; "
                      f"pass {label}={suggestion} to make")
    return Result("PASS", name, f"{command} -> {resolved}")


def node(version: str | None) -> Result:
    if version is None:
        return Result("FAIL", "Node.js", "node not found on PATH", "Install Node.js (CI uses 22.x); make js-check requires it")
    return Result("PASS", "Node.js", version)


def parse_lint_version(output: str) -> str | None:
    match = re.search(r"^golangci-lint has version (\S+)", output, re.M)
    return "v" + match.group(1).removeprefix("v") if match else None


def golangci_lint(expected: str, actual: str | None, command: str) -> Result:
    name = "golangci-lint"
    if actual is None:
        return Result("FAIL", name, f"{command} not found or unreadable", "Run make lint-install and add GOBIN to PATH")
    if actual != expected:
        return Result("FAIL", name, f"found {actual}, make lint requires {expected}", "Run make lint-install")
    return Result("PASS", name, actual)


def is_absolute(value: str, windows: bool) -> bool:
    if not windows:
        return posixpath.isabs(value)
    drive, rest = ntpath.splitdrive(value)
    return bool(drive) and rest[:1] in ("\\", "/")


def windows_profile(env: Mapping[str, str]) -> Result:
    missing = [key for key in ("USERPROFILE", "LOCALAPPDATA", "APPDATA") if not env.get(key)]
    if missing:
        return Result("FAIL", "Windows profile", "missing " + ", ".join(missing),
                      "Go caches and desktop tests need them; they are dropped when a command crosses MSYS "
                      f"runtimes (for example Git Bash running MSYS2 make); see {GUIDE}")
    return Result("PASS", "Windows profile", "USERPROFILE, LOCALAPPDATA and APPDATA are set")


def windows_temp(env: Mapping[str, str]) -> Result:
    # Go on Windows takes its temporary directory from TMP, then TEMP; it ignores TMPDIR.
    defined = [key for key in ("TMP", "TEMP") if key in env]
    if not defined:
        return Result("WARN", "Windows temp", "TMP and TEMP are unset; Go falls back to USERPROFILE or the Windows directory",
                      "Set TMP and TEMP to an absolute Windows path")
    bad = [f"{key}={env[key]!r}" for key in defined if not is_absolute(env[key], True)]
    if bad:
        return Result("FAIL", "Windows temp", ", ".join(bad) + " is not an absolute Windows path",
                      "t.TempDir() becomes drive-relative and Service tests reject their data root; "
                      "set TMP and TEMP to an absolute path such as the profile's AppData/Local/Temp")
    return Result("PASS", "Windows temp", ", ".join(f"{key}={env[key]}" for key in defined))


def crlf_go_files(listing: str | None) -> Result:
    name = "Go line endings"
    if listing is None:
        return Result("WARN", name, "cannot list working-tree line endings with git")
    crlf = [record.split("\t", 1)[1] for record in listing.split("\0")
            if "\t" in record and record.split()[1:2] == ["w/crlf"]]
    if crlf:
        return Result("FAIL", name, f"{len(crlf)} tracked Go file(s) are CRLF in the working tree, e.g. {crlf[0]}",
                      "gofmt reports them although the committed files are LF; delete those files and `git restore` them")
    return Result("PASS", name, "tracked Go files are LF in the working tree")


def run(command: Sequence[str], env: Mapping[str, str] | None = None) -> tuple[int, str, str] | None:
    try:
        result = subprocess.run(list(command), cwd=ROOT, env=None if env is None else {**os.environ, **env},
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120)
    except (OSError, subprocess.TimeoutExpired):
        return None
    return result.returncode, result.stdout, result.stderr.strip()


def go_env(extra: Mapping[str, str]) -> tuple[dict[str, str] | None, str]:
    # GOPROXY=off reports a missing toolchain instead of downloading it.
    result = run(["go", "env", "-json", *GO_VARS], {"GOPROXY": "off", **extra})
    if result is None:
        return None, "go not found on PATH"
    code, stdout, stderr = result
    if code != 0:
        return None, stderr or f"exit status {code}"
    return json.loads(stdout), ""


def default_python() -> str:
    # Mirrors the Makefile's PYTHON default.
    for command in ("python3", "python"):
        if shutil.which(command):
            return command
    return "python3"


def makefile_lint_version() -> str:
    text = (ROOT / "Makefile").read_text(encoding="utf-8")
    match = re.search(r"^GOLANGCI_LINT_VERSION\s*:?=\s*(\S+)", text, re.M)
    return match.group(1) if match else "unknown"


def first_line(command: Sequence[str]) -> str | None:
    result = run(command)
    if result is None or result[0] != 0:
        return None
    return (result[1].splitlines() or [""])[0].strip()


def collect(args: argparse.Namespace, windows: bool) -> list[Result]:
    results = []
    if args.make_version is not None:
        results.append(make_version(args.make_version, windows))
    else:
        output = first_line(["make", "--version"])
        results.append(make_version(parse_make_version(output) if output else None, windows))

    required, _ = check_go_versions.module_minimum(ROOT / "go.mod")
    local, local_error = go_env({"GOTOOLCHAIN": "local"})
    selected, error = go_env({})
    values = selected or local
    results.append(go_toolchain(required, local["GOVERSION"] if local else None,
                                selected["GOVERSION"] if selected else None,
                                selected["GOTOOLCHAIN"] if selected else "", error))
    results.append(go_caches(values, windows, local_error or error))
    if values:
        cc = values["CC"]
        results.append(race(values["CGO_ENABLED"], cc, shutil.which(cc) or shutil.which(cc.split()[0]) if cc else None, windows))

    python = args.python or default_python()
    desktop = args.desktop_python or ("python" if windows else python)
    current = sys.executable
    for label, command in (("PYTHON", python), ("DESKTOP_PYTHON", desktop)):
        if label == "DESKTOP_PYTHON" and command == python:
            continue
        results.append(python_command(label, command, shutil.which(command), current, windows, os.environ))

    results.append(node(first_line(["node", "--version"])))
    lint = run([args.golangci_lint, "version"])
    results.append(golangci_lint(args.golangci_lint_version or makefile_lint_version(),
                                 parse_lint_version(lint[1] + "\n" + lint[2]) if lint else None, args.golangci_lint))
    if windows:
        results.append(windows_profile(os.environ))
        results.append(windows_temp(os.environ))
    listing = run(["git", "ls-files", "-z", "--eol", "--", "*.go"])
    results.append(crlf_go_files(listing[1] if listing and listing[0] == 0 else None))
    return results


def report(results: Sequence[Result]) -> int:
    for result in results:
        print(f"{result.status:<4}  {result.name:<19} {result.detail}")
        if result.hint and result.status != "PASS":
            print(f"{'':25}-> {result.hint}")
    failed = sum(r.status == "FAIL" for r in results)
    warned = sum(r.status == "WARN" for r in results)
    print(f"env-check: {failed} FAIL, {warned} WARN, {len(results) - failed - warned} PASS")
    return 1 if failed else 0


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--make-version", help="version of the running make (the Makefile passes MAKE_VERSION)")
    parser.add_argument("--python", help="the Makefile's PYTHON")
    parser.add_argument("--desktop-python", help="the Makefile's DESKTOP_PYTHON")
    parser.add_argument("--golangci-lint", default="golangci-lint", help="the Makefile's GOLANGCI_LINT")
    parser.add_argument("--golangci-lint-version", help="the Makefile's GOLANGCI_LINT_VERSION")
    args = parser.parse_args(argv)
    return report(collect(args, os.name == "nt"))


if __name__ == "__main__":
    sys.exit(main())
