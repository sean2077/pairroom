#!/usr/bin/env python3
"""Exercise real Git/gofmt discovery, formatting and bounded process arguments."""

from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SOURCE = Path(__file__).with_name("gofmt.py")
DIRTY = b"package fixture\nvar value=1\n"
CLEAN = b"package fixture\n\nvar value = 1\n"


class GofmtTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / "scripts").mkdir()
        shutil.copyfile(SOURCE, self.root / "scripts/gofmt.py")
        self.run_command(["git", "init", "--quiet"])

    def run_command(self, command, expected=0):
        result = subprocess.run(command, cwd=self.root, capture_output=True, text=True)
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
        return result

    def formatter(self, mode, expected=0):
        return self.run_command([sys.executable, "scripts/gofmt.py", mode], expected)

    def test_current_worktree_discovery_and_failure(self):
        for name in ("tracked.go", "deleted.go"):
            (self.root / name).write_bytes(DIRTY)
        self.run_command(["git", "add", "tracked.go", "deleted.go"])
        (self.root / "deleted.go").unlink()
        names = ("tracked.go", "space name.go", "-option.go")
        for name in names:
            (self.root / name).write_bytes(DIRTY)
        (self.root / ".gitignore").write_bytes(b".worktrees/\n")
        ignored = self.root / ".worktrees/other/unrelated.go"
        ignored.parent.mkdir(parents=True)
        ignored.write_bytes(DIRTY)

        self.formatter("--check", 1)
        for name in names:
            self.assertEqual((self.root / name).read_bytes(), DIRTY)
        self.formatter("--write")
        self.formatter("--check")
        for name in names:
            self.assertEqual((self.root / name).read_bytes(), CLEAN)
        self.assertEqual(ignored.read_bytes(), DIRTY)
        self.assertFalse((self.root / "deleted.go").exists())

        (self.root / "invalid.go").write_bytes(b"not a Go source file\n")
        self.formatter("--check", 1)
        self.formatter("--write", 1)

    def test_file_table_larger_than_windows_command_limit(self):
        directory = self.root / "space directory"
        directory.mkdir()
        paths = [directory / ("source_" + "x" * 120 + f"_{index:04}.go") for index in range(300)]
        # Even bare relative paths exceed CreateProcess's 32,767-character
        # command-line limit. This exercises batching through the real CLI.
        self.assertGreater(sum(len(str(path.relative_to(self.root))) + 1 for path in paths), 32767)
        for path in paths:
            path.write_bytes(DIRTY)
        self.formatter("--check", 1)
        self.formatter("--write")
        self.formatter("--check")
        for path in paths:
            self.assertEqual(path.read_bytes(), CLEAN)


if __name__ == "__main__":
    unittest.main()
