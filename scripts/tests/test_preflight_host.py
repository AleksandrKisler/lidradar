"""Проверка хоста перед первым запуском (scripts/preflight-host.sh) на поддельных утилитах.

Ничего не выходит за пределы машины: curl, getent, docker, ss, timeout, uname, df и timedatectl подменены.
Каждый дефект включается переменной QA_BREAK и должен давать ровно то замечание, которое ему положено.
"""
from pathlib import Path
import os
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'preflight-host.sh'

FAKE_CURL = r'''#!/usr/bin/env bash
broken=",${QA_BREAK:-},"
url="${@: -1}"
has() { [[ "${broken}" == *",$1,"* ]]; }
case "${url}" in
  https://api.ipify.org|https://ifconfig.me|https://icanhazip.com)
    has no_public_ip && exit 6
    printf '203.0.113.10\n'; exit 0 ;;
  https://registry-1.docker.io/v2/)
    has hub_down && { printf '000'; exit 28; }
    printf '401'; exit 0 ;;
  https://acme-v02.api.letsencrypt.org/directory)
    has acme_down && { printf '000'; exit 28; }
    printf '200'; exit 0 ;;
  https://api.telegram.org/)
    has telegram_down && { printf '000'; exit 28; }
    printf '302'; exit 0 ;;
  https://storage.example.test|https://storage.example.test/*)
    has s3_down && { printf '000'; exit 28; }
    printf '403'; exit 0 ;;
esac
printf '000'; exit 28
'''

FAKE_GETENT = r'''#!/usr/bin/env bash
broken=",${QA_BREAK:-},"
has() { [[ "${broken}" == *",$1,"* ]]; }
case "$1" in
  ahostsv4)
    has dns_missing && exit 2
    if has dns_mismatch; then printf '198.51.100.9 STREAM %s\n' "$2"; exit 0; fi
    printf '203.0.113.10 STREAM %s\n' "$2"
    has two_records && printf '203.0.113.11 STREAM %s\n' "$2"
    exit 0 ;;
  ahostsv6)
    # glibc отдаёт для домена только с A-записью отображённый адрес ::ffff:…, он не считается AAAA.
    printf '::ffff:203.0.113.10 STREAM %s\n' "$2"
    has aaaa && printf '2001:db8::10 STREAM %s\n' "$2"
    exit 0 ;;
esac
exit 2
'''

FAKE_DOCKER = r'''#!/usr/bin/env bash
broken=",${QA_BREAK:-},"
has() { [[ "${broken}" == *",$1,"* ]]; }
echo "docker $*" >> "${QA_CALLS:-/dev/null}"
case "$1" in
  version)
    has docker_down && exit 1
    printf '28.1.1\n'; exit 0 ;;
  compose)
    has compose_missing && exit 1
    has compose_old && { printf 'v2.5.0\n'; exit 0; }
    printf 'v2.35.1\n'; exit 0 ;;
  pull)
    has pull_fail && exit 1
    exit 0 ;;
esac
exit 0
'''

FAKE_SS = r'''#!/usr/bin/env bash
broken=",${QA_BREAK:-},"
has() { [[ "${broken}" == *",$1,"* ]]; }
case "$*" in
  *':80') has port80_busy && printf 'LISTEN 0 4096 0.0.0.0:80 0.0.0.0:*\n' ;;
  *':443') has port443_busy && printf 'LISTEN 0 4096 0.0.0.0:443 0.0.0.0:*\n' ;;
esac
exit 0
'''

FAKE_TIMEOUT = r'''#!/usr/bin/env bash
broken=",${QA_BREAK:-},"
has() { [[ "${broken}" == *",$1,"* ]]; }
shift
case "$*" in
  *'/dev/tcp/smtp.example.test/'*)
    has smtp_closed && exit 1
    has smtp_garbage && { printf '554 no service here'; exit 0; }
    printf '220 smtp.example.test ESMTP ready'; exit 0 ;;
  *'/dev/tcp/'*) exit 1 ;;
esac
exec "$@"
'''

FAKE_UNAME = r'''#!/usr/bin/env bash
broken=",${QA_BREAK:-},"
case "$1" in
  -s) [[ "${broken}" == *",darwin,"* ]] && echo Darwin || echo Linux ;;
  -m) [[ "${broken}" == *",odd_arch,"* ]] && echo riscv64 || echo x86_64 ;;
esac
'''

FAKE_DF = r'''#!/usr/bin/env bash
broken=",${QA_BREAK:-},"
available=41943040
[[ "${broken}" == *",disk_low,"* ]] && available=5242880
[[ "${broken}" == *",disk_warn,"* ]] && available=12582912
printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n'
printf '/dev/vda1 62914560 10000000 %s 20%% /\n' "${available}"
'''

FAKE_TIMEDATECTL = r'''#!/usr/bin/env bash
broken=",${QA_BREAK:-},"
[[ "${broken}" == *",ntp_off,"* ]] && echo no || echo yes
'''

