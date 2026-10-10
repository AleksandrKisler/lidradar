"""scripts/ai-hardware-report.py и scripts/ai-node-sample.sh: доказательство для порога оборудования."""
from pathlib import Path
import csv
import hashlib
import importlib.util
import json
import os
import signal
import subprocess
import tempfile
import time
import unittest

SCRIPTS = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('ai_hardware', SCRIPTS / 'ai-hardware-report.py')
hardware = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hardware)

START, FINISH = 1_791_639_000, 1_791_639_300


def stamp(epoch):
    return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(epoch))


def write_samples(path, first=START - 5, last=FINISH + 5, used=5360, oom='false', restarts=0, running='true', rows_override=None, driver='595.91.07', image='sha256:aaaa', legacy=False):
    with open(path, 'w', newline='') as handle:
        writer = csv.writer(handle)
        if legacy:  # формат до появления столбцов driverVersion и imageId
            writer.writerow(['epoch', 'gpu', 'memoryUsedMiB', 'memoryTotalMiB', 'restartCount', 'oomKilled', 'running'])
        else:
            writer.writerow(['epoch', 'gpu', 'driverVersion', 'memoryUsedMiB', 'memoryTotalMiB', 'restartCount', 'oomKilled', 'running', 'imageId'])
        for epoch in range(first, last + 1, 5):
            if rows_override:
                writer.writerow(rows_override(epoch))
            elif legacy:
                writer.writerow([epoch, 'NVIDIA GeForce RTX 4060', used - (epoch % 7), 8188, restarts, oom, running])
            else:
                writer.writerow([epoch, 'NVIDIA GeForce RTX 4060', driver, used - (epoch % 7), 8188, restarts, oom, running, image])


def benchmark_report(**overrides):
    performance = {
        'startedAt': stamp(START), 'finishedAt': stamp(FINISH), 'requests': 400, 'responsesWithTimings': 400, 'httpStatuses': {'200': 400},
        'transportErrors': 0, 'lengthFinishes': 0, 'maxPromptTokens': 2686, 'maxCompletionTokens': 210, 'maxTotalTokens': 2896,
        'minTokensPerSecond': 45.2, 'medianTokensPerSecond': 47.0,
    }
    performance.update(overrides.pop('performance', {}))
    report = {'cases': 400, 'passed': True, 'performance': performance, 'server': {'buildInfo': 'b10666-4e97ac86e', 'contextSize': 4096}}
    report.update(overrides)
    return report


class HardwareReportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dir = Path(self.temp.name)

    def build(self, samples=None, reports=None, **limits):
        samples_path = self.dir / 'samples.csv'
        write_samples(samples_path, **(samples or {}))
        reports = reports if reports is not None else [('golden.json', benchmark_report())]
        return hardware.build_report(hardware.parse_samples(samples_path), reports, limits.get('vram', 7500), limits.get('rate', 20), limits.get('tolerance', 120))

    def test_a_healthy_run_passes_and_carries_the_evidence_of_both_sources(self):
        report = self.build()
        self.assertTrue(report['passed'], report['failures'])
        self.assertEqual(report['gpu'], 'NVIDIA GeForce RTX 4060')
        self.assertEqual(report['peakObservedVRAMMiB'], 5360)
        self.assertEqual((report['oom'], report['restartsDuringRun']), (0, 0))
        self.assertEqual((report['minimumObservedTokensPerSecond'], report['maximumPromptTokens'], report['maximumTotalTokens'], report['contextSize']), (45.2, 2686, 2896, 4096))
        self.assertEqual((report['responsesWithTimings'], report['llamaCppBuilds']), (400, ['b10666-4e97ac86e']))

    def test_each_violation_is_reported_and_fails_the_report(self):
        cases = {
            'пик памяти': (dict(samples=dict(used=7700)), 'пик видеопамяти'),
            'OOM': (dict(samples=dict(oom='true')), 'OOM'),
            'перезапуск': (dict(samples=dict(rows_override=lambda e: [e, 'NVIDIA GeForce RTX 4060', '595.91.07', 5000, 8188, 0 if e < START + 100 else 1, 'false', 'true', 'sha256:aaaa'])), 'перезапускался'),
            'драйвер сменился': (dict(samples=dict(rows_override=lambda e: [e, 'NVIDIA GeForce RTX 4060', '595.71.05' if e < START + 100 else '595.91.07', 5000, 8188, 0, 'false', 'true', 'sha256:aaaa'])), 'версия драйвера менялась'),
            'образ сменился': (dict(samples=dict(rows_override=lambda e: [e, 'NVIDIA GeForce RTX 4060', '595.91.07', 5000, 8188, 0, 'false', 'true', 'sha256:aaaa' if e < START + 100 else 'sha256:bbbb'])), 'образ контейнера менялся'),
            'контейнер остановлен': (dict(samples=dict(running='false')), 'не запущен'),
            'снимки кончились раньше прогона': (dict(samples=dict(last=FINISH - 200)), 'не покрывают'),
            'снимки начались позже прогона': (dict(samples=dict(first=START + 200)), 'не покрывают'),
            'медленная генерация': (dict(reports=[('r.json', benchmark_report(performance={'minTokensPerSecond': 12.5}))]), 'скорость генерации'),
            'оборванный ответ': (dict(reports=[('r.json', benchmark_report(performance={'lengthFinishes': 2}))]), 'оборванных по длине'),
            'ответ сервера 400': (dict(reports=[('r.json', benchmark_report(performance={'httpStatuses': {'200': 399, '400': 1}}))]), 'ошибок обращения'),
            'не умещается в контекст': (dict(reports=[('r.json', benchmark_report(performance={'maxTotalTokens': 4300}))]), 'не помещается в контекст'),
            'разные сборки': (dict(reports=[('a.json', benchmark_report()), ('b.json', benchmark_report(server={'buildInfo': 'b1', 'contextSize': 4096}))]), 'разных сборках'),
            'нет времени прогона': (dict(reports=[('r.json', benchmark_report(performance={'startedAt': '', 'finishedAt': ''}))]), 'покрытие снимков не проверить'),
        }
        for name, (arguments, expected) in cases.items():
            with self.subTest(name):
                report = self.build(**arguments)
                self.assertFalse(report['passed'])
                self.assertTrue(any(expected in failure for failure in report['failures']), report['failures'])

    def test_the_driver_and_the_image_are_recorded_and_old_sample_files_still_work(self):
        report = self.build()
        self.assertEqual((report['driverVersions'], report['imageIds']), (['595.91.07'], ['sha256:aaaa']))
        legacy = self.dir / 'legacy.csv'
        write_samples(legacy, legacy=True)
        old = hardware.build_report(hardware.parse_samples(legacy), [('golden.json', benchmark_report())], 7500, 20, 120)
        self.assertTrue(old['passed'], old['failures'])
        self.assertEqual((old['driverVersions'], old['imageIds']), ([], []))

    def test_a_tolerance_covers_clock_skew_between_the_node_and_the_runner(self):
        report = self.build(samples=dict(first=START + 90), tolerance=120)
        self.assertTrue(report['passed'], report['failures'])
        self.assertFalse(self.build(samples=dict(first=START + 90), tolerance=30)['passed'])

    def test_several_reports_are_merged_into_one_verdict(self):
        reports = [('golden.json', benchmark_report()),
                   ('dev.json', benchmark_report(performance={'maxPromptTokens': 3048, 'maxTotalTokens': 3300, 'minTokensPerSecond': 44.1, 'requests': 100, 'responsesWithTimings': 100}))]
        report = self.build(reports=reports)
        self.assertTrue(report['passed'], report['failures'])
        self.assertEqual((report['requests'], report['responsesWithTimings'], report['maximumPromptTokens'], report['minimumObservedTokensPerSecond']), (500, 500, 3048, 44.1))

    def test_a_report_without_measurements_and_unreadable_samples_are_refused(self):
        with self.assertRaises(SystemExit) as raised:
            self.build(reports=[('old.json', {'cases': 400})])
        self.assertIn('performance', str(raised.exception))
        empty = self.dir / 'empty.csv'
        write_samples(empty, rows_override=lambda e: [e, '', '', '', '', 0, 'false', 'true', ''])
        self.assertEqual(hardware.parse_samples(empty), [])
        with self.assertRaises(SystemExit):
            hardware.build_report([], [('r.json', benchmark_report())], 7500, 20, 120)

    def test_the_command_writes_the_report_and_its_exit_code_follows_the_verdict(self):
        samples, golden, out = self.dir / 's.csv', self.dir / 'golden.json', self.dir / 'hardware.json'
        write_samples(samples)
        golden.write_text(json.dumps(benchmark_report()))
        self.assertEqual(hardware.main(['--samples', str(samples), '--benchmark', str(golden), '--out', str(out)]), 0)
        written = json.loads(out.read_text())
        self.assertTrue(written['passed'])
        self.assertEqual((written['samplesFile'], written['samplesSha256']), ('s.csv', hashlib.sha256(samples.read_bytes()).hexdigest()))
        write_samples(samples, used=7800)
        self.assertEqual(hardware.main(['--samples', str(samples), '--benchmark', str(golden), '--out', str(out)]), 1)
        self.assertFalse(json.loads(out.read_text())['passed'])


