#!/usr/bin/env python3
"""Production route inventories must not turn test attack strings into APIs."""
import importlib.util
import tempfile
import unittest
from pathlib import Path

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


if __name__ == "__main__":
    unittest.main()
