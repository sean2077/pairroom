#!/usr/bin/env python3
"""Run the winget submission script offline against a recording GitHub CLI fixture."""
from __future__ import annotations

import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parent
VERSION = (SCRIPTS.parents[1] / "VERSION").read_text(encoding="utf-8").strip()
UPSTREAM_SHA = "a" * 40
FORK_SHA = "b" * 40
OTHER_SHA = "c" * 40

# This fixture never invokes the real gh executable or a network client. Unknown
# calls fail, and the raw compare response exercises the script's own validation.
GH_FIXTURE = r'''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys

root = Path(os.environ["PAIRROOM_WINGET_TEST_ROOT"])
fixture = json.loads((root / "fixture.json").read_text())
args = sys.argv[1:]
with (root / "calls.jsonl").open("a") as log:
    log.write(json.dumps(args) + "\n")

def reply(value):
    print(value)
    raise SystemExit(0)

if args[:2] == ["api", "user"]:
    reply("fixture-owner")
if args[:2] == ["pr", "list"]:
    assert os.environ["FORK_OWNER"] == "fixture-owner"
    reply(fixture.get("existing", ""))
if args[:2] in (["repo", "fork"], ["repo", "view"]):
    reply("{}")
if args[:2] == ["pr", "create"]:
    reply("https://example.invalid/winget/pull/1")
if args[0] == "api":
    endpoint = args[1]
    if endpoint == "repos/microsoft/winget-pkgs/git/refs/heads/master":
        reply(fixture["upstream"])
    if endpoint == "repos/fixture-owner/winget-pkgs/git/refs/heads/master":
        reply(fixture["fork"])
    if endpoint == "repos/fixture-owner/winget-pkgs/git/refs":
        attempts = sum(json.loads(line)[:2] == args[:2]
                       for line in (root / "calls.jsonl").read_text().splitlines())
        if fixture.get("fallback", True) and attempts == 1:
            print("gh: Not Found (HTTP 404)", file=sys.stderr)
            raise SystemExit(1)
        reply("{}")
    if endpoint.startswith("repos/fixture-owner/winget-pkgs/git/refs/heads/pairroom-") and "DELETE" in args:
        reply("{}")
    if endpoint.startswith("repos/microsoft/winget-pkgs/compare/"):
        expected = ("repos/microsoft/winget-pkgs/compare/" + fixture["upstream"]
                    + "...fixture-owner:" + fixture["fork"] + "?per_page=1")
        assert endpoint == expected, (endpoint, expected)
        if fixture.get("compare_error"):
            raise SystemExit(1)
        reply(fixture.get("raw_comparison", json.dumps(fixture["comparison"])))
    if endpoint.startswith("repos/fixture-owner/winget-pkgs/contents/") and "PUT" in args:
        reply("{}")
    if endpoint == "repos/microsoft/winget-pkgs/contents/manifests/s/sean2077/PairRoom":
        raise SystemExit(1)
print("unexpected gh fixture call: " + repr(args), file=sys.stderr)
raise SystemExit(2)
'''


