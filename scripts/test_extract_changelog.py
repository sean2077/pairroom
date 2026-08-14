#!/usr/bin/env python
"""Tests for the repository-owned changelog release-note extractor."""

from __future__ import annotations

import importlib.util
import sys
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("extract-changelog.py")
SPEC = importlib.util.spec_from_file_location("pairroom_extract_changelog", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
extractor = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = extractor
SPEC.loader.exec_module(extractor)


class ExtractChangelogTests(unittest.TestCase):
    def test_extracts_only_the_exact_section(self) -> None:
        text = """# Changelog

## [v1.2.0] — 2026-08-14

### Added

- current

## [v1.1.0] — 2026-08-01

- previous
"""
        self.assertEqual(extractor.extract_notes(text, "v1.2.0"), "### Added\n\n- current\n")

    def test_ignores_heading_like_text_inside_fences(self) -> None:
        text = """# Changelog

```markdown
## [v1.2.0] — 2026-08-14
```

## [v1.2.0] — 2026-08-14

- real
"""
        self.assertEqual(extractor.extract_notes(text, "v1.2.0"), "- real\n")

    def test_rejects_missing_duplicate_malformed_invalid_or_empty_sections(self) -> None:
        cases = {
            "missing": "# Changelog\n",
            "duplicate": "## [v1.2.0] — 2026-08-14\n\n- one\n\n## [v1.2.0] — 2026-08-15\n\n- two\n",
            "malformed": "## [v1.2.0] - 2026-08-14\n\n- body\n",
            "invalid-date": "## [v1.2.0] — 2026-02-30\n\n- body\n",
            "empty": "## [v1.2.0] — 2026-08-14\n\n## [v1.1.0] — 2026-08-01\n\n- old\n",
        }
        for name, text in cases.items():
            with self.subTest(name=name):
                with self.assertRaises(extractor.ExtractionError):
                    extractor.extract_notes(text, "v1.2.0")

    def test_failed_write_preserves_existing_output(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            changelog = root / "CHANGELOG.md"
            output = root / "notes.md"
            changelog.write_text("# Changelog\n", encoding="utf-8")
            output.write_text("keep\n", encoding="utf-8")

            with self.assertRaises(extractor.ExtractionError):
                extractor.write_notes(changelog, "v1.2.0", output)

            self.assertEqual(output.read_text(encoding="utf-8"), "keep\n")


if __name__ == "__main__":
    unittest.main()
