#!/usr/bin/env python3
"""Documentation checks include root/nested guides, images, and new files."""
import importlib.util
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("docs_check", Path(__file__).with_name("docs-check.py"))
docs_check = importlib.util.module_from_spec(spec)
spec.loader.exec_module(docs_check)


class RouteInventoryTests(unittest.TestCase):
    def test_methods_wildcards_and_constants_not_test_urls(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "internal/server"
            source.mkdir(parents=True)
            (source / "server.go").write_text('''package server
const catalogPath = "/api/v1/catalog"
func register() {
    mux.HandleFunc("GET /api/v1/messages/{id}", read)
    mux.HandleFunc("POST "+catalogPath, refresh)
    mux.HandleFunc("/api/v1/rooms/{room}/surface/{path...}", surface)
    mux.Handle("/", files)
    request("/api/v1/export?token=never-a-route")
}
''', encoding="utf-8")
            (source / "server_test.go").write_text('''package server
func test() {
    mux.HandleFunc("POST /api/v1/test-only", fake)
    request("/api/v1/attachments/../../etc/passwd")
}
''', encoding="utf-8")
            self.assertEqual(docs_check.extract_routes(root), [
                "/api/v1/rooms/{room}/surface/{path...}",
                "GET /api/v1/messages/{id}", "POST /api/v1/catalog",
            ])

    def test_unknown_registration_expression_fails_visibly(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "internal/service"
            source.mkdir(parents=True)
            (source / "service.go").write_text('mux.HandleFunc(dynamicRoute, handler)', encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "unsupported route expression"):
                docs_check.extract_routes(root)


class FlagInventoryTests(unittest.TestCase):
    def test_bound_relay_flags_and_test_exclusion(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cli = root / "cmd/pairroom"
            cli.mkdir(parents=True)
            relay = root / "internal/relayclient"
            relay.mkdir(parents=True)
            (cli / "main.go").write_text('flags.Bool("mock", false, "")')
            (cli / "main_test.go").write_text('flags.Bool("test-only", false, "")')
            (relay / "cli.go").write_text('flags.StringVar(&o.room, "room", "", "")\nflags.Var(&paths, "attach", "")')
            with patch.object(docs_check, "ROOT", root):
                self.assertEqual(docs_check.extract_flags(), ["attach", "mock", "room"])


class MarkdownTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def write(self, name, text=""):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
        return path

    def test_git_inventory_covers_root_nested_and_new_docs_not_ignored_outputs(self):
        subprocess.run(["git", "init", "-q", str(self.root)], check=True)
        self.write(".gitignore", ".worktrees/\n.browser-results/\n")
        self.write("SECURITY.md")
        self.write("deleted.md")
        subprocess.run(["git", "-C", str(self.root), "add", "."], check=True)
        (self.root / "deleted.md").unlink()
        self.write("desktop/README.md")
        self.write("docs/validation/README.md")
        self.write(".agents/skills/README.md")
        self.write(".worktrees/other/README.md")
        self.write(".browser-results/README.md")
        self.assertEqual([p.relative_to(self.root).as_posix() for p in docs_check.markdown_files(self.root)], [
            ".agents/skills/README.md", "SECURITY.md", "desktop/README.md", "docs/validation/README.md",
        ])

    def test_source_archive_discovery_excludes_build_outputs(self):
        self.write("README.md")
        self.write("desktop/README.md")
        self.write("node_modules/package/README.md")
        self.write("desktop/bin/README.md")
        self.write(".worktrees/other/README.md")
        self.assertEqual([p.relative_to(self.root).as_posix() for p in docs_check.markdown_files(self.root)], [
            "README.md", "desktop/README.md",
        ])

    def test_detects_deleted_guides_in_security_and_support(self):
        for filename in ("SECURITY.md", "SUPPORT.md", "desktop/README.md"):
            path = self.write(filename, "[obsolete](missing.md)")
            self.assertEqual(len(docs_check.check_links(path, self.root)), 1)

    def test_checks_markdown_and_html_images_and_reference_destinations(self):
        path = self.write("README.md", '''![shot](missing.png)
<img alt="shot" src="other.png">
[guide][reference]
[reference]: missing.md "Guide"
<a href="removed.md">Guide</a>
''')
        failures = docs_check.check_links(path, self.root)
        self.assertEqual(len(failures), 4, failures)

    def test_encoded_path_anchor_external_and_code_label(self):
        self.write("docs/first run.md", "# Start\n")
        self.write("docs/image.png")
        path = self.write("README.md", '''[`Guide`](docs/first%20run.md#start)
[Guide](<docs/first run.md>)
![shot](docs/image.png "Screenshot")
[web](https://example.com/missing) [mail](mailto:help@example.com)
[anchor](#local) [remote](//example.com/image.png)
''')
        self.assertEqual(docs_check.check_links(path, self.root), [])

    def test_fenced_and_inline_examples_and_comments_are_not_links(self):
        path = self.write("README.md", '''```md
[example](missing.md)
```
~~~text
<img src="missing.png">
~~~
<!-- [removed](missing.md) -->
Use `[example](missing.md)` to illustrate Markdown.
''')
        self.assertEqual(docs_check.check_links(path, self.root), [])

    def test_long_fence_is_not_closed_by_short_nested_fence(self):
        text = "````md\n```md\n[example](missing.md)\n```\n````\n[real](real.md)\n"
        self.assertEqual(docs_check.link_targets(text), ["real.md"])

    def test_encoded_escape_is_rejected(self):
        path = self.write("README.md", "[escape](%2e%2e/outside.md)")
        self.assertIn("escapes repository", docs_check.check_links(path, self.root)[0])

    def test_symlink_escape_is_rejected(self):
        with tempfile.TemporaryDirectory() as outside:
            (Path(outside) / "file.md").write_text("outside", encoding="utf-8")
            try:
                (self.root / "escape").symlink_to(outside, target_is_directory=True)
            except OSError:
                self.skipTest("symlink creation unavailable")
            path = self.write("README.md", "[escape](escape/file.md)")
            self.assertIn("escapes repository", docs_check.check_links(path, self.root)[0])

    def test_document_symlink_escape_is_rejected(self):
        with tempfile.TemporaryDirectory() as outside:
            target = Path(outside) / "file.md"
            target.write_text("outside", encoding="utf-8")
            path = self.root / "README.md"
            try:
                path.symlink_to(target)
            except OSError:
                self.skipTest("symlink creation unavailable")
            self.assertIn("document symlink escapes", docs_check.check_links(path, self.root)[0])

    def test_internal_document_symlink_remains_supported(self):
        target = self.write("AGENTS.md", "[readme](README.md)")
        self.write("README.md")
        try:
            (self.root / "CLAUDE.md").symlink_to(target.name)
        except OSError:
            self.skipTest("symlink creation unavailable")
        self.assertEqual(docs_check.check_links(self.root / "CLAUDE.md", self.root), [])


if __name__ == "__main__":
    unittest.main()