FAKE_DOCKER = r'''#!/usr/bin/env bash
case "$1" in
  exec) [ -n "${FAKE_NO_GPU:-}" ] && exit 1; echo "NVIDIA GeForce RTX 4060, 595.91.07, 5360, 8188" ;;
  inspect) echo "${FAKE_STATE:-0,false,true,sha256:abc123}" ;;
esac
'''


class SamplerTests(unittest.TestCase):
    def sample(self, **env):
        with tempfile.TemporaryDirectory() as directory:
            bin_dir = Path(directory) / 'bin'
            bin_dir.mkdir()
            (bin_dir / 'docker').write_text(FAKE_DOCKER)
            (bin_dir / 'docker').chmod(0o755)
            out = Path(directory) / 'samples.csv'
            process = subprocess.Popen(['bash', str(SCRIPTS / 'ai-node-sample.sh'), str(out), 'model-container'],
                                       env={**os.environ, 'PATH': f'{bin_dir}{os.pathsep}{os.environ["PATH"]}', 'LIDRADAR_SAMPLE_INTERVAL': '0.2', **env})
            time.sleep(1.3)
            process.send_signal(signal.SIGTERM)
            self.assertEqual(process.wait(timeout=5), 0)
            return out.read_text().splitlines(), hardware.parse_samples(out)

    def test_rows_carry_time_gpu_memory_and_container_state(self):
        lines, rows = self.sample(FAKE_STATE='2,true,false,sha256:abc123')
        self.assertEqual(lines[0], 'epoch,gpu,driverVersion,memoryUsedMiB,memoryTotalMiB,restartCount,oomKilled,running,imageId')
        self.assertGreaterEqual(len(lines), 4)
        epoch, gpu, driver, used, total, restarts, oom, running, image = lines[1].split(',')
        self.assertTrue(epoch.isdigit())
        self.assertEqual((gpu, driver, used, total, restarts, oom, running, image),
                         ('NVIDIA GeForce RTX 4060', '595.91.07', '5360', '8188', '2', 'true', 'false', 'sha256:abc123'))
        self.assertEqual((rows[0]['used'], rows[0]['restarts'], rows[0]['oom'], rows[0]['running'], rows[0]['driver'], rows[0]['image']),
                         (5360, 2, True, False, '595.91.07', 'sha256:abc123'))

    def test_an_unavailable_gpu_yields_rows_the_report_ignores(self):
        lines, rows = self.sample(FAKE_NO_GPU='1')
        self.assertEqual(len(lines[1].split(',')), 9)
        self.assertEqual(rows, [])

    def test_the_output_file_is_required(self):
        result = subprocess.run(['bash', str(SCRIPTS / 'ai-node-sample.sh')], capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)


if __name__ == '__main__':
    unittest.main()
