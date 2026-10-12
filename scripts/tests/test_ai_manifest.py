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


def agreement_metrics(precision, recall, positives=80):
    true_positive = round(positives * recall)
    false_positive = round(true_positive * (1 - precision) / precision)
    return {'truePositive': true_positive, 'falsePositive': false_positive, 'falseNegative': positives - true_positive,
            'precision': precision, 'recall': recall, 'f1': 0.9}


def commitment_report(cases, letter, precision=0.97, recall=0.91, agreement_precision=None, agreement_recall=None, **overrides):
    positives = 80
    true_positive = round(positives * recall)
    false_positive = round(true_positive * (1 - precision) / precision)
    report = run_report(cases, datasetSha256=letter * 64, passed=False, precision=0.7, recall=0.9, exactRate=0.8,
                        byFactType={'BUSINESS_COMMITMENT': {'truePositive': true_positive, 'falsePositive': false_positive, 'falseNegative': positives - true_positive,
                                                           'precision': true_positive / (true_positive + false_positive), 'recall': recall},
                                    'BOOKING_INTENT': {'truePositive': 0, 'falsePositive': 9, 'falseNegative': 0, 'precision': 0.0, 'recall': 1.0}})
    report['commitmentAgreements'] = agreement_metrics(precision if agreement_precision is None else agreement_precision,
                                                       recall if agreement_recall is None else agreement_recall)
    build = overrides.pop('build', None)
    if build:
        report['server'] = {**report['server'], 'buildInfo': build}
    report.update(overrides)
    return report


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
        self.write('commitments.json', commitment_report(282, 'c'))
        self.write('holdout.json', commitment_report(160, 'h'))
        self.write('baseline.json', commitment_report(160, 'h', recall=0.62, promptVersions=[PROMPT], build='older'))
        self.write('model-hash.json', {'sha256': BASE['modelSHA256'], 'sizeBytes': 5027783488, 'checkedAt': '2026-10-10T17:00:00Z'})
        self.write('hardware.json', {'passed': True, 'failures': [], 'peakObservedVRAMMiB': 5360, 'oom': 0, 'restartsDuringRun': 0,
                                      'minimumObservedTokensPerSecond': 45.2, 'maximumPromptTokens': 3048, 'maximumTotalTokens': 3300,
                                      'driverVersions': ['595.91.07'], 'imageIds': ['sha256:aaaa']})
        self.write('selection.json', {
            'promptVersion': PROMPT, 'schemaVersion': SCHEMA, 'treeSha256': 'a' * 64,
            'datasets': {f'models/datasets/{name}.jsonl': {'sha256': sha * 64, 'cases': 10} for name, sha in (('dev_v1', 'd'), ('intent_regression_v2', 'i'), ('agreements_dev_v2', 'a'), ('agreements_independent_v2', 'a'), ('commitment_dev_v1', 'c'), ('commitment_holdout_v2', 'h'))},
            'devReports': {str(self.dir / 'dev.json'): {'passed': True}},
        })

    def write(self, name, value, text=None):
        (self.dir / name).write_text(text if text is not None else json.dumps(value))

    def arguments(self, **overrides):
        values = dict(base=str(self.dir / 'base.json'), previous_report=str(self.dir / 'previous.json'), selection=str(self.dir / 'selection.json'),
                      dev=str(self.dir / 'dev.json'), golden=str(self.dir / 'golden.json'), golden_checksum=str(self.dir / 'golden.sha256'),
                      v2_dev=[f'{n}={self.dir / (n + ".json")}' for n in ('intents', 'agreements', 'independent')], hardware=str(self.dir / 'hardware.json'),
                      minimum_fact_recall=0.85, model_hash_report=str(self.dir / 'model-hash.json'), context_size=None, limitation=['известное ограничение'],
                      commitment_dev=None, holdout=None, holdout_baseline=None, golden_regression=False)
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

    def commitment_overrides(self, **overrides):
        values = dict(commitment_dev=str(self.dir / 'commitments.json'), holdout=str(self.dir / 'holdout.json'),
                      holdout_baseline=str(self.dir / 'baseline.json'), golden_regression=True)
        values.update(overrides)
        return values

    def commitment_arguments(self, **overrides):
        return self.arguments(**self.commitment_overrides(**overrides))

    def test_the_commitment_sets_are_judged_by_their_own_fact_type_and_not_by_the_overall_gates(self):
        result = manifest.build(self.commitment_arguments())
        self.assertEqual(result['status'], 'FROZEN')
        self.assertEqual(result['qualityGate']['commitmentHoldout'], {'minimumPrecision': 0.95, 'minimumRecall': 0.80})
        self.assertEqual((result['holdoutResult']['report'], result['holdoutResult']['recall']), (str(self.dir / 'holdout.json'), 0.91))
        self.assertEqual(result['holdoutBaseline']['recall'], 0.62)  # базовая ревизия записана рядом, чтобы выигрыш был виден
        self.assertNotIn('revision', result['holdoutBaseline'])
        described = manifest.build(self.commitment_arguments(holdout_baseline_revision='3dd2807 с исполнителем слепой выборки'))
        self.assertEqual(described['holdoutBaseline']['revision'], '3dd2807 с исполнителем слепой выборки')
        self.assertEqual(result['commitmentValidation']['cases'], 282)
        self.assertEqual(result['dataset']['holdout'], {'sha256': 'h' * 64, 'cases': 10})
        self.assertIs(result['goldenResult']['blind'], False)  # регрессионный прогон не выдаётся за слепой
        self.assertIn('Регрессионный', result['goldenResult']['note'])
        plain = manifest.build(self.arguments())
        self.assertNotIn('holdoutResult', plain)
        self.assertNotIn('blind', plain['goldenResult'])

    def test_a_blind_result_below_the_preregistered_bars_blocks_the_manifest(self):
        self.write('base.json', {**BASE, 'qualityGate': {**BASE['qualityGate'], 'minimumValidRate': 0.99}})
        self.mismatch('Слепая выборка обязательств: точность BUSINESS_COMMITMENT', holdout=commitment_report(160, 'h', precision=0.90), arguments=self.commitment_overrides())
        self.mismatch('Слепая выборка обязательств: полнота BUSINESS_COMMITMENT', holdout=commitment_report(160, 'h', recall=0.79), arguments=self.commitment_overrides())
        self.mismatch('Набор обязательств (DEV): полнота BUSINESS_COMMITMENT', commitments=commitment_report(282, 'c', recall=0.70), arguments=self.commitment_overrides())
        self.mismatch('Слепая выборка обязательств: доля валидных ответов', holdout=commitment_report(160, 'h', validRate=0.95), arguments=self.commitment_overrides())
        self.mismatch('Слепая выборка обязательств: p95 9000 мс', holdout=commitment_report(160, 'h', p95Ms=9000), arguments=self.commitment_overrides())
        self.mismatch('в наборе нет обещаний', holdout=commitment_report(160, 'h', byFactType={}), arguments=self.commitment_overrides())

    def test_unmet_bars_leave_a_candidate_when_asked_but_inconsistent_data_still_blocks(self):
        self.write('base.json', {**BASE, 'qualityGate': {**BASE['qualityGate'], 'minimumValidRate': 0.99}})
        self.write('holdout.json', commitment_report(160, 'h', recall=0.60, validRate=0.989))
        self.write('golden.json', run_report(400, precision=0.80, passed=False))
        with self.assertRaises(SystemExit):  # без разрешения непройденные границы блокируют запись
            manifest.build(self.commitment_arguments())
        result = manifest.build(self.commitment_arguments(candidate_on_unmet=True))
        self.assertEqual(result['status'], 'CANDIDATE')
        self.assertNotIn('frozenAt', result)
        joined = '\n'.join(result['unmetBars'])
        for fragment in ('Слепая выборка обязательств: полнота BUSINESS_COMMITMENT', 'доля валидных ответов', 'GOLDEN: пороги не пройдены', 'GOLDEN: precision'):
            self.assertIn(fragment, joined)
        self.assertTrue(all(f'граница не пройдена: {item}' in result['pending'] for item in result['unmetBars']))
        self.assertEqual(result['holdoutResult']['recall'], 0.6)  # результат записан, а не скрыт
        # противоречие данных (чужая выборка) блокирует запись и в режиме кандидата
        self.write('holdout.json', commitment_report(160, '0', recall=0.60))
        with self.assertRaises(SystemExit) as raised:
            manifest.build(self.commitment_arguments(candidate_on_unmet=True))
        self.assertIn('отчёт снят не на models/datasets/commitment_holdout_v2.jsonl', str(raised.exception))
        # а когда всё пройдено, флаг ничего не меняет
        self.write('golden.json', run_report(400))
        self.write('holdout.json', commitment_report(160, 'h'))
        clean = manifest.build(self.commitment_arguments(candidate_on_unmet=True))
        self.assertEqual(clean['status'], 'FROZEN')
        self.assertNotIn('unmetBars', clean)

    def test_a_failed_hardware_report_is_an_unmet_bar_in_candidate_mode_and_a_blocker_otherwise(self):
        failed = {'passed': False, 'failures': ['ответов, оборванных по длине: 1'], 'peakObservedVRAMMiB': 5844, 'oom': 0, 'restartsDuringRun': 0,
                  'minimumObservedTokensPerSecond': 41.3, 'maximumPromptTokens': 6417, 'maximumTotalTokens': 8192, 'driverVersions': ['595.91.07'], 'imageIds': ['sha256:aaaa']}
        self.write('hardware.json', failed)
        with self.assertRaises(SystemExit) as raised:
            manifest.build(self.arguments())
        self.assertIn('отчёт об оборудовании не пройден', str(raised.exception))
        result = manifest.build(self.arguments(candidate_on_unmet=True))
        self.assertEqual(result['status'], 'CANDIDATE')
        self.assertTrue(any('ответов, оборванных по длине: 1' in item for item in result['unmetBars']))
        self.assertIs(result['hardwareResult']['passed'], False)  # результат записан, а не скрыт
        self.assertEqual(result['hardwareResult']['peakObservedVRAMMiB'], 5844)

    def test_the_blind_set_must_be_the_sealed_one_and_the_baseline_must_use_the_same_file(self):
        self.mismatch('Слепая выборка обязательств: отчёт снят не на models/datasets/commitment_holdout_v2.jsonl', holdout=commitment_report(160, '0'), arguments=self.commitment_overrides())
        self.mismatch('базовая ревизия снята не на той слепой выборке', baseline=commitment_report(160, 'z'), arguments=self.commitment_overrides())
        self.mismatch('Слепая выборка обязательств: версии инструкции', holdout=commitment_report(160, 'h', promptVersions=['analyze-conversation.prompt.v6']), arguments=self.commitment_overrides())
        self.mismatch('Слепая выборка обязательств: параметр temperature', holdout=commitment_report(160, 'h', server={**run_report(1)['server'], 'observedSampling': {**run_report(1)['server']['observedSampling'], 'temperature': 0.9}}), arguments=self.commitment_overrides())

    def test_a_change_must_not_lower_the_recall_the_previous_revision_had_on_the_same_blind_set(self):
        self.write('baseline.json', commitment_report(160, 'h', recall=0.97, build='older'))
        self.mismatch('полнота BUSINESS_COMMITMENT на слепой выборке упала', arguments=self.commitment_overrides())

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

    def test_the_commitment_agreement_is_judged_by_the_same_bars_as_the_fact(self):
        self.write('base.json', {**BASE, 'qualityGate': {**BASE['qualityGate'], 'minimumValidRate': 0.99}})
        self.mismatch('Слепая выборка обязательств: точность договорённости COMMITMENT', holdout=commitment_report(160, 'h', agreement_precision=0.90), arguments=self.commitment_overrides())
        self.mismatch('Слепая выборка обязательств: полнота договорённости COMMITMENT', holdout=commitment_report(160, 'h', agreement_recall=0.79), arguments=self.commitment_overrides())
        self.mismatch('Набор обязательств (DEV): полнота договорённости COMMITMENT', commitments=commitment_report(282, 'c', agreement_recall=0.7), arguments=self.commitment_overrides())
        result = manifest.build(self.commitment_arguments())
        self.assertEqual(result['holdoutResult']['commitmentAgreements']['recall'], 0.91)  # договорённость записана рядом с фактом
        self.assertEqual(result['commitmentValidation']['commitmentAgreements']['precision'], 0.97)

    def test_the_owner_bars_are_inclusive_and_eighty_percent_recall_is_enough(self):
        self.write('base.json', {**BASE, 'qualityGate': {**BASE['qualityGate'], 'minimumValidRate': 0.99}})
        self.write('holdout.json', commitment_report(160, 'h', precision=0.95, recall=0.80, agreement_precision=0.95, agreement_recall=0.80))
        self.write('baseline.json', commitment_report(160, 'h', recall=0.62, build='older'))
        result = manifest.build(self.commitment_arguments())
        self.assertEqual(result['status'], 'FROZEN')  # ровно на границе: пройдено, ниже — нет (проверено соседними тестами)
        self.write('holdout.json', commitment_report(160, 'h', recall=0.82, agreement_recall=0.82))
        self.assertEqual(manifest.build(self.commitment_arguments())['status'], 'FROZEN')  # прежние 0,85 здесь не требуются

    def test_a_report_without_the_agreement_metric_is_refused(self):
        stale = commitment_report(160, 'h')
        del stale['commitmentAgreements']
        self.mismatch('в отчёте нет метрики договорённостей COMMITMENT', holdout=stale, arguments=self.commitment_overrides())
        empty = commitment_report(160, 'h', commitmentAgreements={'truePositive': 0, 'falsePositive': 0, 'falseNegative': 0, 'precision': 1, 'recall': 1})
        self.mismatch('в наборе нет обещаний, полноту договорённостей оценить нельзя', holdout=empty, arguments=self.commitment_overrides())

    def test_the_agreement_recall_must_not_fall_below_the_previous_revision_either(self):
        self.write('baseline.json', commitment_report(160, 'h', recall=0.62, agreement_recall=0.95, build='older'))
        self.mismatch('полнота договорённости COMMITMENT на слепой выборке упала', arguments=self.commitment_overrides())
        # базовая линия, снятая без метрики договорённостей, сравнением по ней не блокирует
        old = commitment_report(160, 'h', recall=0.62, build='older')
        del old['commitmentAgreements']
        self.write('baseline.json', old)
        result = manifest.build(self.commitment_arguments())
        self.assertIsNone(result['holdoutBaseline']['commitmentAgreements'])

    def test_the_holdout_dataset_can_be_named_explicitly(self):
        self.write('selection.json', {**json.loads((self.dir / 'selection.json').read_text()), 'datasets': {
            **json.loads((self.dir / 'selection.json').read_text())['datasets'], 'models/datasets/commitment_holdout_v3.jsonl': {'sha256': 'x' * 64, 'cases': 10}}})
        self.write('holdout.json', commitment_report(160, 'x'))
        self.write('baseline.json', commitment_report(160, 'x', recall=0.62, build='older'))
        result = manifest.build(self.commitment_arguments(holdout_dataset='models/datasets/commitment_holdout_v3.jsonl'))
        self.assertEqual(result['dataset']['holdout']['sha256'], 'x' * 64)

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
