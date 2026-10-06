"""Regression tests for documentation tooling, not product tests."""
import importlib.util
from pathlib import Path
import tempfile
import unittest
import zipfile
import io

SPEC = importlib.util.spec_from_file_location("docs_builder", Path(__file__).with_name("build.py"))
BUILD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BUILD)


class DocumentationToolingTests(unittest.TestCase):
    def test_valid_unicode_anchor_and_encoded_filename(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            page = root / "entry.md"
            target = root / "другая страница.md"
            page.write_text("# Entry\n[перейти](другая%20страница.md#данные)\n", encoding="utf-8")
            target.write_text("# Данные\n", encoding="utf-8")
            errors, stats = BUILD.validate_markdown([page, target], root)
            self.assertEqual(errors, [])
            self.assertEqual(stats["anchorsChecked"], 1)

    def test_missing_target_and_anchor_fail(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            page = root / "entry.md"
            page.write_text("# Entry\n[missing](absent.md)\n[anchor](#wrong)\n")
            errors, _ = BUILD.validate_markdown([page], root)
            self.assertEqual(len(errors), 2)

    def test_code_examples_are_not_links_or_headings(self):
        text = "# Real\n```md\n# Fake\n[example](missing.md)\n```\n"
        self.assertEqual(BUILD.anchors(text), {"real"})
        with tempfile.TemporaryDirectory() as temp:
            page = Path(temp) / "entry.md"
            page.write_text(text)
            errors, _ = BUILD.validate_markdown([page], Path(temp))
            self.assertEqual(errors, [])

    def test_unclosed_fence_fails(self):
        with tempfile.TemporaryDirectory() as temp:
            page = Path(temp) / "entry.md"
            page.write_text("# Entry\n```text\nunterminated\n")
            errors, _ = BUILD.validate_markdown([page], Path(temp))
            self.assertTrue(any("unclosed" in e for e in errors))

    def test_explicit_anchor_and_duplicate_heading(self):
        self.assertEqual(
            BUILD.anchors('# Name\n## Name\n<a id="stable"></a>\n'),
            {"name", "name-1", "stable"},
        )

    def test_cross_root_and_excluded_package_target_fail(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            page = root / "entry.md"
            extra = root / "extra.md"
            extra.write_text("# Extra\n")
            page.write_text("# Entry\n[outside](../outside.md)\n[extra](extra.md)\n")
            errors, _ = BUILD.validate_markdown([page, extra], root, {page.resolve()})
            self.assertEqual(len(errors), 2)

    def test_generated_links_are_rebased_and_source_is_visible(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            canonical = root / "backend"
            views = root / "notion"
            canonical.mkdir()
            views.mkdir()
            page = canonical / "page.md"
            page.write_text('# Page\n[peer](peer.md#x)\n[web](https://example.test)\n')
            result = BUILD.derived_text(page, views / "view.md")
            self.assertIn("../backend/peer.md#x", result)
            self.assertIn("https://example.test", result)
            self.assertIn("GENERATED", result)
            self.assertIn("source-sha256:", result)

    def test_zip_is_deterministic_and_preserves_payload(self):
        a = BUILD.zip_bytes({"b.txt": b"B", "a.txt": b"A"})
        b = BUILD.zip_bytes({"a.txt": b"A", "b.txt": b"B"})
        self.assertEqual(a, b)
        with zipfile.ZipFile(io.BytesIO(a)) as z:
            self.assertEqual(z.read("a.txt"), b"A")
            self.assertIsNone(z.testzip())

    def test_check_mode_refuses_stale_file_without_mutating(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            page = root / "output.md"
            page.write_bytes(b"old")
            original = BUILD.REPO
            BUILD.REPO = root
            try:
                with self.assertRaises(ValueError):
                    BUILD.emit(page, b"new", True)
                self.assertEqual(page.read_bytes(), b"old")
            finally:
                BUILD.REPO = original

    def test_partitions_cover_every_source_once(self):
        entries = {f"doc-{n:03}.md": f"# {n}".encode() for n in range(103)}
        batches = BUILD.partition(entries, {"meta.json": b"{}"})
        self.assertEqual(len(batches), 2)
        self.assertEqual(sum(map(len, batches)), len(entries))
        self.assertEqual({k: v for batch in batches for k, v in batch.items()}, entries)
        self.assertTrue(all(len(batch) <= BUILD.BATCH_FILES for batch in batches))


if __name__ == "__main__":
    unittest.main()
