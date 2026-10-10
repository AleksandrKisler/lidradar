"""scripts/ai-selection.py: выбор AI tuple фиксируется до GOLDEN и проверяется после прогона."""
from pathlib import Path
import argparse
import importlib.util
import json
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'ai-selection.py'
spec = importlib.util.spec_from_file_location('ai_selection', SCRIPT)
selection = importlib.util.module_from_spec(spec)
spec.loader.exec_module(selection)


def make_tree(root):
    for directory in selection.SOURCE_DIRECTORIES:
        (root / directory).mkdir(parents=True)
        (root / directory / 'file.go').write_text(f'package x // {directory}\n')
        (root / directory / 'file_test.go').write_text('package x // test\n')
    for name in selection.CONTRACTS + selection.DATASETS:
        path = root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text('{"id":"a"}\n{"id":"b"}\n' if name.endswith('.jsonl') else f'content of {name}\n')
    report = root / 'models/reports/dev.json'
    report.parent.mkdir(parents=True, exist_ok=True)
    report.write_text(json.dumps({'cases': 100, 'passed': True}))


def arguments(*reports, note=None):
    return argparse.Namespace(prompt_version='analyze-conversation.prompt.v9', schema_version='analyze-conversation.v2', dev_report=list(reports), note=note)


class AiSelectionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        make_tree(self.root)
        self.chosen = selection.build_selection(arguments('models/reports/dev.json'), self.root)

    def problems(self):
        return selection.verify_selection(self.chosen, self.root)

    def test_an_untouched_tree_matches_and_test_files_are_not_part_of_the_tuple(self):
        self.assertEqual(self.problems(), [])
        names = set(self.chosen['sha256'])
        self.assertFalse([n for n in names if n.endswith('_test.go')])
        self.assertEqual(len([n for n in names if n.endswith('.go')]), len(selection.SOURCE_DIRECTORIES))
        # Правка теста не ломает выбор: он не влияет на ответ модели.
        (self.root / 'backend/internal/ai/application/file_test.go').write_text('package x // changed\n')
        self.assertEqual(self.problems(), [])

    def test_the_selection_records_what_was_decided_on(self):
        self.assertEqual(self.chosen['promptVersion'], 'analyze-conversation.prompt.v9')
        self.assertEqual(self.chosen['schemaVersion'], 'analyze-conversation.v2')
        self.assertEqual(self.chosen['note'], selection.MARKER)
        self.assertEqual(self.chosen['datasets']['models/datasets/golden_v1.jsonl']['cases'], 2)
        self.assertEqual(self.chosen['devReports']['models/reports/dev.json'], {'sha256': selection.sha256(self.root / 'models/reports/dev.json'), 'cases': 100, 'passed': True})
        self.assertEqual(len(self.chosen['treeSha256']), 64)

    def test_every_kind_of_change_after_the_selection_is_reported_by_name(self):
        cases = {
            'инструкция': ('backend/internal/ai/infrastructure/file.go', lambda p: p.write_text('package x // edited\n'), 'файл изменён после выбора: backend/internal/ai/infrastructure/file.go'),
            'валидатор удалён': ('backend/internal/ai/application/file.go', lambda p: p.unlink(), 'файл удалён: backend/internal/ai/application/file.go'),
            'новый файл': ('backend/internal/ai/domain/extra.go', lambda p: p.write_text('package x\n'), 'файл не входил в выбор: backend/internal/ai/domain/extra.go'),
            'контракт': ('contracts/ai/analyze_conversation_v2.schema.json', lambda p: p.write_text('{}'), 'файл изменён после выбора: contracts/ai/analyze_conversation_v2.schema.json'),
            'GOLDEN': ('models/datasets/golden_v1.jsonl', lambda p: p.write_text('{"id":"c"}\n'), 'набор данных изменён после выбора: models/datasets/golden_v1.jsonl'),
            'отчёт DEV': ('models/reports/dev.json', lambda p: p.write_text('{}'), 'отчёт DEV изменён после выбора: models/reports/dev.json'),
        }
        for name, (relative, change, expected) in cases.items():
            with self.subTest(name):
                path = self.root / relative
                original = path.read_bytes() if path.exists() else None
                change(path)
                self.assertIn(expected, self.problems())
                if original is None:
                    path.unlink()
                else:
                    path.write_bytes(original)
                self.assertEqual(self.problems(), [])

    def test_a_failed_dev_report_cannot_justify_opening_golden(self):
        (self.root / 'models/reports/dev.json').write_text(json.dumps({'cases': 100, 'passed': False}))
        with self.assertRaises(SystemExit) as raised:
            selection.build_selection(arguments('models/reports/dev.json'), self.root)
        self.assertIn('не пройден', str(raised.exception))

    def test_the_tree_digest_depends_on_names_not_only_contents(self):
        digest = selection.tree_digest({'a.go': 'x' * 64, 'b.go': 'y' * 64})
        self.assertNotEqual(digest, selection.tree_digest({'a.go': 'y' * 64, 'b.go': 'x' * 64}))
        self.assertEqual(digest, selection.tree_digest({'b.go': 'y' * 64, 'a.go': 'x' * 64}))

    def test_a_tampered_digest_is_caught_even_when_every_file_matches(self):
        self.chosen['treeSha256'] = '0' * 64
        self.assertEqual(self.problems(), ['сумма набора файлов не сходится с перечнем в выборе'])

    def test_the_real_repository_lists_exist(self):
        root = SCRIPT.parents[1]
        for name in selection.tuple_files(root) + selection.DATASETS:
            self.assertTrue((root / name).exists(), name)
        self.assertGreaterEqual(len(selection.tuple_files(root)), 20)


if __name__ == '__main__':
    unittest.main()
