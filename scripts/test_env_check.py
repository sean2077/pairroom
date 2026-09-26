#!/usr/bin/env python3
"""Offline regressions for the read-only `make env-check` detection rules."""
from contextlib import redirect_stdout
import importlib.util
import io
from pathlib import Path
import sys
import unittest

SCRIPT = Path(__file__).with_name("env-check.py")
SPEC = importlib.util.spec_from_file_location("pairroom_env_check", SCRIPT)
env_check = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = env_check
SPEC.loader.exec_module(env_check)

WINDOWS_PROFILE = {"USERPROFILE": "C:\\Users\\dev", "LOCALAPPDATA": "C:\\Users\\dev\\AppData\\Local",
                   "APPDATA": "C:\\Users\\dev\\AppData\\Roaming"}


class MakeTests(unittest.TestCase):
    def test_parses_gnu_make_banner(self):
        self.assertEqual(env_check.parse_make_version("GNU Make 3.81\nCopyright (C) 2006"), "3.81")
        self.assertEqual(env_check.parse_make_version("GNU Make 4.4.1\nBuilt for x86_64-pc-msys"), "4.4.1")
        self.assertIsNone(env_check.parse_make_version("bmake 20240711"))

    def test_make_3_fails_only_on_windows(self):
        self.assertEqual(env_check.make_version("3.81", windows=True).status, "FAIL")
        # macOS still ships 3.81; its recipes are not truncated.
        self.assertEqual(env_check.make_version("3.81", windows=False).status, "PASS")
        self.assertEqual(env_check.make_version("4.4.1", windows=True).status, "PASS")
        self.assertEqual(env_check.make_version(None, windows=False).status, "FAIL")


class GoTests(unittest.TestCase):
    def test_toolchain_compares_numeric_versions_with_go_mod(self):
        self.assertEqual(env_check.go_toolchain("1.27.0", "go1.27.10", "go1.27.10", "auto").status, "PASS")
        self.assertEqual(env_check.go_toolchain("1.27.0", "go1.26.5", "go1.26.5", "local").status, "FAIL")
        self.assertEqual(env_check.go_toolchain("1.27.0", None, None, "", "go not found on PATH").status, "FAIL")

    def test_auto_toolchain_rescuing_an_old_installation_warns(self):
        result = env_check.go_toolchain("1.27.0", "go1.26.5", "go1.27.0", "auto")
        self.assertEqual(result.status, "WARN")
        self.assertIn("go1.26.5", result.detail)

    def test_missing_toolchain_download_warns_instead_of_downloading(self):
        error = "go: download go1.27.2 for windows/amd64: toolchain not available"
        self.assertEqual(env_check.go_toolchain("1.27.2", "go1.27.0", None, "", error).status, "WARN")

    def test_caches_must_be_absolute_for_the_platform(self):
        good = {"GOMODCACHE": "C:\\Users\\dev\\go\\pkg\\mod", "GOCACHE": "C:\\Users\\dev\\AppData\\Local\\go-build"}
        self.assertEqual(env_check.go_caches(good, windows=True).status, "PASS")
        self.assertEqual(env_check.go_caches({**good, "GOCACHE": ""}, windows=True).status, "FAIL")
        self.assertEqual(env_check.go_caches({**good, "GOCACHE": "\\go-build"}, windows=True).status, "FAIL")
        self.assertEqual(env_check.go_caches({"GOMODCACHE": "/home/dev/go/pkg/mod", "GOCACHE": "/home/dev/.cache/go-build"},
                                             windows=False).status, "PASS")
        failed = env_check.go_caches(None, windows=True, error="module cache not found: neither GOMODCACHE nor GOPATH is set")
        self.assertEqual(failed.status, "FAIL")
        self.assertIn("USERPROFILE", failed.hint)

    def test_race_needs_cgo_and_a_resolvable_compiler(self):
        self.assertEqual(env_check.race("1", "gcc", "/usr/bin/gcc", windows=False).status, "PASS")
        self.assertEqual(env_check.race("0", "gcc", "/usr/bin/gcc", windows=False).status, "FAIL")
        missing = env_check.race("1", "gcc", None, windows=True)
        self.assertEqual(missing.status, "FAIL")
        self.assertIn("C compiler", missing.hint)

    def test_lint_version_must_match_the_pin(self):
        banner = "golangci-lint has version 2.13.2 built with go1.27.1 from (unknown)"
        self.assertEqual(env_check.parse_lint_version(banner), "v2.13.2")
        self.assertEqual(env_check.golangci_lint("v2.13.2", "v2.13.2", "golangci-lint").status, "PASS")
        self.assertEqual(env_check.golangci_lint("v2.13.2", "v2.12.0", "golangci-lint").status, "FAIL")
        self.assertEqual(env_check.golangci_lint("v2.13.2", None, "golangci-lint").status, "FAIL")