@unittest.skipUnless(os.name == "posix" and shutil.which("bash"), "submission runs on a Linux release runner")
class SubmissionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="pairroom winget submission ")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        executable = self.root / "bin" / "gh"
        executable.parent.mkdir()
        executable.write_text(GH_FIXTURE, encoding="utf-8")
        executable.chmod(0o755)
        self.installer = self.root / f"pairroom-desktop-v{VERSION}-windows-amd64-setup.exe"
        self.installer.write_bytes(b"installer fixture")
        digest = hashlib.sha256(self.installer.read_bytes()).hexdigest()
        (self.root / "SHA256SUMS").write_text(f"{digest}  {self.installer.name}\n", encoding="utf-8")

    def run_submission(self, **overrides):
        fixture = {
            "upstream": UPSTREAM_SHA,
            "fork": FORK_SHA,
            "comparison": {
                "status": "behind",
                "base_commit": {"sha": UPSTREAM_SHA},
                "merge_base_commit": {"sha": FORK_SHA},
            },
        }
        fixture.update(overrides)
        (self.root / "fixture.json").write_text(json.dumps(fixture), encoding="utf-8")
        calls = self.root / "calls.jsonl"
        calls.unlink(missing_ok=True)
        environment = dict(os.environ)
        environment.update({
            "PATH": str(self.root / "bin") + os.pathsep + environment["PATH"],
            "GH_TOKEN": "fixture-token-never-sent",
            "PAIRROOM_WINGET_TEST_ROOT": str(self.root),
        })
        result = subprocess.run(
            ["bash", str(SCRIPTS / "submit-winget.sh"), "--version", VERSION,
             "--installer", str(self.installer), "--python", sys.executable],
            env=environment, text=True, capture_output=True, timeout=15,
        )
        self.calls = [json.loads(line) for line in calls.read_text().splitlines()] if calls.exists() else []
        self.assertNotIn(environment["GH_TOKEN"], json.dumps(self.calls) + result.stdout + result.stderr)
        return result

    def ref_creations(self):
        return [call for call in self.calls if call[:2] == ["api", "repos/fixture-owner/winget-pkgs/git/refs"]]

    def assert_submission(self, expected_shas):
        self.assertEqual([next(arg for arg in call if arg.startswith("sha="))
                          for call in self.ref_creations()], ["sha=" + sha for sha in expected_shas])
        uploads = [call for call in self.calls if "PUT" in call]
        expected_names = {"sean2077.PairRoom.yaml", "sean2077.PairRoom.installer.yaml",
                          "sean2077.PairRoom.locale.en-US.yaml"}
        self.assertEqual({call[1].rsplit("/", 1)[1] for call in uploads}, expected_names)
        self.assertEqual(len(uploads), 3)
        for call in uploads:
            self.assertTrue(call[1].startswith(
                f"repos/fixture-owner/winget-pkgs/contents/manifests/s/sean2077/PairRoom/{VERSION}/"))
            content = next(arg.removeprefix("content=") for arg in call if arg.startswith("content="))
            self.assertIn(f"PackageVersion: {VERSION}", base64.b64decode(content).decode())
        self.assertEqual(sum(call[:2] == ["pr", "create"] for call in self.calls), 1)

    def assert_rejected_before_fallback_writes(self, result):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(self.ref_creations()), 1, self.calls)  # failed upstream attempt only
        self.assertFalse(any("PUT" in call or call[:2] == ["pr", "create"] for call in self.calls), self.calls)

    def test_upstream_branch_does_not_need_fallback_ancestry(self):
        result = self.run_submission(fallback=False, compare_error=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_submission([UPSTREAM_SHA])
        self.assertFalse(any("/compare/" in call[1] for call in self.calls))

    def test_behind_fork_is_checked_before_creating_fallback(self):
        result = self.run_submission()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_submission([UPSTREAM_SHA, FORK_SHA])
        comparison = next(call for call in self.calls if "/compare/" in call[1])
        self.assertLess(self.calls.index(comparison), self.calls.index(self.ref_creations()[1]))

    def test_identical_fork_is_accepted(self):
        result = self.run_submission(fork=UPSTREAM_SHA, comparison={
            "status": "identical", "base_commit": {"sha": UPSTREAM_SHA},
            "merge_base_commit": {"sha": UPSTREAM_SHA},
        })
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_submission([UPSTREAM_SHA, UPSTREAM_SHA])

    def test_fork_changes_and_unknown_status_are_rejected(self):
        for status, ancestor in (("ahead", UPSTREAM_SHA), ("diverged", OTHER_SHA), ("unknown", FORK_SHA)):
            with self.subTest(status=status):
                result = self.run_submission(comparison={
                    "status": status, "base_commit": {"sha": UPSTREAM_SHA},
                    "merge_base_commit": {"sha": ancestor},
                })
                self.assert_rejected_before_fallback_writes(result)

    def test_unverified_comparisons_are_rejected(self):
        for comparison in (None, {}, {"status": "behind"},
                           {"status": "behind", "base_commit": {"sha": OTHER_SHA}, "merge_base_commit": {"sha": FORK_SHA}},
                           {"status": "behind", "base_commit": {"sha": UPSTREAM_SHA}, "merge_base_commit": {"sha": OTHER_SHA}}):
            with self.subTest(comparison=comparison):
                self.assert_rejected_before_fallback_writes(self.run_submission(comparison=comparison))

    def test_failed_or_malformed_compare_is_rejected(self):
        for response in ({"compare_error": True}, {"raw_comparison": "{"}):
            with self.subTest(response=response):
                self.assert_rejected_before_fallback_writes(self.run_submission(**response))

    def test_invalid_fork_sha_is_rejected(self):
        self.assert_rejected_before_fallback_writes(self.run_submission(fork="null"))

    def test_existing_pr_stops_before_repository_mutations(self):
        result = self.run_submission(existing="https://example.invalid/winget/pull/existing")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([call[:2] for call in self.calls], [["api", "user"], ["pr", "list"]])
        self.assertIn("pull/existing", result.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
