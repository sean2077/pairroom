#!/usr/bin/env python3
"""Offline regression tests: no real install, registry write, build, or daemon."""
from __future__ import annotations
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("update_local", Path(__file__).with_name("update-local.py"))
updater = importlib.util.module_from_spec(spec)
spec.loader.exec_module(updater)


class UpdateLocalTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.build = self.root / "source" / "desktop"
        self.destination = self.root / "Installed PairRoom"

    def source_files(self, platform):
        files = updater.payload(platform, self.build)
        if platform == "darwin":
            for rel in ["Contents/MacOS/PairRoom", "Contents/MacOS/pairroom", "Contents/Info.plist"]:
                path = files[0][0] / rel
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("new " + rel)
        else:
            for source, _ in files:
                source.parent.mkdir(parents=True, exist_ok=True)
                source.write_text("new " + source.name)
                source.chmod(0o755)
        return updater.validate_payload(platform, self.build)

    def test_updates_all_platform_payloads_without_deleting_other_files(self):
        for platform in ["linux", "win32", "darwin"]:
            with self.subTest(platform=platform):
                destination = self.destination / platform
                destination.mkdir(parents=True)
                unrelated = destination / "uninstall.exe"
                unrelated.write_text("preserve")
                files = self.source_files(platform)
                updater.replace_payload(destination, files)
                for source, relative in files:
                    target = destination / relative
                    self.assertTrue(target.exists())
                    if source.is_file():
                        self.assertEqual(target.read_bytes(), source.read_bytes())
                        self.assertEqual(target.stat().st_mode, source.stat().st_mode)
                updater.replace_payload(destination, files) # repeat update
                self.assertEqual(unrelated.read_text(), "preserve")
                self.assertFalse(list(destination.glob(".pairroom-desktop-update*")))

    def test_missing_cli_does_not_replace_host(self):
        self.source_files("win32")
        (self.build / "bin/cli/pairroom.exe").unlink()
        with self.assertRaisesRegex(RuntimeError, "missing or empty"):
            updater.validate_payload("win32", self.build)
        self.assertFalse(self.destination.exists())

    def test_partial_replace_rolls_back_both_binaries(self):
        files = self.source_files("win32")
        for _, relative in files:
            path = self.destination / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("old " + path.name)
        original = Path.rename
        def rename(path, destination):
            if path.name == "new-1":
                raise PermissionError("locked CLI")
            return original(path, destination)
        with patch.object(Path, "rename", rename):
            with self.assertRaisesRegex(RuntimeError, "rolled back"):
                updater.replace_payload(self.destination, files)
        for _, relative in files:
            path = self.destination / relative
            self.assertEqual(path.read_text(), "old " + path.name)
        self.assertFalse(list(self.destination.glob(".pairroom-desktop-update*")))

    def test_copy_failure_leaves_existing_install_untouched(self):
        files = self.source_files("linux")
        self.destination.mkdir()
        (self.destination / "PairRoom").write_text("old")
        with patch.object(updater.shutil, "copy2", side_effect=OSError("disk full")):
            with self.assertRaises(OSError):
                updater.replace_payload(self.destination, files)
        self.assertEqual((self.destination / "PairRoom").read_text(), "old")

    def test_refuses_concurrent_update(self):
        files = self.source_files("linux")
        lock = self.destination / ".pairroom-desktop-update.lock"
        lock.mkdir(parents=True)
        with self.assertRaisesRegex(RuntimeError, "another update"):
            updater.replace_payload(self.destination, files)
        self.assertTrue(lock.exists())

    def test_refuses_installing_into_build_output(self):
        files = self.source_files("linux")
        with self.assertRaisesRegex(RuntimeError, "build output itself"):
            updater.replace_payload(self.build / "bin", files)
        self.assertEqual((self.build / "bin/PairRoom").read_text(), "new PairRoom")

    def test_explicit_directory_and_unsupported_platform(self):
        self.assertEqual(updater.installation_directory("win32", str(self.destination)), self.destination)
        with self.assertRaisesRegex(RuntimeError, "unsupported"):
            updater.installation_directory("unknown")

    def test_windows_existing_custom_install_and_ambiguity(self):
        for name in ["custom-a", "custom-b"]:
            path = self.root / name
            path.mkdir()
            (path / "PairRoom.exe").write_text("installed")
        with patch.dict(os.environ, {"LOCALAPPDATA": str(self.root / "local")}):
            with patch.object(updater, "windows_registry_directories", return_value=[self.root / "custom-a"]):
                self.assertEqual(updater.installation_directory("win32"), self.root / "custom-a")
            with patch.object(updater, "windows_registry_directories", return_value=[self.root / "custom-a", self.root / "custom-b"]):
                with self.assertRaisesRegex(RuntimeError, "multiple desktop installations"):
                    updater.installation_directory("win32")

    def test_build_uses_production_native_task_not_daemon_or_installer(self):
        for platform, task in [("linux", "linux:build"), ("win32", "windows:build"), ("darwin", "darwin:package")]:
            with patch.dict(os.environ, {}, clear=True), \
                 patch.object(updater.shutil, "which", return_value=str(self.root / "tools/wails3")), \
                 patch.object(updater.subprocess, "check_output", return_value="amd64\n"), \
                 patch.object(updater.subprocess, "run") as run:
                updater.build(platform, "wails3")
                self.assertEqual(run.call_count, 2)
                arguments = run.call_args_list[-1].args[0]
                self.assertEqual(arguments[1:], ["task", task, "PRODUCTION=true", "ARCH=amd64"])
                self.assertEqual(run.call_args.kwargs["env"]["PAIRROOM_DESKTOP_PYTHON"], updater.sys.executable)

    def test_cross_compilation_is_rejected(self):
        with patch.dict(os.environ, {"GOOS": "windows"}), \
             patch.object(updater.shutil, "which", return_value="/tools/wails3"), \
             patch.object(updater.subprocess, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "cross-compilation"):
                updater.build("linux", "wails3")
            run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
