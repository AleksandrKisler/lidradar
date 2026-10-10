"""scripts/ai-context-probe.py: зонд следует за пределом продукта и воспроизводим."""
from pathlib import Path
import importlib.util
import json
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('ai_context_probe', ROOT / 'scripts' / 'ai-context-probe.py')
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


def runes_of(case):
    return len(case['input']['companyContext']) + sum(len(m['body']) for m in case['input']['messages'])


class ContextProbeTests(unittest.TestCase):
    def setUp(self):
        self.max_messages, self.max_runes = probe.limits()
        self.cases = [json.loads(line) for line in probe.render(self.max_messages, self.max_runes).splitlines()]

    def test_the_limits_come_from_the_product_code(self):
        self.assertEqual((self.max_messages, self.max_runes), (20, 12000))

    def test_no_conversation_exceeds_the_limits_and_the_largest_reaches_them(self):
        for case in self.cases:
            self.assertLessEqual(len(case['input']['messages']), self.max_messages, case['id'])
            self.assertLessEqual(runes_of(case), self.max_runes, case['id'])
        limit = {c['id']: c for c in self.cases}['limit-many-short']
        self.assertEqual(len(limit['input']['messages']), self.max_messages)
        self.assertGreaterEqual(runes_of(limit), self.max_runes - self.max_messages)  # округление длины сообщения

    def test_every_case_takes_the_heaviest_prompt_path_and_is_valid(self):
        ids = set()
        for case in self.cases:
            self.assertNotIn(case['id'], ids)
            ids.add(case['id'])
            messages = case['input']['messages']
            self.assertIn('записаться', messages[0]['body'])
            self.assertIn('Проверю', messages[1]['body'])
            self.assertEqual(case['input']['analysisThroughMessageId'], messages[-1]['id'])
            self.assertEqual((case['split'], case['expectedFacts']), ('DEV', []))
            self.assertEqual((case['input']['schemaVersion'], case['input']['promptVersion']), ('analyze-conversation.v2', 'analyze-conversation.prompt.v9'))

    def test_generation_is_deterministic_and_the_checked_in_file_is_current(self):
        self.assertEqual(probe.render(self.max_messages, self.max_runes), probe.render(self.max_messages, self.max_runes))
        self.assertEqual(probe.main(['--check']), 0)

    def test_a_stale_file_is_reported_and_a_changed_limit_changes_the_dataset(self):
        with tempfile.TemporaryDirectory() as directory:
            stale = Path(directory) / 'stale.jsonl'
            stale.write_text('{}\n')
            self.assertEqual(probe.main(['--out', str(stale), '--check']), 1)
            self.assertEqual(probe.main(['--out', str(stale)]), 0)
            self.assertEqual(probe.main(['--out', str(stale), '--check']), 0)
        self.assertNotEqual(probe.render(20, 12000), probe.render(20, 3000))


if __name__ == '__main__':
    unittest.main()
