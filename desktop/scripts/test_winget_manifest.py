#!/usr/bin/env python3
"""Deterministic guards for the winget manifest renderer and its templates."""
from __future__ import annotations

import hashlib
import importlib.util
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parent
DESKTOP = SCRIPTS.parent
REPOSITORY = DESKTOP.parent
VERSION = (REPOSITORY / "VERSION").read_text(encoding="utf-8").strip()


def load(name: str):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), SCRIPTS / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


renderer = load("render-winget-manifest")


class RenderTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="pairroom winget ")
        self.addCleanup(temporary.cleanup)
        self.output = Path(temporary.name) / "manifests"
        self.sha = hashlib.sha256(b"installer fixture").hexdigest()

    def render(self, version: str = VERSION, release_date: str = "2026-09-17"):
        return renderer.render(
            version=version,
            installer_sha256=self.sha,
            release_date=release_date,
            output_dir=self.output,
        )

    def test_templates_are_complete(self):
        found = sorted(path.name for path in renderer.TEMPLATE_DIR.glob("*.yaml"))
        self.assertEqual(found, sorted(renderer.MANIFEST_FILES))

    def test_render_produces_all_manifests(self):
        rendered = self.render()
        self.assertEqual(sorted(path.name for path in rendered), sorted(renderer.MANIFEST_FILES))
        for path in rendered:
            self.assertTrue(path.is_file())

    def test_rendered_values_are_substituted(self):
        self.render(release_date="2026-09-17")
        installer = (self.output / f"{renderer.PACKAGE_IDENTIFIER}.installer.yaml").read_text(
            encoding="utf-8"
        )
        expected_url = (
            "https://github.com/sean2077/pairroom/releases/download/"
            f"v{VERSION}/pairroom-desktop-v{VERSION}-windows-amd64-setup.exe"
        )
        self.assertIn(f"PackageVersion: {VERSION}", installer)
        self.assertIn(f"InstallerUrl: {expected_url}", installer)
        self.assertIn(f"InstallerSha256: {self.sha.upper()}", installer)
        self.assertIn("ReleaseDate: 2026-09-17", installer)
        self.assertIn("InstallerType: inno", installer)
        self.assertIn("Scope: machine", installer)
        self.assertIn("ProductCode: com.sean2077.pairroom.desktop_is1", installer)
        self.assertIn("ManifestVersion: 1.12.0", installer)
        for name in renderer.MANIFEST_FILES:
            text = (self.output / name).read_text(encoding="utf-8")
            self.assertNotIn("{{", text)
            self.assertNotIn("}}", text)
            self.assertIn(f"PackageIdentifier: {renderer.PACKAGE_IDENTIFIER}", text)
            self.assertIn(f"PackageVersion: {VERSION}", text)

    def test_rendered_manifests_parse_as_yaml(self):
        try:
            import yaml
        except ImportError:
            self.skipTest("PyYAML is not installed")
        self.render()
        for name in renderer.MANIFEST_FILES:
            data = yaml.safe_load((self.output / name).read_text(encoding="utf-8"))
            self.assertEqual(data["PackageIdentifier"], renderer.PACKAGE_IDENTIFIER)
            self.assertEqual(data["PackageVersion"], VERSION)
            self.assertEqual(data["ManifestVersion"], "1.12.0")

    def test_rejects_malformed_version_and_date(self):
        with self.assertRaises(SystemExit):
            self.render(version="v1.2.3")
        with self.assertRaises(SystemExit):
            self.render(release_date="17.09.2026")

    def test_digest_matches_hashlib(self):
        with tempfile.TemporaryDirectory(prefix="pairroom winget digest ") as root:
            target = Path(root) / "installer.exe"
            payload = b"\x00" * 4096 + b"tail"
            target.write_bytes(payload)
            self.assertEqual(renderer.digest(target), hashlib.sha256(payload).hexdigest())


class CommandLineTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="pairroom winget cli ")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.script = SCRIPTS / "render-winget-manifest.py"

    def run_cli(self, version: str, installer_name: str):
        installer = self.root / installer_name
        installer.write_bytes(b"installer fixture")
        return subprocess.run(
            [
                sys.executable,
                str(self.script),
                "--version",
                version,
                "--installer",
                str(installer),
                "--release-date",
                "2026-09-17",
                "--output-dir",
                str(self.root / "out"),
            ],
            capture_output=True,
            text=True,
        )

    def test_happy_path(self):
        result = self.run_cli(VERSION, renderer.installer_filename(VERSION))
        self.assertEqual(result.returncode, 0, result.stderr)
        for name in renderer.MANIFEST_FILES:
            self.assertTrue((self.root / "out" / name).is_file())

    def test_rejects_version_that_disagrees_with_repository(self):
        result = self.run_cli("999.0.0", renderer.installer_filename("999.0.0"))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not match repository VERSION", result.stderr)

    def test_rejects_wrong_installer_name(self):
        result = self.run_cli(VERSION, f"pairroom-desktop-v{VERSION}-linux-amd64.AppImage")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("installer must be named", result.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