class PythonTests(unittest.TestCase):
    ALIAS = "C:\\Users\\dev\\AppData\\Local\\Microsoft\\WindowsApps\\python3.EXE"
    REAL = "C:\\Users\\dev\\AppData\\Local\\Python\\pythoncore-3.14-64\\python.exe"

    def test_windows_apps_alias_is_flagged_and_worse_without_localappdata(self):
        warned = env_check.python_command("PYTHON", "python3", self.ALIAS, self.REAL, True, WINDOWS_PROFILE)
        self.assertEqual(warned.status, "WARN")
        self.assertIn("PYTHON=C:/Users/dev/AppData/Local/Python/pythoncore-3.14-64/python.exe", warned.hint)
        self.assertEqual(env_check.python_command("PYTHON", "python3", self.ALIAS, self.REAL, True, {}).status, "FAIL")

    def test_real_interpreters_and_non_windows_paths_pass(self):
        self.assertEqual(env_check.python_command("PYTHON", "python", self.REAL, self.REAL, True, WINDOWS_PROFILE).status, "PASS")
        self.assertEqual(env_check.python_command("PYTHON", "python3", "/usr/bin/python3", "/usr/bin/python3", False, {}).status, "PASS")
        self.assertEqual(env_check.python_command("PYTHON", "python3", None, "/usr/bin/python3", False, {}).status, "FAIL")


class WindowsEnvironmentTests(unittest.TestCase):
    def test_profile_variables_are_required(self):
        self.assertEqual(env_check.windows_profile(WINDOWS_PROFILE).status, "PASS")
        result = env_check.windows_profile({"USERPROFILE": "C:\\Users\\dev"})
        self.assertEqual(result.status, "FAIL")
        self.assertIn("LOCALAPPDATA, APPDATA", result.detail)

    def test_empty_or_drive_relative_temp_fails_and_tmpdir_is_ignored(self):
        # An empty TMP makes Go's t.TempDir() drive-relative (`\TestX\001`).
        self.assertEqual(env_check.windows_temp({"TMP": "", "TEMP": "C:\\Temp", "TMPDIR": "C:\\Temp"}).status, "FAIL")
        self.assertEqual(env_check.windows_temp({"TMP": "\\Temp", "TEMP": "C:\\Temp"}).status, "FAIL")
        self.assertEqual(env_check.windows_temp({"TMP": "C:/Temp", "TEMP": "C:\\Temp", "TMPDIR": "/tmp"}).status, "PASS")
        self.assertEqual(env_check.windows_temp({"TMPDIR": "C:\\Temp"}).status, "WARN")


class LineEndingTests(unittest.TestCase):
    def test_only_crlf_working_tree_files_fail(self):
        clean = "i/lf    w/lf    attr/text=auto eol=lf \tcmd/a.go\0"
        stale = clean + "i/lf    w/crlf  attr/text=auto eol=lf \tcmd/b.go\0"
        self.assertEqual(env_check.crlf_go_files(clean).status, "PASS")
        result = env_check.crlf_go_files(stale)
        self.assertEqual(result.status, "FAIL")
        self.assertIn("cmd/b.go", result.detail)
        self.assertEqual(env_check.crlf_go_files(None).status, "WARN")


class ReportTests(unittest.TestCase):
    def test_exit_status_ignores_warnings(self):
        warn = env_check.Result("WARN", "x", "detail", "hint")
        fail = env_check.Result("FAIL", "y", "detail", "hint")
        with redirect_stdout(io.StringIO()) as out:
            self.assertEqual(env_check.report([warn]), 0)
            self.assertEqual(env_check.report([warn, fail]), 1)
        self.assertIn("-> hint", out.getvalue())


if __name__ == "__main__":
    unittest.main()
