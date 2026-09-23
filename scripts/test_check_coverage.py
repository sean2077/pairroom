#!/usr/bin/env python3
"""Fail-closed parsing, weighting and executable exit-code regressions."""

from contextlib import redirect_stderr, redirect_stdout
from decimal import Decimal
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import check_coverage as coverage

P = coverage.PREFIX
PROFILE = (f"mode: count\n{P}internal/room/a.go:1.1,1.2 3 1\n"
           f"{P}internal/room/b.go:2.1,3.2 7 0\n"
           f"{P}internal/store/a.go:1.1,2.2 9 1\n")


class CoverageTests(unittest.TestCase):
    def quiet_check(self, totals, floors):
        with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
            return coverage.check(totals, floors)

    def test_statement_weighting_not_file_average(self):
        totals = coverage.package_coverage(PROFILE)
        self.assertEqual(totals, {"internal/room": (3, 10), "internal/store": (9, 9)})
        self.assertTrue(self.quiet_check(totals, {"internal/room": Decimal(30)}))
        self.assertFalse(self.quiet_check(totals, {"internal/room": Decimal("30.01")}))

    def test_modes_and_repeated_blocks_merge_without_double_counting(self):
        block = f"{P}internal/room/a.go:1.1,1.2 3 "
        for mode in ("set", "count", "atomic"):
            with self.subTest(mode=mode):
                totals = coverage.package_coverage(f"mode: {mode}\n{block}0\n{block}1\n")
                self.assertEqual(totals, {"internal/room": (3, 3)})

    def test_missing_and_zero_statement_packages_fail_even_at_zero_floor(self):
        for totals in ({}, {"internal/room": (0, 0)}):
            self.assertFalse(self.quiet_check(totals, {"internal/room": Decimal(0)}))

    def test_unrounded_threshold(self):
        self.assertFalse(self.quiet_check({"p": (99999, 100000)}, {"p": Decimal(100)}))
        self.assertTrue(self.quiet_check({"p": (1, 3)}, {"p": Decimal("33.333")}))

    def test_rejects_malformed_profiles(self):
        row = f"{P}internal/room/a.go:1.1,2.2 3 1\n"
        for value in ("", "mode: count\n", "mode: invalid\n" + row,
                      "mode: count\ninvalid\n", "mode: count\n" + row.replace("3 1", "-3 1"),
                      "mode: count\n" + row.replace("3 1", "3 -1"),
                      "mode: count\n" + row.replace("1.1,2.2", "3.1,2.2"),
                      "mode: count\n" + row.replace("1.1,2.2", "0.1,2.2"),
                      "mode: count\n" + row.replace(P, "example.com/foreign/"),
                      "mode: count\n" + row + row.replace("3 1", "4 1")):
            with self.subTest(profile=value), self.assertRaises(ValueError):
                coverage.package_coverage(value)

    def test_floor_validation_and_duplicate_keys(self):
        self.assertEqual(coverage.load_floors('{"p": 12.5}'), {"p": Decimal("12.5")})
        for value in ('{}', '[]', '{"p": true}', '{"p": "10"}', '{"p": -1}',
                      '{"p": 101}', '{"p": NaN}', '{"p": Infinity}',
                      '{"p": 1, "p": 2}', '{"../p": 1}', '{"p//q": 1}', 'bad'):
            with self.subTest(floors=value), self.assertRaises(ValueError):
                coverage.load_floors(value)

    def test_command_line_exit_codes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            profile, floors = root / "profile", root / "floors.json"
            profile.write_text(PROFILE, encoding="utf-8")
            for floor, expected in ((30, 0), (31, 1)):
                floors.write_text(json.dumps({"internal/room": floor}), encoding="utf-8")
                result = subprocess.run([sys.executable, coverage.__file__, str(profile),
                                         "--floors", str(floors)], capture_output=True, text=True)
                self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
                self.assertIn("internal/room", result.stdout)
            profile.unlink()
            result = subprocess.run([sys.executable, coverage.__file__, str(profile),
                                     "--floors", str(floors)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertIn("coverage check failed", result.stderr)
            self.assertNotIn("Traceback", result.stderr)


if __name__ == "__main__":
    unittest.main()
