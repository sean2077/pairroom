#!/usr/bin/env python3
"""Deterministic packaging guards; the Windows workflow also runs the real installer."""
from __future__ import annotations

import importlib.util
from pathlib import Path
import plistlib
import struct
import subprocess
import tempfile
import unittest
from unittest import mock

SCRIPTS = Path(__file__).resolve().parent


def load(name: str):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), SCRIPTS / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


packaging = load("package-windows")
collector = load("collect-artifacts")
prepare = load("prepare-build")
checksums = load("merge-checksums")


def pe(machine: int) -> bytes:
    header = bytearray(64)
    header[:2] = b"MZ"
    struct.pack_into("<I", header, 60, 64)
    return bytes(header) + b"PE\0\0" + struct.pack("<H", machine)


class WindowsPackagingTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="pairroom package space ")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name) / "desktop"
        for relative in ("bin/cli", "build/windows/inno"):
            (self.root / relative).mkdir(parents=True)
        (self.root.parent / "VERSION").write_text("5.0.1\n", encoding="utf-8")
        (self.root / "build/config.yml").write_text('info:\n  version: "5.0.1"\n', encoding="utf-8")
        (self.root.parent / "LICENSE").write_text("license", encoding="utf-8")
        for relative in ("bin/PairRoom.exe", "bin/cli/pairroom.exe"):
            (self.root / relative).write_bytes(pe(0x8664))
        for relative in ("build/windows/icon.ico", "build/windows/inno/PairRoom.iss",
                         "build/windows/inno/MicrosoftEdgeWebview2Setup.exe"):
            (self.root / relative).write_bytes(b"fixture")
        self.output = self.root / "bin/PairRoom-amd64-installer.exe"
        self.signature = mock.patch.object(packaging, "verify_bootstrapper")
        self.verify = self.signature.start()
        self.addCleanup(self.signature.stop)

    def test_version_is_not_another_manually_synchronized_constant(self):
        self.assertEqual(packaging.release_version(self.root), "5.0.1")
        (self.root / "build/config.yml").write_text('version: "4.0.0"\n', encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "must match"):
            packaging.release_version(self.root)

    def test_rejects_invalid_or_overflowing_versions(self):
        for version in ("5.0.1-beta", "5.0", "v5.0.1", "1.2.65536", '1.2.3" /Dfoo=bar'):
            with self.subTest(version=version):
                (self.root.parent / "VERSION").write_text(version, encoding="utf-8")
                with self.assertRaises(ValueError):
                    packaging.release_version(self.root)

    def test_host_and_cli_must_match_requested_architecture(self):
        (self.root / "bin/cli/pairroom.exe").write_bytes(pe(0xAA64))
        with self.assertRaisesRegex(ValueError, "not a Windows amd64"):
            packaging.package(self.root, "amd64", "iscc")
        self.verify.assert_not_called()

    def test_rejects_missing_empty_and_invalid_payloads(self):
        cli = self.root / "bin/cli/pairroom.exe"
        for data in (b"", b"not an executable", b"MZ" + bytes(100)):
            with self.subTest(data=data):
                cli.write_bytes(data)
                with self.assertRaises(ValueError):
                    packaging.package(self.root, "amd64", "iscc")
        cli.unlink()
        with self.assertRaisesRegex(ValueError, "missing or empty"):
            packaging.package(self.root, "amd64", "iscc")

    def test_unsupported_architecture_never_falls_through_to_arm64(self):
        with self.assertRaisesRegex(ValueError, "unsupported"):
            packaging.package(self.root, "386", "iscc")

    def test_build_uses_argument_array_and_validated_metadata(self):
        def compile_installer(command, **kwargs):
            self.assertEqual(command[0], "C:/Compiler With Spaces/ISCC.exe")
            self.assertIn("/DPairRoomVersion=5.0.1", command)
            self.assertIn("/DPairRoomArch=amd64", command)
            self.assertEqual(kwargs["cwd"], self.root)
            self.assertTrue(kwargs["check"])
            self.output.write_bytes(b"new installer")
        with mock.patch.object(packaging.subprocess, "run", side_effect=compile_installer):
            self.assertEqual(packaging.package(self.root, "amd64", "C:/Compiler With Spaces/ISCC.exe"), self.output)
        self.verify.assert_called_once()

    def test_failed_compile_removes_stale_and_partial_installer(self):
        self.output.write_bytes(b"old installer")
        def fail(command, **kwargs):
            self.assertFalse(self.output.exists())
            self.output.write_bytes(b"partial installer")
            raise subprocess.CalledProcessError(1, command)
        with mock.patch.object(packaging.subprocess, "run", side_effect=fail):
            with self.assertRaises(subprocess.CalledProcessError):
                packaging.package(self.root, "amd64", "iscc")
        self.assertFalse(self.output.exists())

    def test_successful_exit_without_output_is_a_failure(self):
        with mock.patch.object(packaging.subprocess, "run"):
            with self.assertRaisesRegex(ValueError, "missing or empty"):
                packaging.package(self.root, "amd64", "iscc")

    def test_untrusted_bootstrapper_stops_packaging(self):
        self.verify.side_effect = subprocess.CalledProcessError(1, "signature validation")
        with mock.patch.object(packaging.subprocess, "run") as run:
            with self.assertRaises(subprocess.CalledProcessError):
                packaging.package(self.root, "amd64", "iscc")
            run.assert_not_called()

    def test_missing_explicit_compiler_does_not_fall_back(self):
        with mock.patch.dict(packaging.os.environ, {"PAIRROOM_ISCC": "missing-iscc"}):
            with mock.patch.object(packaging.shutil, "which", return_value=None):
                with self.assertRaisesRegex(ValueError, "PAIRROOM_ISCC"):
                    packaging.find_compiler()

    def test_collector_selects_exact_architecture(self):
        self.output.write_bytes(b"amd64 installer")
        (self.root / "bin/PairRoom-arm64-installer.exe").write_bytes(b"arm64 installer")
        (self.root / "bin/stale-installer.exe").write_bytes(b"stale installer")
        with mock.patch.object(collector, "ROOT", self.root):
            self.assertEqual(collector.collect("windows", "amd64"), [self.output.resolve()])
            self.output.unlink()
            with self.assertRaises(SystemExit):
                collector.collect("windows", "amd64")

    def test_release_filename_stays_compatible_with_release_contract(self):
        self.assertEqual(
            collector.release_filename("5.0.1", "windows", "amd64", self.output),
            "pairroom-desktop-v5.0.1-windows-amd64-setup.exe",
        )

    def test_prepare_removes_only_generated_nsis_assets(self):
        nsis = self.root / "build/windows/nsis"
        nsis.mkdir()
        (nsis / "project.nsi").write_text("generated template", encoding="utf-8")
        with mock.patch.object(prepare, "ROOT", self.root):
            prepare.remove_unused_windows_packaging()
            prepare.remove_unused_windows_packaging()
        self.assertFalse(nsis.exists())
        self.assertTrue((self.root / "build/windows/inno/PairRoom.iss").is_file())

    def test_prepare_requires_macos_with_webview_web_locks(self):
        darwin = self.root / "build/darwin"
        darwin.mkdir(parents=True)
        plist = darwin / "Info.plist"
        # The DOCTYPE-first form and 12.0.0 default that Wails generates.
        plist.write_bytes(b'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" '
                          b'"http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n'
                          b'<plist version="1.0"><dict><key>LSMinimumSystemVersion</key>'
                          b'<string>12.0.0</string></dict></plist>\n')
        with mock.patch.object(prepare, "ROOT", self.root):
            prepare.configure_macos_bundle()
            prepare.configure_macos_bundle()
        payload = plistlib.loads(plist.read_bytes())
        # macOS 12.3 is the first WKWebView release with navigator.locks.
        self.assertEqual(payload["LSMinimumSystemVersion"], "12.3.0")
        self.assertIs(payload["NSAppTransportSecurity"]["NSAllowsLocalNetworking"], True)
        taskfile = (SCRIPTS.parent / "build/darwin/Taskfile.yml").read_text(encoding="utf-8")
        for setting in ('-mmacosx-version-min=12.3"', 'MACOSX_DEPLOYMENT_TARGET: "12.3"'):
            self.assertIn(setting, taskfile)
        self.assertNotIn("12.0", taskfile)

    def test_installer_preserves_process_and_data_boundaries(self):
        source = (SCRIPTS.parent / "build/windows/inno/PairRoom.iss").read_text(encoding="utf-8")
        for contract in ("AppId={#PairRoomId}", "UninstallDisplayName=PairRoom",
                         "WizardStyle=modern dynamic windows11", "CloseApplications=no",
                         "RestartApplications=no", "skipifsilent", "function InitializeUninstall",
                         "function PrepareToInstall", "HKLM32, WebViewKey", "NativeInt",
                         "WebView2SilentWaitMs", "WizardSilent", "ChangesEnvironment=yes",
                         "Name: addtopath", "WizardIsTaskSelected('addtopath')",
                         "RemoveCliFromPath;", "RegWriteExpandStringValue(HKLM, EnvironmentKey"):
            self.assertIn(contract, source)
        for forbidden in ("[InstallDelete]", "[UninstallDelete]", "[UninstallRun]",
                          "restartreplace", "taskkill", 'Flags: recursesubdirs',
                          "HKCU, EnvironmentKey", "Root: HKLM; Subkey:", "Flags: unchecked"):
            self.assertNotIn(forbidden, source)


class DesktopChecksumMergeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="pairroom checksums ")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def write_platform(self, label: str, names: list[str]) -> None:
        directory = self.root / f"pairroom-desktop-{label}"
        directory.mkdir()
        lines = []
        for name in names:
            (directory / name).write_bytes(name.encode())
            lines.append(f"{checksums.digest(directory / name)}  {name}")
        (directory / "SHA256SUMS").write_text("\n".join(lines) + "\n", encoding="utf-8")

    def write_all(self) -> None:
        names = checksums.required_packages("5.0.1")
        self.write_platform("windows-amd64", [names[0]])
        self.write_platform("linux-amd64", [names[1], names[2]])
        self.write_platform("macos-arm64", [names[3]])
        self.write_platform("macos-amd64", [names[4]])

    def test_merges_verified_platform_lists_in_name_order(self):
        self.write_all()
        lines = checksums.merge(self.root, "v5.0.1")
        names = [line.split("  ", 1)[1] for line in lines]
        self.assertEqual(names, sorted(checksums.required_packages("5.0.1")))
        self.assertEqual(checksums.output_name("v5.0.1"), "pairroom-desktop-v5.0.1-SHA256SUMS")

    def test_rejects_a_package_that_does_not_match_its_list(self):
        self.write_all()
        package = self.root / "pairroom-desktop-windows-amd64" / checksums.required_packages("5.0.1")[0]
        package.write_bytes(b"tampered")
        with self.assertRaisesRegex(SystemExit, "checksum mismatch"):
            checksums.merge(self.root, "5.0.1")

    def test_rejects_a_missing_platform(self):
        self.write_all()
        for path in (self.root / "pairroom-desktop-macos-amd64").iterdir():
            path.unlink()
        with self.assertRaisesRegex(SystemExit, "missing required packages"):
            checksums.merge(self.root, "5.0.1")


class BootstrapperVerificationTests(unittest.TestCase):
    def test_signature_check_uses_the_child_powershell_module_path(self):
        for variable in ("PSModulePath", "PSMODULEPATH"):
            with self.subTest(variable=variable):
                with mock.patch.dict(packaging.os.environ, {variable: "wrong-pwsh7-modules"}):
                    with mock.patch.object(packaging.subprocess, "run") as run:
                        packaging.verify_bootstrapper(Path("C:/path with spaces/runtime.exe"))
                environment = run.call_args.kwargs["env"]
                self.assertFalse(any(key.upper() == "PSMODULEPATH" for key in environment))
                self.assertEqual(environment["PAIRROOM_WEBVIEW_BOOTSTRAPPER"],
                                 str(Path("C:/path with spaces/runtime.exe")))
                self.assertTrue(run.call_args.kwargs["check"])
                self.assertIn("Get-AuthenticodeSignature", run.call_args.args[0][-1])


if __name__ == "__main__":
    unittest.main()
