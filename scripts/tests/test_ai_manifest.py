"""scripts/ai-manifest.py: манифест собирается из отчётов и не записывается при расхождении."""
from pathlib import Path
import argparse
import importlib.util
import json
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'ai-manifest.py'
spec = importlib.util.spec_from_file_location('ai_manifest', SCRIPT)
manifest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(manifest)

PROMPT, SCHEMA = 'analyze-conversation.prompt.v9', 'analyze-conversation.v2'
CHECKSUM = 'f' * 64
GENERATION = {'temperature': 0.2, 'topP': 0.8, 'topK': 20, 'minP': 0, 'presencePenalty': 0, 'seed': 42, 'thinking': False, 'contextSize': 4096, 'parallelism': 1}
BASE = {
    'name': 'lidradar-main-v1', 'status': 'FROZEN', 'datasetVersion': 'lidradar-ai-benchmark.v1', 'dataset': {'cases': 500}, 'runtime': 'llama.cpp',
    'targetHardware': 'NVIDIA GeForce RTX 4060 8 GB', 'hardware': {'gpu': 'RTX 4060'}, 'generation': GENERATION,
    'qualityGate': {'minimumPrecision': 0.9, 'minimumRecall': 0.9}, 'performanceGate': {'maximumP95Ms': 8000, 'maximumVRAMMiB': 7500}, 'modelArtifact': 'Qwen3-8B-Q4_K_M.gguf', 'modelSHA256': 'd9' * 32,
}


def run_report(cases, **overrides):
    report = {
        'promptVersions': [PROMPT], 'schemaVersions': [SCHEMA], 'datasetSha256': CHECKSUM, 'cases': cases, 'precision': 0.96, 'recall': 0.97, 'f1': 0.965,
        'exactRate': 0.94, 'validRate': 1, 'evidenceExactRate': 0.99, 'p95Ms': 4300, 'passed': True,
        'byFactType': {'BOOKING_INTENT': {'recall': 0.95}, 'BUSINESS_COMMITMENT': {'recall': 1.0}},
        'performance': {'maxPromptTokens': 2686, 'minTokensPerSecond': 45.2},
        'server': {'buildInfo': 'b10666-4e97ac86e', 'modelPath': '/models/Qwen3-8B-Q4_K_M.gguf', 'modelFtype': 'Q4_K - Medium', 'modelSizeBytes': 5021827072, 'modelParameters': 8190735360,
                   'contextSize': 4096, 'totalSlots': 1,
                   'observedSampling': {'seed': 42, 'temperature': 0.2, 'topP': 0.8, 'topK': 20, 'minP': 0, 'presencePenalty': 0, 'thinkingDisabled': True}},
    }
    report.update(overrides)
    return report


class ManifestTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dir = Path(self.temp.name)
        self.write('base.json', BASE)
        self.write('golden.sha256', None, text=CHECKSUM + '  golden_v1.jsonl\n')
        self.write('dev.json', run_report(100, datasetSha256='d' * 64))
        self.write('golden.json', run_report(400))
        for name, letter in (('intents', 'i'), ('agreements', 'a'), ('independent', 'a')):
            self.write(f'{name}.json', run_report(27, datasetSha256=letter * 64))
        self.write('previous.json', {'name': 'previous manifest'})
        self.write('model-hash.json', {'sha256': BASE['modelSHA256'], 'sizeBytes': 5027783488, 'checkedAt': '2026-10-10T17:00:00Z'})
        self.write('hardware.json', {'passed': True, 'failures': [], 'peakObservedVRAMMiB': 5360, 'oom': 0, 'restartsDuringRun': 0,
                                      'minimumObservedTokensPerSecond': 45.2, 'maximumPromptTokens': 3048, 'maximumTotalTokens': 3300,
                                      'driverVersions': ['595.91.07'], 'imageIds': ['sha256:aaaa']})
        self.write('selection.json', {
            'promptVersion': PROMPT, 'schemaVersion': SCHEMA, 'treeSha256': 'a' * 64,
            'datasets': {f'models/datasets/{name}.jsonl': {'sha256': sha * 64, 'cases': 10} for name, sha in (('dev_v1', 'd'), ('intent_regression_v2', 'i'), ('agreements_dev_v2', 'a'), ('agreements_independent_v2', 'a'))},
            'devReports': {str(self.dir / 'dev.json'): {'passed': True}},
        })

    def write(self, name, value, text=None):
        (self.dir / name).write_text(text if text is not None else json.dumps(value))

    def arguments(self, **overrides):
        values = dict(base=str(self.dir / 'base.json'), previous_report=str(self.dir / 'previous.json'), selection=str(self.dir / 'selection.json'),
                      dev=str(self.dir / 'dev.json'), golden=str(self.dir / 'golden.json'), golden_checksum=str(self.dir / 'golden.sha256'),
                      v2_dev=[f'{n}={self.dir / (n + ".json")}' for n in ('intents', 'agreements', 'independent')], hardware=str(self.dir / 'hardware.json'),
                      minimum_fact_recall=0.85, model_hash_report=str(self.dir / 'model-hash.json'), context_size=None, limitation=['известное ограничение'])
        values.update(overrides)
        return argparse.Namespace(**values)

    def test_a_complete_qualification_freezes_the_manifest_with_numbers_taken_from_the_reports(self):
        result = manifest.build(self.arguments())
        self.assertEqual((result['status'], result['promptVersion'], result['contractVersion']), ('FROZEN', PROMPT, SCHEMA))
        self.assertIn('frozenAt', result)
        self.assertNotIn('pending', result)
        self.assertEqual(result['qualityGate']['minimumFactRecall'], 0.85)
        self.assertEqual(result['goldenResult']['exactRate'], 0.94)
        self.assertEqual(result['goldenResult']['recallByFactType']['BUSINESS_COMMITMENT'], 1.0)
        self.assertEqual((result['runtimeBuild'], result['hardwareResult']['peakObservedVRAMMiB'], result['selectionTreeSHA256']), ('b10666-4e97ac86e', 5360, 'a' * 64))
        self.assertEqual(set(result['v2ValidationResults']), {'intents', 'agreements', 'independent'})
        self.assertEqual(result['knownLimitations'], ['известное ограничение'])
        self.assertEqual((result['driverVersion'], result['runtimeImageId']), ('595.91.07', 'sha256:aaaa'))
        self.assertEqual(result['previousQualification'], str(self.dir / 'previous.json'))

    def test_a_larger_node_context_is_an_explicit_decision_checked_against_the_server(self):
        bigger = run_report(400, server={**run_report(400)['server'], 'contextSize': 8192})
        self.write('golden.json', bigger)
        with self.assertRaises(SystemExit) as raised:  # без явного решения расхождение с прежним манифестом блокирует запись
            manifest.build(self.arguments())
        self.assertIn('контекст сервера 8192, в манифесте 4096', str(raised.exception))
        result = manifest.build(self.arguments(context_size=8192))
        self.assertEqual(result['generation']['contextSize'], 8192)
        self.assertIn('контекст 8192', result['hardware']['fit'])
        self.assertIn('наблюдаемый пик 5360 MiB', result['hardware']['fit'])
        without_hardware = manifest.build(self.arguments(context_size=8192, hardware=None))
        self.assertIn('для контекста 8192 видеопамять не измерена', without_hardware['hardware']['fit'])
        self.assertEqual(without_hardware['status'], 'CANDIDATE')

    def test_without_hardware_evidence_the_model_stays_a_candidate(self):
        result = manifest.build(self.arguments(hardware=None, model_hash_report=None))
        self.assertEqual(result['status'], 'CANDIDATE')
        self.assertNotIn('frozenAt', result)
        self.assertEqual(len(result['pending']), 2)
        self.assertTrue(any('видеопамяти' in item for item in result['pending']))
        self.assertTrue(any('SHA-256' in item for item in result['pending']))
        self.assertIsNone(result['hardwareResult'])

    def mismatch(self, expected, **changes):
        arguments = self.arguments(**changes.pop('arguments', {}))
        originals = {name: (self.dir / f'{name}.json').read_text() for name in changes}
        try:
            for name, value in changes.items():
                self.write(f'{name}.json', value)
            with self.assertRaises(SystemExit) as raised:
                manifest.build(arguments)
            self.assertIn(expected, str(raised.exception))
        finally:
            for name, text in originals.items():
                (self.dir / f'{name}.json').write_text(text)  # следующий случай начинается с исправного набора

    def test_every_inconsistency_blocks_the_manifest(self):
        self.mismatch('версии инструкции', golden=run_report(400, promptVersions=['analyze-conversation.prompt.v6']))
        self.mismatch('версии контракта', golden=run_report(400, schemaVersions=['analyze-conversation.v1']))
        self.mismatch('GOLDEN: пороги не пройдены', golden=run_report(400, passed=False))
        self.mismatch('DEV: пороги не пройдены', dev=run_report(100, passed=False, datasetSha256='d' * 64))
        self.mismatch('объём выборок', golden=run_report(399))
        self.mismatch('сумма GOLDEN', golden=run_report(400, datasetSha256='0' * 64))
        self.mismatch('нет привязки к серверу', golden=run_report(400, server=None))
        self.mismatch('сервер не вернул параметры', golden=run_report(400, server={'contextSize': 4096, 'totalSlots': 1}))
        self.mismatch('параметр temperature на сервере 0.8', golden=run_report(400, server={'observedSampling': {'seed': 42, 'temperature': 0.8, 'topP': 0.8, 'topK': 20, 'minP': 0, 'presencePenalty': 0, 'thinkingDisabled': True}, 'contextSize': 4096, 'totalSlots': 1}))
        self.mismatch('режим рассуждений', golden=run_report(400, server={'observedSampling': {'seed': 42, 'temperature': 0.2, 'topP': 0.8, 'topK': 20, 'minP': 0, 'presencePenalty': 0, 'thinkingDisabled': False}, 'contextSize': 4096, 'totalSlots': 1}))
        self.mismatch('контекст сервера 8192', golden=run_report(400, server={**run_report(400)['server'], 'contextSize': 8192}))
        self.mismatch('DEV v2 intents: пороги не пройдены', intents=run_report(27, passed=False, datasetSha256='i' * 64))
        self.mismatch('отчёт об оборудовании не пройден: пик', hardware={'passed': False, 'failures': ['пик видеопамяти 7700 МиБ выше 7500'], 'peakObservedVRAMMiB': 7700, 'oom': 0, 'restartsDuringRun': 0,
                                                                         'minimumObservedTokensPerSecond': 45, 'maximumPromptTokens': 1, 'maximumTotalTokens': 1})

    def test_numbers_below_the_manifest_gates_are_refused_even_when_the_report_says_passed(self):
        # runner выставляет passed по порогам, которые передал запускавший: ослабленные флаги не должны давать зелёный манифест
        weak = {'minimumPrecision': 0.9, 'minimumRecall': 0.9, 'minimumF1': 0.9, 'minimumExactRate': 0.85, 'minimumValidRate': 0.99, 'minimumEvidenceExactRate': 0.9, 'minimumFactPrecision': 0.85}
        self.write('base.json', {**BASE, 'qualityGate': weak})
        self.assertEqual(manifest.build(self.arguments())['status'], 'FROZEN')
        self.mismatch('GOLDEN: precision 0.5 ниже порога minimumPrecision 0.9', golden=run_report(400, precision=0.5))
        self.mismatch('GOLDEN: recall 0.8 ниже порога minimumRecall 0.9', golden=run_report(400, recall=0.8))
        self.mismatch('GOLDEN: f1 0.8 ниже порога minimumF1 0.9', golden=run_report(400, f1=0.8))
        self.mismatch('GOLDEN: exactRate 0.7 ниже порога minimumExactRate 0.85', golden=run_report(400, exactRate=0.7))
        self.mismatch('GOLDEN: validRate 0.97 ниже порога minimumValidRate 0.99', golden=run_report(400, validRate=0.97))
        self.mismatch('GOLDEN: evidenceExactRate 0.8 ниже порога minimumEvidenceExactRate 0.9', golden=run_report(400, evidenceExactRate=0.8))
        self.mismatch('DEV: agreementExactRate 0.8 ниже порога minimumExactRate 0.85', dev=run_report(100, datasetSha256='d' * 64, agreementCases=20, agreementExactRate=0.8))
        self.mismatch('DEV v2 agreements: exactRate 0.6 ниже порога', agreements=run_report(21, datasetSha256='a' * 64, exactRate=0.6))

    def test_each_fact_type_must_reach_its_own_precision_and_recall(self):
        self.write('base.json', {**BASE, 'qualityGate': {**BASE['qualityGate'], 'minimumFactPrecision': 0.85}})
        good = {'BUSINESS_COMMITMENT': {'truePositive': 15, 'falsePositive': 0, 'falseNegative': 0, 'precision': 1.0, 'recall': 1.0}}
        self.assertEqual(manifest.build(self.arguments())['status'], 'FROZEN')
        self.write('golden.json', run_report(400, byFactType=good))
        self.assertEqual(manifest.build(self.arguments())['status'], 'FROZEN')
        poor_recall = {**good, 'BUSINESS_COMMITMENT': {'truePositive': 11, 'falsePositive': 0, 'falseNegative': 4, 'precision': 1.0, 'recall': 0.7333}}
        self.mismatch('GOLDEN: полнота BUSINESS_COMMITMENT 0.7333 ниже порога minimumFactRecall 0.85', golden=run_report(400, byFactType=poor_recall))
        poor_precision = {**good, 'PRICE_MENTIONED': {'truePositive': 10, 'falsePositive': 5, 'falseNegative': 0, 'precision': 0.6667, 'recall': 1.0}}
        self.mismatch('GOLDEN: точность PRICE_MENTIONED 0.6667 ниже порога minimumFactPrecision 0.85', golden=run_report(400, byFactType=poor_precision))
        absent = {'FOLLOW_UP_CANDIDATE': {'truePositive': 0, 'falsePositive': 0, 'falseNegative': 0, 'precision': 1.0, 'recall': 0.0}}
        self.write('golden.json', run_report(400, byFactType=absent))
        self.assertEqual(manifest.build(self.arguments())['status'], 'FROZEN')  # тип без положительных примеров полнотой не оценивается

    def test_the_p95_latency_is_checked_against_the_performance_gate(self):
        self.mismatch('GOLDEN: p95 9100 мс выше порога 8000 мс', golden=run_report(400, p95Ms=9100))
        self.mismatch('DEV: p95 8500 мс выше порога 8000 мс', dev=run_report(100, datasetSha256='d' * 64, p95Ms=8500))
        self.mismatch('DEV v2 independent: p95 8001 мс выше порога 8000 мс', independent=run_report(27, datasetSha256='a' * 64, p95Ms=8001))

    def test_a_report_taken_on_a_converted_copy_of_a_dataset_is_refused(self):
        self.mismatch('DEV: отчёт снят не на models/datasets/dev_v1.jsonl', dev=run_report(100, datasetSha256='0' * 64))
        self.mismatch('DEV v2 agreements: отчёт снят не на models/datasets/agreements_dev_v2.jsonl', agreements=run_report(21, datasetSha256='0' * 64))

    def test_the_weights_hash_computed_on_the_node_must_match_the_qualified_file(self):
        result = manifest.build(self.arguments())
        self.assertEqual(result['modelVerification']['sha256'], BASE['modelSHA256'])
        self.write('model-hash.json', {'sha256': '0' * 64, 'sizeBytes': 1})
        with self.assertRaises(SystemExit) as raised:
            manifest.build(self.arguments())
        self.assertIn('SHA-256 файла весов на узле', str(raised.exception))

    def test_the_previous_manifest_must_be_archived_first(self):
        with self.assertRaises(SystemExit) as raised:
            manifest.build(self.arguments(previous_report=str(self.dir / 'missing.json')))
        self.assertIn('прежний манифест не сохранён', str(raised.exception))

    def test_the_dev_report_must_be_the_one_chosen_before_golden(self):
        self.write('selection.json', {**json.loads((self.dir / 'selection.json').read_text()), 'devReports': {}})
        with self.assertRaises(SystemExit) as raised:
            manifest.build(self.arguments())
        self.assertIn('не входит в выбор', str(raised.exception))

    def test_the_command_writes_the_manifest_only_when_everything_agrees(self):
        out = self.dir / 'manifest.json'
        argv = ['--base', str(self.dir / 'base.json'), '--previous-report', str(self.dir / 'previous.json'), '--selection', str(self.dir / 'selection.json'), '--dev', str(self.dir / 'dev.json'),
                '--golden', str(self.dir / 'golden.json'), '--golden-checksum', str(self.dir / 'golden.sha256'), '--out', str(out), '--model-hash-report', str(self.dir / 'model-hash.json')]
        self.assertEqual(manifest.main(argv), 0)
        self.assertEqual(json.loads(out.read_text())['status'], 'CANDIDATE')  # без отчёта об оборудовании
        out.unlink()
        self.write('golden.json', run_report(400, passed=False))
        with self.assertRaises(SystemExit):
            manifest.main(argv)
        self.assertFalse(out.exists())


if __name__ == '__main__':
    unittest.main()
