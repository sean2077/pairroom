#!/usr/bin/env python
"""Tests for the synchronized version bumper."""

from __future__ import annotations

import importlib.util
import sys
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("bump-version.py")
SPEC = importlib.util.spec_from_file_location("pairroom_bump_version", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
bumper = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = bumper
SPEC.loader.exec_module(bumper)


def build_fixture(root: Path, *, unreleased_body: str = "- pending note\n") -> None:
    (root / "VERSION").write_text("1.2.3\n", encoding="utf-8")
    version_dir = root / "internal" / "version"
    version_dir.mkdir(parents=True)
    (version_dir / "version.go").write_text(
        'package version\n\nconst (\n\tCurrent       = "1.2.3"\n\tStoreSchema   = 12\n)\n',
        encoding="utf-8",
    )
    desktop_dir = root / "desktop" / "build"
    desktop_dir.mkdir(parents=True)
    (desktop_dir / "config.yml").write_text(
        "version: '3'\n\ninfo:\n  productName: \"PairRoom\"\n  version: \"1.2.3\"\n\ndev_mode:\n  enabled: false\n",
        encoding="utf-8",
    )
    (root / "CHANGELOG.md").write_text(
        f"# Changelog\n\n## [Unreleased]\n\n{unreleased_body}\n## [v1.2.3] — 2026-09-01\n\n- old note\n",
        encoding="utf-8",
    )


class BumpVersionTests(unittest.TestCase):
    def test_bumps_every_surface_and_moves_unreleased(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            build_fixture(root)
            report = bumper.apply_bump(root, "1.3.0", "2026-09-17")
            self.assertEqual((root / "VERSION").read_text(encoding="utf-8"), "1.3.0\n")
            go_text = (root / "internal" / "version" / "version.go").read_text(encoding="utf-8")
            self.assertIn('Current       = "1.3.0"', go_text)
            self.assertIn("StoreSchema   = 12", go_text)
            desktop_text = (root / "desktop" / "build" / "config.yml").read_text(encoding="utf-8")
            self.assertIn("version: '3'", desktop_text)
            self.assertIn('version: "1.3.0"', desktop_text)
            changelog = (root / "CHANGELOG.md").read_text(encoding="utf-8")
            self.assertIn("## [Unreleased]\n\n## [v1.3.0] — 2026-09-17\n\n- pending note\n\n## [v1.2.3] — 2026-09-01", changelog)
            self.assertTrue(any("VERSION: 1.2.3 -> 1.3.0" in line for line in report))

    def test_empty_unreleased_warns_and_keeps_old_sections(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            build_fixture(root, unreleased_body="")
            report = bumper.apply_bump(root, "2.0.0", "2026-09-17")
            changelog = (root / "CHANGELOG.md").read_text(encoding="utf-8")
            self.assertIn("## [v2.0.0] — 2026-09-17", changelog)
            self.assertIn("## [v1.2.3] — 2026-09-01\n\n- old note", changelog)
            self.assertTrue(any("empty; fill release notes" in line for line in report))

    def test_rejects_invalid_version_and_duplicate_heading(self) -> None:
        self.assertEqual(bumper.main(["1.3", "--skip-verify"]), 2)
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            build_fixture(root)
            self.assertEqual(bumper.main(["1.2.3", "--root", str(root), "--date", "2026-09-17", "--skip-verify"]), 1)

    def test_fails_closed_before_writing_when_a_surface_is_malformed(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            build_fixture(root)
            go_file = root / "internal" / "version" / "version.go"
            go_file.write_text("package version\n", encoding="utf-8")
            with self.assertRaises(bumper.BumpError):
                bumper.apply_bump(root, "1.3.0", "2026-09-17")
            self.assertEqual((root / "VERSION").read_text(encoding="utf-8"), "1.2.3\n")
            self.assertIn("1.2.3", (root / "desktop" / "build" / "config.yml").read_text(encoding="utf-8"))
            self.assertNotIn("v1.3.0", (root / "CHANGELOG.md").read_text(encoding="utf-8"))

    def test_rejects_missing_surface(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            build_fixture(root)
            (root / "desktop" / "build" / "config.yml").unlink()
            with self.assertRaises(bumper.BumpError):
                bumper.apply_bump(root, "1.3.0", "2026-09-17")
            self.assertEqual((root / "VERSION").read_text(encoding="utf-8"), "1.2.3\n")


if __name__ == "__main__":
    unittest.main()