TOOLS = {
    'curl': FAKE_CURL, 'getent': FAKE_GETENT, 'docker': FAKE_DOCKER, 'ss': FAKE_SS,
    'timeout': FAKE_TIMEOUT, 'uname': FAKE_UNAME, 'df': FAKE_DF, 'timedatectl': FAKE_TIMEDATECTL,
}

COMPOSE = """\
services:
  edge:
    image: caddy:2.11.7-alpine
  postgres:
    image: postgres:18-alpine
  api:
    image: lidradar-api:${LIDRADAR_VERSION}
"""


class PreflightHostTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.bin = root / 'bin'
        self.bin.mkdir()
        for name, text in TOOLS.items():
            (self.bin / name).write_text(text)
            (self.bin / name).chmod(0o755)
        self.calls = root / 'docker-calls'
        self.calls.write_text('')
        self.meminfo = root / 'meminfo'
        self.set_memory(4046848)
        compose_dir = root / 'deploy'
        compose_dir.mkdir()
        (compose_dir / 'compose.yaml').write_text(COMPOSE)
        self.compose_dir = compose_dir
        self.env = {**os.environ, 'PATH': f'{self.bin}{os.pathsep}{os.environ["PATH"]}',
                    'PREFLIGHT_MEMINFO': str(self.meminfo), 'QA_CALLS': str(self.calls)}
        self.env.pop('QA_BREAK', None)

    def set_memory(self, kilobytes):
        self.meminfo.write_text(f'MemTotal:       {kilobytes} kB\nMemFree:        1000 kB\n')

    def run_script(self, *args, broken=''):
        env = dict(self.env)
        if broken:
            env['QA_BREAK'] = broken
        return subprocess.run(['bash', str(SCRIPT), *args], env=env, capture_output=True, text=True)

    BASE = ['--domain', 'app.example.test', '--smtp', 'smtp.example.test:587', '--s3-endpoint', 'https://storage.example.test']

    def test_a_healthy_host_has_no_blocking_remarks(self):
        result = self.run_script(*self.BASE)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('блокирующих замечаний нет, предупреждений 0', result.stdout)
        self.assertNotIn('FAIL', result.stdout + result.stderr)
        self.assertNotIn('warn ', result.stdout)
        for expected in ('Linux x86_64', 'Docker 28.1.1', 'Compose 2.35.1', 'памяти 3.9 ГБ', 'свободно 40 ГБ', 'часы синхронизированы',
                         'порт 80 свободен', 'порт 443 свободен', 'app.example.test указывает на 203.0.113.10', 'AAAA-записи нет',
                         'Docker Hub (registry-1.docker.io) отвечает', "Let's Encrypt", 'Telegram Bot API', 'SMTP smtp.example.test:587 отвечает баннером 220',
                         'хранилище копий https://storage.example.test отвечает'):
            self.assertIn(expected, result.stdout)
        # Счёт ловит проверку, молча выпавшую из скрипта.
        self.assertEqual(result.stdout.count('ok    '), 15, result.stdout)

    def test_each_blocking_defect_is_reported(self):
        cases = {
            'darwin': 'система Darwin: боевой стек рассчитан на Linux',
            'docker_down': 'демон Docker не отвечает',
            'compose_missing': 'плагин Docker Compose v2 не найден',
            'port80_busy': 'порт 80 занят',
            'port443_busy': 'порт 443 занят',
            'dns_missing': 'A-запись app.example.test не найдена',
            'dns_mismatch': 'app.example.test указывает на 198.51.100.9, а внешний адрес хоста 203.0.113.10',
            'acme_down': "Let's Encrypt недоступен (HTTP 000)",
            'telegram_down': 'Telegram Bot API (api.telegram.org) недоступен',
            'smtp_closed': 'SMTP smtp.example.test:587 недоступен: хостер, вероятно, закрыл исходящий порт',
            's3_down': 'хранилище копий https://storage.example.test недоступно',
            'disk_low': 'меньше 10 ГБ',
        }
        for defect, message in cases.items():
            with self.subTest(defect=defect):
                result = self.run_script(*self.BASE, broken=defect)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn(message, result.stdout)
                self.assertIn('блокирующих замечаний 1', result.stderr)

    def test_missing_docker_binary_is_a_blocking_remark(self):
        # Без docker в PATH, но с остальными утилитами.
        (self.bin / 'docker').unlink()
        minimal = os.pathsep.join([str(self.bin), '/usr/bin', '/bin'])
        env = {**self.env, 'PATH': minimal}
        result = subprocess.run(['bash', str(SCRIPT), *self.BASE], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn('docker не найден', result.stdout)

    def test_warnings_do_not_block_the_start(self):
        cases = {
            'hub_down': 'Docker Hub недоступен с хоста (HTTP 000): образы caddy и postgres доставьте через docker save',
            'aaaa': 'есть AAAA-запись (2001:db8::10)',
            'ntp_off': 'часы не синхронизированы',
            'disk_warn': 'рекомендуется не меньше 15 ГБ',
            'compose_old': 'Compose 2.5.0 старее проверенной 2.35.1',
            'odd_arch': 'архитектура riscv64 не проверялась',
            'two_records': 'несколько A-записей',
            'smtp_garbage': 'отвечает не баннером 220: 554 no service here',
        }
        for name, message in cases.items():
            with self.subTest(name=name):
                result = self.run_script(*self.BASE, broken=name)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn('warn  ', result.stdout)
                self.assertIn(message, result.stdout)

    def test_memory_thresholds(self):
        self.set_memory(1500000)
        low = self.run_script(*self.BASE)
        self.assertEqual(low.returncode, 1)
        self.assertIn('меньше 2 ГБ', low.stdout)
        self.set_memory(3000000)
        medium = self.run_script(*self.BASE)
        self.assertEqual(medium.returncode, 0, medium.stdout)
        self.assertIn('рекомендуется не меньше 4 ГБ', medium.stdout)
        (self.meminfo).unlink()
        unknown = self.run_script(*self.BASE)
        self.assertEqual(unknown.returncode, 0, unknown.stdout)
        self.assertIn('объём памяти прочитать не удалось', unknown.stdout)

    def test_public_address_can_be_given_or_is_detected(self):
        undetected = self.run_script(*self.BASE, broken='no_public_ip')
        self.assertEqual(undetected.returncode, 0, undetected.stdout)
        self.assertIn('внешний адрес хоста определить не удалось (задайте --public-ip)', undetected.stdout)
        given = self.run_script(*self.BASE, '--public-ip', '203.0.113.10', broken='no_public_ip')
        self.assertEqual(given.returncode, 0, given.stdout)
        self.assertIn('указывает на 203.0.113.10', given.stdout)
        wrong = self.run_script(*self.BASE, '--public-ip', '192.0.2.77', broken='no_public_ip')
        self.assertEqual(wrong.returncode, 1)
        self.assertIn('а внешний адрес хоста 192.0.2.77', wrong.stdout)

    def test_unknown_storage_is_a_warning_not_a_failure(self):
        result = self.run_script('--domain', 'app.example.test', '--smtp', 'smtp.example.test:587')
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn('адрес хранилища копий не задан', result.stdout)

    def test_third_party_images_are_pulled_on_request(self):
        result = self.run_script(*self.BASE, '--pull-images', '--compose-dir', str(self.compose_dir))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('образ caddy:2.11.7-alpine скачан', result.stdout)
        self.assertIn('образ postgres:18-alpine скачан', result.stdout)
        calls = self.calls.read_text()
        self.assertIn('docker pull --quiet caddy:2.11.7-alpine', calls)
        self.assertIn('docker pull --quiet postgres:18-alpine', calls)
        self.assertNotIn('lidradar-api', calls, 'образы выпуска не скачиваются: они приходят с docker load')
        failing = self.run_script(*self.BASE, '--pull-images', '--compose-dir', str(self.compose_dir), broken='pull_fail')
        self.assertEqual(failing.returncode, 1)
        self.assertIn('не скачался: доставьте его через docker save', failing.stdout)
        missing = self.run_script(*self.BASE, '--pull-images', '--compose-dir', str(self.compose_dir / 'нет'))
        self.assertEqual(missing.returncode, 1)
        self.assertIn('не найден', missing.stdout)
        # Без флага скачивания не происходит.
        self.calls.write_text('')
        self.run_script(*self.BASE)
        self.assertNotIn('docker pull', self.calls.read_text())

    def test_invalid_arguments_are_refused_before_any_check(self):
        for args in (
            [], ['--domain'], ['--domain', 'localhost'], ['--domain', 'bad_domain.example.test'], ['--domain', 'a..example.test'],
            ['--domain', 'app.example.test', '--smtp', 'smtp.example.test'], ['--domain', 'app.example.test', '--public-ip', '999.1'],
            ['--domain', 'app.example.test', '--s3-endpoint', 'storage.example.test'], ['--domain', 'app.example.test', '--bogus'],
        ):
            with self.subTest(args=args):
                result = self.run_script(*args)
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                self.assertNotIn('== Хост ==', result.stdout)

    def test_the_script_changes_nothing_on_the_machine(self):
        before = sorted(p.name for p in Path(self.temp.name).iterdir())
        self.run_script(*self.BASE)
        self.assertEqual(sorted(p.name for p in Path(self.temp.name).iterdir()), before)
        # Единственные вызовы docker — чтение версий.
        self.assertTrue(all(line.startswith(('docker version', 'docker compose version')) for line in self.calls.read_text().splitlines()))


if __name__ == '__main__':
    unittest.main()
