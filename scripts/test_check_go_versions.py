#!/usr/bin/env python3
"""Offline regression tests for the build toolchain and artifact gates."""

from contextlib import redirect_stderr, redirect_stdout
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import check_go_versions as gate


class GoVersionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="pairroom go versions ")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.module = self.root / "go.mod"
        self.module.write_text("module example.test/fixture\n\ngo 1.27.0\n", encoding="utf-8")
        self.provenance = self.root / "provenance.json"
        self.provenance.write_text(json.dumps({"go_version": "go1.27.1"}), encoding="utf-8")
        self.binaries = [self.root / name for name in ("linux cli", "windows cli.exe", "darwin arm", "darwin x86")]
        for binary in self.binaries:
            binary.write_bytes(b"fixture")

    def run_gate(self, args, outputs):
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.object(gate, "run_go", side_effect=outputs) as run:
            with redirect_stdout(stdout), redirect_stderr(stderr):
                result = gate.main(args)
        return result, stdout.getvalue(), stderr.getvalue(), run

    def test_release_versions_are_numeric_not_lexicographic(self):
        self.assertGreater(gate.release_version("go1.27.10"), gate.release_version("go1.27.9"))
        self.assertEqual(gate.release_version("go1.27"), (1, 27, 0))
        for value in ("devel go1.28-abcdef", "go1.28rc1", "go1.28beta1", "1.27.1", "go1.27.1 extra", ""):
            with self.subTest(value=value), self.assertRaises(gate.VersionError):
                gate.release_version(value)

    def test_minimum_checks_module_context_and_prints_actual_toolchain(self):
        result, stdout, stderr, run = self.run_gate(
            ["minimum", str(self.module)], ["go version go1.27.1 linux/amd64"]
        )
        self.assertEqual(result, 0, stderr)
        self.assertIn("go version go1.27.1 linux/amd64", stdout)
        run.assert_called_once_with(["version"], cwd=self.root.resolve())

    def test_old_local_go_is_rejected(self):
        result, _, stderr, _ = self.run_gate(
            ["minimum", str(self.module)], ["go version go1.26.5 windows/amd64"]
        )
        self.assertEqual(result, 1)
        self.assertIn("below Go 1.27.0", stderr)
        self.assertIn("GOTOOLCHAIN=auto", stderr)

    def test_minimum_accepts_exact_minimum_and_newer_major(self):
        for version in ("go1.27.0", "go1.28.0", "go2.0.0"):
            with self.subTest(version=version):
                result, _, stderr, _ = self.run_gate(
                    ["minimum", str(self.module)], [f"go version {version} linux/amd64"]
                )
                self.assertEqual(result, 0, stderr)

    def test_module_directive_validation(self):
        for text in (
            "module example.test/no-go\n",
            "go 1.27.0\ngo 1.28.0\n",
            "go 1.27.0 extra\n",
            "go 1.27.0\ntoolchain go1.27.1\n",
        ):
            with self.subTest(text=text):
                self.module.write_text(text, encoding="utf-8")
                with self.assertRaises(gate.VersionError):
                    gate.module_minimum(self.module)
        self.module.write_text("go 1.27.0 // minimum\n", encoding="utf-8")
        self.assertEqual(gate.module_minimum(self.module), ("1.27.0", (1, 27, 0)))

    def artifact_args(self):
        return ["artifacts", "--provenance", str(self.provenance), *map(str, self.binaries)]

    def test_every_platform_matches_provenance_without_running_binaries(self):
        outputs = [f"{binary.resolve()}: go1.27.1" for binary in self.binaries]
        result, stdout, stderr, run = self.run_gate(self.artifact_args(), outputs)
        self.assertEqual(result, 0, stderr)
        self.assertEqual(run.call_count, 4)
        self.assertEqual(stdout.count("matches provenance"), 4)
        for call, binary in zip(run.call_args_list, self.binaries):
            self.assertEqual(call.args, (["version", str(binary.resolve())],))

    def test_mixed_toolchain_artifact_set_fails(self):
        # Model go version's real output for a release set with one binary
        # built by another toolchain. No old toolchain download is needed by check.
        outputs = [f"{binary.resolve()}: go1.27.1" for binary in self.binaries]
        outputs[2] = f"{self.binaries[2].resolve()}: go1.26.5"
        result, _, stderr, _ = self.run_gate(self.artifact_args(), outputs)
        self.assertEqual(result, 1)
        self.assertIn("built with go1.26.5, provenance requires go1.27.1", stderr)

    def test_uniform_binaries_still_fail_if_provenance_disagrees(self):
        outputs = [f"{binary.resolve()}: go1.27.0" for binary in self.binaries]
        result, _, stderr, _ = self.run_gate(self.artifact_args(), outputs)
        self.assertEqual(result, 1)
        self.assertIn("provenance requires go1.27.1", stderr)

    def test_missing_or_invalid_provenance_fails_closed(self):
        for value in ({}, [], {"go_version": 127}, {"go_version": "go1.28rc1"}):
            with self.subTest(value=value):
                self.provenance.write_text(json.dumps(value), encoding="utf-8")
                result, _, _, run = self.run_gate(self.artifact_args(), [])
                self.assertEqual(result, 1)
                run.assert_not_called()

    def test_missing_binary_and_unexpected_output_fail_closed(self):
        self.binaries[0].unlink()
        result, _, stderr, run = self.run_gate(self.artifact_args(), [])
        self.assertEqual(result, 1)
        self.assertIn("missing CLI artifact", stderr)
        run.assert_not_called()
        self.binaries[0].write_bytes(b"fixture")
        result, _, stderr, _ = self.run_gate(self.artifact_args(), ["not a Go executable"])
        self.assertEqual(result, 1)
        self.assertIn("unexpected go version output", stderr)
        with self.assertRaises(gate.VersionError):
            gate.check_artifacts(self.provenance, [])

    def test_go_subprocess_preserves_environment_and_reports_errors(self):
        with patch.object(gate.subprocess, "run") as run:
            run.return_value.stdout = "go version go1.27.1 linux/amd64\n"
            self.assertEqual(gate.run_go(["version"]), "go version go1.27.1 linux/amd64")
            self.assertNotIn("env", run.call_args.kwargs)
            run.side_effect = FileNotFoundError()
            with self.assertRaisesRegex(gate.VersionError, "Go is required on PATH"):
                gate.run_go(["version"])
            run.side_effect = subprocess.CalledProcessError(1, "go", stderr="requires go >= 1.27.0")
            with self.assertRaisesRegex(gate.VersionError, "requires go >= 1.27.0"):
                gate.run_go(["version"])


if __name__ == "__main__":
    unittest.main()
