"""Контейнер копий вне хоста (ADR 0051) на подставных pg_dump, psql, age, rclone и curl.

Сеть и база не используются: «хранилище» — каталог на диске, «шифрование» —
обратимая подстановка с проверкой получателей. Скрипты запускаются тем bash, что
есть на машине (на macOS это 3.2), поэтому тесты заодно держат их переносимыми.
"""
from pathlib import Path
import json
import os
import shutil
import subprocess
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[2]
BACKUP = ROOT / 'deploy' / 'backup'
CYCLE = BACKUP / 'lidradar-backup-cycle'
CLI = BACKUP / 'lidradar-backup'
LIB = BACKUP / 'lib.sh'

RECIPIENT_A = 'age1' + 'a' * 58
RECIPIENT_B = 'age1' + 'b' * 58
DB_PASSWORD = 'db-password-for-tests'
S3_SECRET = 's3-secret-for-tests'
PING_TOKEN = 'ping-token-for-tests'
COUNTS = '49|000023_unfinished_agreements|1|10|2|3|4'

FAKE_AGE = r'''#!/usr/bin/env python3
import base64, re, sys
args = sys.argv[1:]
with open(__import__("os").environ["QA_CALLS"], "a") as calls:
    calls.write("age " + " ".join(args) + "\n")
if "QA_AGE_FAIL" in __import__("os").environ and "-o" in args:
    sys.stderr.write("age: error: boom\n"); sys.exit(1)
if "-d" in args:
    identity = args[args.index("-i") + 1]
    out = args[args.index("-o") + 1]
    source = args[-1]
    owner = open(identity).read().split("FOR:")[1].split()[0]
    text = open(source, "rb").read()
    header, body = text.split(b"\n", 1)
    recipients = header.decode().split("recipients=")[1].split(",")
    if owner not in recipients:
        sys.stderr.write("age: error: no identity matched any of the recipients\n"); sys.exit(1)
    open(out, "wb").write(base64.b64decode(body))
    sys.exit(0)
recipients = [args[i + 1] for i, a in enumerate(args) if a == "-r"]
for r in recipients:
    if not re.fullmatch(r"age1[a-z0-9]{20,}", r):
        sys.stderr.write("age: error: malformed recipient %r\n" % r); sys.exit(1)
if "-o" not in args:
    sys.stdin.read(); sys.exit(0)  # проверка получателя: age -r КЛЮЧ < /dev/null
out = args[args.index("-o") + 1]
data = open(args[-1], "rb").read()
open(out, "wb").write(b"age-encryption.org/v1 FAKE recipients=" + ",".join(recipients).encode() + b"\n" + base64.b64encode(data))
'''

FAKE_RCLONE = r'''#!/usr/bin/env python3
import glob, os, shutil, sys, time
args = sys.argv[1:]
env = os.environ
with open(env["QA_CALLS"], "a") as calls:
    calls.write("rclone " + " ".join(args) + "\n")
    if args and args[0] == "copyto":
        calls.write("rclone-env type=%s no_head=%s no_check_bucket=%s endpoint=%s provider=%s no_check_dest=%s\n" % (
            env.get("RCLONE_CONFIG_OFFHOST_TYPE"), env.get("RCLONE_CONFIG_OFFHOST_NO_HEAD"),
            env.get("RCLONE_CONFIG_OFFHOST_NO_CHECK_BUCKET"), env.get("RCLONE_CONFIG_OFFHOST_ENDPOINT"),
            env.get("RCLONE_CONFIG_OFFHOST_PROVIDER"), env.get("RCLONE_NO_CHECK_DEST")))
remote_dir = env["QA_REMOTE_DIR"]

import re

def is_remote(path):
    return re.match(r"^(:[a-z0-9]+:|[A-Za-z0-9_][A-Za-z0-9_.-]*:)", path) is not None

def local(path):
    tail = path.split(":", 2)[2] if path.startswith(":") else path.split(":", 1)[1]
    return os.path.join(remote_dir, tail.lstrip("/"))

def fail(message, code=1):
    sys.stderr.write(message + "\n"); sys.exit(code)

if args[0] == "copyto":
    src, dst = args[1], args[2]
    if is_remote(dst):
        # Открытый текст выгрузки к началу загрузки должен быть уже удалён с диска.
        if glob.glob(os.path.join(env["LIDRADAR_BACKUP_SPOOL"], "work-*", "plain", "*.dump")):
            fail("ERROR : plaintext dump is still on disk during the upload")
        time.sleep(float(env.get("QA_RCLONE_DELAY", "0")))
        if env.get("QA_RCLONE_FAIL_UPLOAD"):
            fail("ERROR : upload refused (fake): secret=" + "x" * 3)
        if env.get("QA_RCLONE_FAIL_MATCH") and env["QA_RCLONE_FAIL_MATCH"] in dst:
            fail("ERROR : upload of this object refused (fake)")
        target = local(dst)
        os.makedirs(os.path.dirname(target), exist_ok=True)
        shutil.copyfile(src, target)
        if env.get("QA_RCLONE_CORRUPT") and target.endswith(".dump.age"):
            with open(target, "ab") as handle:
                handle.write(b"corrupted")
    else:
        if env.get("QA_RCLONE_FAIL_DOWNLOAD"):
            fail("ERROR : download refused (fake)")
        source = local(src)
        if not os.path.exists(source):
            fail("ERROR : object not found", 3)
        shutil.copyfile(source, dst)
elif args[0] == "lsf":
    directory = local(args[-1])
    if not os.path.isdir(directory):
        fail("ERROR : directory not found", 3)
    for name in sorted(os.listdir(directory)):
        print(name)
elif args[0] == "cat":
    source = local(args[-1])
    if not os.path.exists(source):
        fail("ERROR : object not found", 3)
    sys.stdout.buffer.write(open(source, "rb").read())
else:
    fail("fake rclone: unsupported command " + args[0], 9)
'''

FAKE_PSQL = r'''#!/usr/bin/env bash
set -eu
echo "psql $*" >> "$QA_CALLS"
[ -z "${QA_PSQL_FAIL:-}" ] || { echo 'psql: error: connection refused' >&2; exit 2; }
case "$*" in
  *lidradar_platform*) echo "${QA_SEES_ALL_ROWS:-t}" ;;
  *schema_migrations*)
    if [[ "$*" == *lidradar_drill_* ]]; then
      echo "${QA_COUNTS_RESTORED:-$QA_COUNTS_BEFORE}"
    else
      n="$(cat "$QA_STATE/counts" 2>/dev/null || echo 0)"
      echo $((n + 1)) > "$QA_STATE/counts"
      if [ "$n" -eq 0 ]; then echo "$QA_COUNTS_BEFORE"; else echo "${QA_COUNTS_AFTER:-$QA_COUNTS_BEFORE}"; fi
    fi ;;
  *server_version*) echo 18.6 ;;
esac
'''

FAKE_PG_DUMP = r'''#!/usr/bin/env bash
set -eu
echo "pg_dump $*" >> "$QA_CALLS"
if [ "${1:-}" = "--version" ]; then echo 'pg_dump (PostgreSQL) 18.6'; exit 0; fi
[ -z "${QA_DUMP_FAIL:-}" ] || { echo 'pg_dump: error: connection to server failed' >&2; exit 1; }
echo "PGDMP-plaintext-marker-${QA_DUMP_TOKEN:-one}"
head -c 4096 /dev/zero
'''

FAKE_PG_RESTORE = r'''#!/usr/bin/env bash
set -eu
echo "pg_restore $*" >> "$QA_CALLS"
case "$*" in
  *--list*) [ -z "${QA_TOC_FAIL:-}" ] || { echo 'pg_restore: error: unsupported version' >&2; exit 1; } ;;
  *) [ -z "${QA_RESTORE_FAIL:-}" ] || { echo 'pg_restore: error: could not restore' >&2; exit 1; } ;;
esac
'''

FAKE_CURL = r'''#!/usr/bin/env bash
set -eu
echo "curl ${@: -1}" >> "$QA_CALLS"
[ -z "${QA_CURL_FAIL:-}" ] || exit 22
'''

FAKE_TIMEOUT = r'''#!/usr/bin/env bash
if [ -n "${QA_TIMEOUT_HIT:-}" ]; then exit 124; fi
shift
exec "$@"
'''

FAKE_SLEEP = '#!/usr/bin/env bash\necho "sleep $*" >> "$QA_CALLS"\n'


def have_jq():
    return shutil.which('jq') is not None


@unittest.skipUnless(have_jq(), 'jq is required')
class OffhostBackupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.spool = self.root / 'spool'
        self.remote_dir = self.root / 'remote'
        self.remote_dir.mkdir()
        self.state = self.root / 'state'
        self.state.mkdir()
        self.calls = self.root / 'calls'
        self.calls.write_text('')
        for name, body in {'age': FAKE_AGE, 'rclone': FAKE_RCLONE, 'psql': FAKE_PSQL, 'pg_dump': FAKE_PG_DUMP,
                           'pg_restore': FAKE_PG_RESTORE, 'curl': FAKE_CURL, 'timeout': FAKE_TIMEOUT, 'sleep': FAKE_SLEEP}.items():
            self.tool(name, body)
        if not shutil.which('flock'):
            self.tool('flock', '#!/usr/bin/env bash\nexit 0\n')
        self.env = {
            **os.environ,
            'PATH': f'{self.bin}{os.pathsep}{os.environ["PATH"]}',
            'LIDRADAR_ENV': 'staging', 'LIDRADAR_VERSION': 'abc123def456',
            'LIDRADAR_BACKUP_SPOOL': str(self.spool), 'LIDRADAR_BACKUP_SCRIPT': str(ROOT / 'scripts' / 'backup.sh'),
            'LIDRADAR_BACKUP_DATABASE_URL': f'postgres://lidradar:{DB_PASSWORD}@db:5432/lidradar?sslmode=disable',
            'LIDRADAR_BACKUP_REMOTE': 'offhost:bucket/prefix',
            'LIDRADAR_BACKUP_AGE_RECIPIENTS': f'{RECIPIENT_A}, {RECIPIENT_B}',
            'LIDRADAR_BACKUP_S3_ENDPOINT': 'https://s3.example.test', 'LIDRADAR_BACKUP_S3_ACCESS_KEY_ID': 'access-key-id',
            'LIDRADAR_BACKUP_S3_SECRET_ACCESS_KEY': S3_SECRET,
            'LIDRADAR_BACKUP_PING_URL': f'https://ping.example.test/ok/{PING_TOKEN}',
            'LIDRADAR_BACKUP_FAIL_URL': f'https://ping.example.test/fail/{PING_TOKEN}',
            'QA_CALLS': str(self.calls), 'QA_REMOTE_DIR': str(self.remote_dir), 'QA_STATE': str(self.state),
            'QA_COUNTS_BEFORE': COUNTS,
        }
        for name in ('LIDRADAR_BACKUP_INTERVAL', 'LIDRADAR_BACKUP_MAX_AGE', 'LIDRADAR_BACKUP_CYCLE_TIMEOUT', 'LIDRADAR_BACKUP_MAX_CYCLES'):
            self.env.pop(name, None)

    # --- помощники ---------------------------------------------------------------

    def tool(self, name, body):
        path = self.bin / name
        path.write_text(body)
        path.chmod(0o755)

    def run_cli(self, *args, env=None, script=CLI):
        merged = {**self.env, **(env or {})}
        return subprocess.run(['bash', str(script), *map(str, args)], env=merged, capture_output=True, text=True)

    def cycle(self, *args, env=None):
        return self.run_cli(*args, env=env, script=CYCLE)

    def events(self, result):
        lines = [json.loads(line) for line in result.stderr.splitlines() if line.startswith('{')]
        return [line['event'] for line in lines], lines

    def call_log(self):
        return self.calls.read_text().splitlines()

    def encryptions(self):
        # Вызов age без -o — проверка получателя при старте, с -o — шифрование выгрузки.
        return [line for line in self.call_log() if line.startswith('age ') and ' -o ' in line]

    def remote_files(self, tier):
        directory = self.remote_dir / 'bucket' / 'prefix' / tier
        return sorted(p.name for p in directory.iterdir()) if directory.is_dir() else []

    def status(self):
        return json.loads((self.spool / 'status.json').read_text())

    def write_status(self, **values):
        self.spool.mkdir(parents=True, exist_ok=True)
        (self.spool / 'status.json').write_text(json.dumps(values))

    def one_stamp(self, tier='frequent'):
        names = [n for n in self.remote_files(tier) if n.endswith('.manifest.json')]
        self.assertEqual(len(names), 1, names)
        return names[0].removesuffix('.manifest.json')

    def identity(self, owner=RECIPIENT_A):
        path = self.root / f'identity-{owner[-3:]}.key'
        path.write_text(f'AGE-SECRET-KEY-FOR:{owner}\n')
        return path

    def assertNoSecrets(self, result):
        for secret in (DB_PASSWORD, S3_SECRET, PING_TOKEN):
            self.assertNotIn(secret, result.stdout + result.stderr)

    # --- цикл --------------------------------------------------------------------

    def test_successful_cycle_publishes_an_encrypted_dump_and_the_manifest_last(self):
        result = self.cycle()
        self.assertEqual(result.returncode, 0, result.stderr)
        stamp = self.one_stamp()
        self.assertEqual(self.remote_files('frequent'), [f'{stamp}.dump.age', f'{stamp}.manifest.json'])
        dump = (self.remote_dir / 'bucket' / 'prefix' / 'frequent' / f'{stamp}.dump.age').read_bytes()
        self.assertTrue(dump.startswith(b'age-encryption.org/v1'))
        self.assertNotIn(b'PGDMP-plaintext-marker', dump)
        # Каждый ярус: сначала файл копии, затем манифест; сверочные скачивания идут между ними.
        uploads = [line.rsplit('/', 1)[1] for line in self.call_log() if line.startswith('rclone copyto') and line.split()[-1].startswith('offhost:')]
        self.assertEqual([name.split('.', 1)[1] for name in uploads], ['dump.age', 'manifest.json', 'dump.age', 'manifest.json'])
        manifest = json.loads((self.remote_dir / 'bucket' / 'prefix' / 'frequent' / f'{stamp}.manifest.json').read_text())
        self.assertEqual(manifest['format'], 1)
        self.assertEqual(manifest['tier'], 'frequent')
        self.assertEqual(manifest['migration'], '000023_unfinished_agreements')
        self.assertEqual(manifest['tables'], 49)
        self.assertEqual(manifest['release'], 'abc123def456')
        self.assertEqual(manifest['database'], 'lidradar')
        self.assertEqual(manifest['server'], '18.6')
        self.assertEqual(manifest['recipients'], [RECIPIENT_A, RECIPIENT_B])
        self.assertEqual(manifest['counts']['before']['messages'], 10)
        self.assertEqual(manifest['dump']['bytes'], len(dump))
        self.assertEqual(manifest['dump']['name'], f'{stamp}.dump.age')
        self.assertEqual(len(manifest['dump']['sha256']), 64)
        self.assertNotEqual(manifest['dump']['sha256'], manifest['dump']['plainSha256'])
        # Статус, сигнал мониторингу, чистый спул, журнал без секретов.
        status = self.status()
        self.assertTrue(status['lastAttemptOk'])
        self.assertEqual(status['consecutiveFailures'], 0)
        self.assertEqual(status['lastKey'], f'frequent/{stamp}')
        self.assertIsNone(status['lastGapSeconds'])
        self.assertEqual(sorted(p.name for p in self.spool.iterdir() if p.name.startswith('work-')), [])
        self.assertIn(f'curl https://ping.example.test/ok/{PING_TOKEN}', self.call_log())
        self.assertNotIn(f'curl https://ping.example.test/fail/{PING_TOKEN}', self.call_log())
        events, lines = self.events(result)
        self.assertIn('backup.succeeded', events)
        done = [line for line in lines if line['event'] == 'backup.succeeded'][0]
        self.assertEqual(done['service'], 'lidradar-backup')
        self.assertEqual(done['environment'], 'staging')
        self.assertNoSecrets(result)

    def test_the_first_cycle_of_the_day_also_fills_the_daily_tier_once(self):
        self.assertEqual(self.cycle().returncode, 0)
        stamp = self.one_stamp()
        self.assertEqual(self.remote_files('daily'), [f'{stamp}.dump.age', f'{stamp}.manifest.json'])
        self.assertEqual(json.loads((self.remote_dir / 'bucket' / 'prefix' / 'daily' / f'{stamp}.manifest.json').read_text())['tier'], 'daily')
        today = time.strftime('%Y-%m-%d', time.gmtime())
        self.assertEqual(self.status()['lastDailyDate'], today)
        before = len(self.call_log())
        time.sleep(1.1)  # другой штамп секунды, тот же день
        self.assertEqual(self.cycle().returncode, 0)
        self.assertEqual(len(self.remote_files('daily')), 2)
        self.assertEqual(len(self.remote_files('frequent')), 4)
        self.assertGreater(len(self.call_log()), before)

    def test_s3_settings_become_an_rclone_remote_without_listing_or_head_requests(self):
        self.assertEqual(self.cycle().returncode, 0)
        line = [line for line in self.call_log() if line.startswith('rclone-env')][0]
        self.assertEqual(line, 'rclone-env type=s3 no_head=true no_check_bucket=true endpoint=https://s3.example.test provider=Other no_check_dest=true')

    def test_failed_dump_uploads_nothing_records_the_failure_and_pings_the_failure_url(self):
        result = self.cycle(env={'QA_DUMP_FAIL': '1'})
        self.assertEqual(result.returncode, 1)
        self.assertEqual(self.remote_files('frequent'), [])
        self.assertEqual(self.encryptions(), [])
        status = self.status()
        self.assertFalse(status['lastAttemptOk'])
        self.assertEqual(status['consecutiveFailures'], 1)
        self.assertTrue(status['lastError'].startswith('dump:'))
        self.assertIn(f'curl https://ping.example.test/fail/{PING_TOKEN}', self.call_log())
        self.assertNotIn(f'curl https://ping.example.test/ok/{PING_TOKEN}', self.call_log())
        self.assertIn('backup.failed', self.events(result)[0])
        self.assertNoSecrets(result)

    def test_role_that_does_not_see_all_rows_stops_before_any_dump(self):
        result = self.cycle(env={'QA_SEES_ALL_ROWS': 'f'})
        self.assertEqual(result.returncode, 1)
        self.assertFalse(any(line.startswith('pg_dump --dbname') or line.startswith('pg_dump -F') for line in self.call_log()))
        self.assertIn('lidradar_platform', self.status()['lastError'])

    def test_unreachable_database_is_a_failure_before_any_work(self):
        result = self.cycle(env={'QA_PSQL_FAIL': '1'})
        self.assertEqual(result.returncode, 1)
        self.assertTrue(self.status()['lastError'].startswith('database:'))
        self.assertEqual([line for line in self.call_log() if line.startswith(('pg_dump', 'rclone'))] + self.encryptions(), [])

    def test_unreadable_table_of_contents_stops_before_encryption(self):
        result = self.cycle(env={'QA_TOC_FAIL': '1'})
        self.assertEqual(result.returncode, 1)
        self.assertTrue(self.status()['lastError'].startswith('validate:'))
        self.assertEqual([line for line in self.call_log() if line.startswith('rclone')] + self.encryptions(), [])

    def test_encryption_failure_leaves_nothing_behind(self):
        result = self.cycle(env={'QA_AGE_FAIL': '1'})
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertTrue(self.status()['lastError'].startswith('encrypt:'))
        self.assertEqual(self.remote_files('frequent'), [])
        self.assertEqual([p.name for p in self.spool.iterdir() if p.name.startswith('work-')], [])

    def test_upload_failure_counts_consecutive_failures_and_keeps_the_last_good_point(self):
        self.assertEqual(self.cycle().returncode, 0)
        good = self.status()
        time.sleep(1.1)
        for expected in (1, 2):
            result = self.cycle(env={'QA_RCLONE_FAIL_UPLOAD': '1'})
            self.assertEqual(result.returncode, 1)
            status = self.status()
            self.assertEqual(status['consecutiveFailures'], expected)
            self.assertEqual(status['lastDataEpoch'], good['lastDataEpoch'])
            self.assertEqual(status['lastKey'], good['lastKey'])
            self.assertTrue(status['lastError'].startswith('upload:'))
            self.assertNoSecrets(result)
            time.sleep(1.1)
        self.assertEqual(self.cycle().returncode, 0)
        self.assertEqual(self.status()['consecutiveFailures'], 0)

    def test_a_corrupted_upload_is_caught_by_the_read_back_and_no_manifest_is_published(self):
        result = self.cycle(env={'QA_RCLONE_CORRUPT': '1'})
        self.assertEqual(result.returncode, 1)
        self.assertTrue(self.status()['lastError'].startswith('verify:'))
        self.assertEqual([n for n in self.remote_files('frequent') if n.endswith('.manifest.json')], [])

    def test_a_copy_without_a_manifest_is_never_reported_as_a_point(self):
        result = self.cycle(env={'QA_RCLONE_FAIL_MATCH': '.manifest.json'})
        self.assertEqual(result.returncode, 1)
        self.assertTrue(any(n.endswith('.dump.age') for n in self.remote_files('frequent')))
        self.assertNotIn('lastDataEpoch', self.status())

    def test_a_daily_tier_failure_does_not_fail_the_confirmed_frequent_point(self):
        result = self.cycle(env={'QA_RCLONE_FAIL_MATCH': '/daily/'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('backup.daily_failed', self.events(result)[0])
        status = self.status()
        self.assertTrue(status['lastAttemptOk'])
        self.assertNotIn('lastDailyDate', status)
        self.assertIn(f'curl https://ping.example.test/ok/{PING_TOKEN}', self.call_log())
        self.assertNotIn(f'curl https://ping.example.test/fail/{PING_TOKEN}', self.call_log())

    def test_a_failing_ping_url_does_not_change_the_outcome(self):
        result = self.cycle(env={'QA_CURL_FAIL': '1'})
        self.assertEqual(result.returncode, 0)
        self.assertIn('backup.ping_failed', self.events(result)[0])
        self.assertNoSecrets(result)

    def test_gap_is_measured_from_the_previous_data_point_to_this_completion(self):
        data = int(time.time()) - 600
        self.write_status(lastDataEpoch=data, maxGapSeconds=300, lastDailyDate=time.strftime('%Y-%m-%d', time.gmtime()))
        self.assertEqual(self.cycle().returncode, 0)
        status = self.status()
        self.assertGreaterEqual(status['lastGapSeconds'], 600)
        self.assertLess(status['lastGapSeconds'], 640)
        self.assertEqual(status['maxGapSeconds'], status['lastGapSeconds'])
        self.assertGreater(status['lastDataEpoch'], data)
        # Прежний рекорд больше нового разрыва — остаётся прежним.
        self.write_status(lastDataEpoch=int(time.time()) - 100, maxGapSeconds=900)
        self.assertEqual(self.cycle().returncode, 0)
        self.assertEqual(self.status()['maxGapSeconds'], 900)

    def test_two_cycles_in_the_same_second_never_share_object_names(self):
        # Берём момент в начале секунды, чтобы прежняя копия и новый цикл гарантированно делили её.
        while time.time() % 1 > 0.4:
            time.sleep(0.05)
        stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
        self.write_status(lastDataEpoch=int(time.time()) - 10, lastKey=f'frequent/{stamp}')
        self.assertEqual(self.cycle().returncode, 0)
        self.assertNotEqual(self.status()['lastKey'], f'frequent/{stamp}')
        self.assertEqual([n for n in self.remote_files('frequent') if n.startswith(stamp)], [])

    def test_the_gap_includes_the_time_the_cycle_itself_took(self):
        # Копия считается доступной вне хоста только по завершении загрузки: время цикла входит в разрыв.
        self.write_status(lastDataEpoch=int(time.time()) - 600, lastDailyDate=time.strftime('%Y-%m-%d', time.gmtime()))
        self.assertEqual(self.cycle(env={'QA_RCLONE_DELAY': '1.2'}).returncode, 0)
        self.assertGreaterEqual(self.status()['lastGapSeconds'], 600 + 2)  # две загрузки (файл и манифест) по 1,2 с

    def test_check_mode_uses_its_own_prefix_and_leaves_status_and_monitoring_alone(self):
        result = self.cycle('--check')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.remote_files('frequent'), [])
        self.assertEqual(self.remote_files('daily'), [])
        self.assertEqual(len([n for n in self.remote_files('_check') if n.endswith('.manifest.json')]), 1)
        self.assertFalse((self.spool / 'status.json').exists())
        self.assertEqual([line for line in self.call_log() if line.startswith('curl')], [])
        # Сбой пробного цикла тоже не пишет статус и не поднимает тревогу.
        failed = self.cycle('--check', env={'QA_RCLONE_FAIL_UPLOAD': '1'})
        self.assertEqual(failed.returncode, 1)
        self.assertFalse((self.spool / 'status.json').exists())
        self.assertEqual([line for line in self.call_log() if line.startswith('curl')], [])

    def test_a_dump_in_the_spool_is_plaintext_only_while_the_cycle_runs(self):
        self.assertEqual(self.cycle().returncode, 0)
        leftovers = [p for p in self.spool.rglob('*') if p.is_file() and p.name not in ('status.json',) and p.stat().st_size > 0]
        self.assertEqual(leftovers, [])
        failed = self.cycle(env={'QA_RCLONE_FAIL_UPLOAD': '1'})
        self.assertEqual(failed.returncode, 1)
        self.assertEqual([p for p in self.spool.rglob('*.dump*') if p.is_file()], [])

    @unittest.skipUnless(shutil.which('flock'), 'real flock is required (util-linux)')
    def test_an_overlapping_cycle_is_skipped(self):
        import fcntl
        self.spool.mkdir(parents=True)
        with open(self.spool / '.cycle.lock', 'w') as handle:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.cycle()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('backup.skipped', self.events(result)[0])
        calls = self.call_log()
        self.assertEqual([line for line in calls if line.startswith(('pg_dump', 'rclone'))], [])
        # age вызывается только проверкой получателей (age -r КЛЮЧ до блокировки), данные не шифруются.
        self.assertTrue(all(len(line.split()) == 3 for line in calls if line.startswith('age ')), calls)
        with open(self.spool / '.cycle.lock', 'w') as handle:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            probe = self.cycle('--check')
        # Пробный запрос оператора при занятой блокировке — ошибка, а не молчаливый пропуск.
        self.assertEqual(probe.returncode, 1)
        self.assertIn('backup.busy', self.events(probe)[0])

    # --- настройка ---------------------------------------------------------------

    def test_invalid_configuration_is_refused_before_any_work(self):
        cases = {
            'нет получателей': {'LIDRADAR_BACKUP_AGE_RECIPIENTS': ''},
            'получатель не принят age': {'LIDRADAR_BACKUP_AGE_RECIPIENTS': 'not-a-key'},
            'нет хранилища': {'LIDRADAR_BACKUP_REMOTE': ''},
            'хранилище без пути': {'LIDRADAR_BACKUP_REMOTE': 'offhost'},
            'пробел в хранилище': {'LIDRADAR_BACKUP_REMOTE': 'offhost:bucket/pre fix'},
            'S3 для хранилища с другим именем': {'LIDRADAR_BACKUP_REMOTE': 'other:bucket'},
            'ключ S3 без секрета': {'LIDRADAR_BACKUP_S3_SECRET_ACCESS_KEY': ''},
            'нет адреса базы': {'LIDRADAR_BACKUP_DATABASE_URL': ''},
        }
        for name, change in cases.items():
            with self.subTest(name):
                result = self.cycle(env=change)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn('backup.configuration_invalid', self.events(result)[0])
                self.assertEqual([line for line in self.call_log() if line.startswith(('pg_dump', 'rclone ', 'curl'))], [])
                self.assertNoSecrets(result)

    def test_any_rclone_remote_works_without_the_s3_variables(self):
        env = {'LIDRADAR_BACKUP_REMOTE': ':local:/data/offhost', 'LIDRADAR_BACKUP_S3_ACCESS_KEY_ID': ''}
        result = self.cycle(env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        stored = sorted(p.name for p in (self.remote_dir / 'data' / 'offhost' / 'frequent').iterdir())
        self.assertEqual(len(stored), 2)
        # Переменные хранилища S3 не заданы, поэтому и настроек offhost у rclone нет.
        self.assertIn('rclone-env type=None', [line for line in self.call_log() if line.startswith('rclone-env')][0])

    # --- состояние и расписание --------------------------------------------------

    def test_status_reports_age_and_exit_code(self):
        missing = self.run_cli('status')
        self.assertEqual(missing.returncode, 1)
        self.assertIn('ещё нет', missing.stdout)
        now = int(time.time())
        self.write_status(lastDataEpoch=now - 120, consecutiveFailures=0, maxGapSeconds=640, lastKey='frequent/x')
        fresh = self.run_cli('status')
        self.assertEqual(fresh.returncode, 0, fresh.stdout)
        self.assertIn('возраст 2m', fresh.stdout)
        self.assertIn('наибольший разрыв между точками: 10m40s', fresh.stdout)
        stale = self.run_cli('status', '--max-age', '1m')
        self.assertEqual(stale.returncode, 1)
        self.assertIn('ОШИБКА', stale.stdout)
        data = json.loads(self.run_cli('status', '--json').stdout)
        self.assertTrue(data['ok'])
        self.assertEqual(data['maxAgeSeconds'], 900)
        self.assertGreaterEqual(data['ageSeconds'], 120)
        self.assertFalse(json.loads(self.run_cli('status', '--json', '--max-age', '30s').stdout)['ok'])
        self.assertEqual(self.run_cli('status', '--max-age', 'soon').returncode, 2)

    def test_a_damaged_status_file_reads_as_empty(self):
        self.spool.mkdir()
        (self.spool / 'status.json').write_text('{not json')
        self.assertEqual(self.run_cli('status').returncode, 1)
        self.assertEqual(self.cycle().returncode, 0)
        self.assertTrue(self.status()['lastAttemptOk'])

    def helper(self, expression):
        script = f'. "{LIB}"; {expression}'
        return subprocess.run(['bash', '-c', script], env=self.env, capture_output=True, text=True).stdout.strip()

    def test_schedule_arithmetic(self):
        self.assertEqual(self.helper('duration_seconds 10m'), '600')
        self.assertEqual(self.helper('duration_seconds 2h'), '7200')
        self.assertEqual(self.helper('duration_seconds 90'), '90')
        self.assertEqual(self.helper('duration_seconds 45s'), '45')
        for bad in ('', 'm', '-5m', '1d', '10 m', '1.5h'):
            self.assertEqual(self.helper(f'duration_seconds "{bad}" || echo bad'), 'bad', bad)
        self.assertEqual(self.helper('seconds_until_next 600 1000'), '200')
        self.assertEqual(self.helper('seconds_until_next 600 1200'), '600')
        self.assertEqual(self.helper('seconds_until_next 600 1199'), '1')
        self.assertEqual(self.helper('human_duration 42'), '42s')
        self.assertEqual(self.helper('human_duration 647'), '10m47s')
        self.assertEqual(self.helper('human_duration 3723'), '1h02m03s')
        self.assertEqual(self.helper('url_with_database "postgres://u:p@h:5432/lidradar?sslmode=disable" scratch'), 'postgres://u:p@h:5432/scratch?sslmode=disable')
        self.assertEqual(self.helper('url_with_database "postgres://u:p@h/lidradar" scratch'), 'postgres://u:p@h/scratch')

    def test_the_service_runs_cycles_on_a_wall_clock_grid_and_stops_after_the_limit(self):
        result = self.run_cli('run', env={'LIDRADAR_BACKUP_MAX_CYCLES': '2', 'LIDRADAR_BACKUP_INTERVAL': '10m'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len([line for line in self.call_log() if line.startswith('pg_dump --dbname')]), 2)
        sleeps = [int(line.split()[1]) for line in self.call_log() if line.startswith('sleep ')]
        self.assertEqual(len(sleeps), 1)
        self.assertTrue(1 <= sleeps[0] <= 600, sleeps)
        now = int(time.time())
        # Пауза доводит до границы 10-минутной сетки: к моменту проверки прошло лишь несколько секунд.
        self.assertLessEqual((now + sleeps[0]) % 600, 10)
        self.assertIn('backup.started', self.events(result)[0])

    def test_a_hung_cycle_is_recorded_as_a_timeout_and_a_crash_as_a_failure(self):
        result = self.run_cli('run', env={'LIDRADAR_BACKUP_MAX_CYCLES': '1', 'QA_TIMEOUT_HIT': '1'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.status()['lastError'].startswith('timeout:'))
        self.assertIn(f'curl https://ping.example.test/fail/{PING_TOKEN}', self.call_log())
        crash = self.root / 'crash-cycle'
        crash.write_text('#!/usr/bin/env bash\nexit 5\n')
        crash.chmod(0o755)
        result = self.run_cli('run', env={'LIDRADAR_BACKUP_MAX_CYCLES': '1', 'LIDRADAR_BACKUP_CYCLE': str(crash)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.status()['consecutiveFailures'], 2)
        self.assertTrue(self.status()['lastError'].startswith('cycle:'))

    def test_the_service_refuses_unsafe_or_malformed_schedules(self):
        for value in ('30s', '0', 'often', '1d'):
            with self.subTest(value):
                result = self.run_cli('run', env={'LIDRADAR_BACKUP_INTERVAL': value, 'LIDRADAR_BACKUP_MAX_CYCLES': '1'})
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn('backup.configuration_invalid', self.events(result)[0])
        result = self.run_cli('run', env={'LIDRADAR_BACKUP_MAX_AGE': 'soon', 'LIDRADAR_BACKUP_MAX_CYCLES': '1'})
        self.assertEqual(result.returncode, 2)
        self.assertEqual([line for line in self.call_log() if line.startswith('pg_dump --dbname')], [])

    def test_the_service_removes_leftovers_of_an_interrupted_cycle(self):
        stale = self.spool / 'work-20260101T000000Z' / 'plain'
        stale.mkdir(parents=True)
        (stale / 'lidradar-old.dump').write_text('plaintext from a killed cycle')
        self.assertEqual(self.run_cli('run', env={'LIDRADAR_BACKUP_MAX_CYCLES': '1'}).returncode, 0)
        self.assertFalse(stale.exists())

    # --- чтение и восстановление -------------------------------------------------

    def publish(self, count=1, tier='frequent'):
        stamps = []
        for index in range(count):
            self.assertEqual(self.cycle(env={'QA_DUMP_TOKEN': f'dump-{index}'}).returncode, 0)
            stamps = sorted(n.removesuffix('.manifest.json') for n in self.remote_files(tier) if n.endswith('.manifest.json'))
            if index + 1 < count:
                time.sleep(1.1)
        return stamps

    def fetch(self, *args, identity=None, env=None):
        out = self.root / 'out'
        return self.run_cli('fetch', '--identity', identity or self.identity(), '--output', out, *args, env=env), out

    def test_fetch_takes_the_newest_complete_copy_and_decrypts_it(self):
        stamps = self.publish(2)
        # Объект новее всех, но без манифеста: загрузка не завершилась, брать его нельзя.
        orphan = self.remote_dir / 'bucket' / 'prefix' / 'frequent' / '20991231T235959Z.dump.age'
        orphan.write_bytes(b'incomplete')
        result, out = self.fetch()
        self.assertEqual(result.returncode, 0, result.stderr)
        summary = json.loads(result.stdout)
        self.assertEqual(summary['manifest']['stamp'], stamps[-1])
        dump = Path(summary['dump'])
        self.assertEqual(dump, out / f'lidradar-{stamps[-1]}.dump')
        self.assertIn(b'PGDMP-plaintext-marker-dump-1', dump.read_bytes())
        self.assertEqual(sorted(p.name for p in out.iterdir()), sorted([f'lidradar-{stamps[-1]}.dump', f'{stamps[-1]}.manifest.json']))

    def test_fetch_by_stamp_and_by_tier(self):
        stamps = self.publish(2)
        result, _ = self.fetch('--stamp', stamps[0])
        self.assertEqual(json.loads(result.stdout)['manifest']['stamp'], stamps[0])
        self.assertIn(b'dump-0', Path(json.loads(result.stdout)['dump']).read_bytes())
        daily, _ = self.fetch('--tier', 'daily')
        self.assertEqual(daily.returncode, 0, daily.stderr)
        self.assertEqual(json.loads(daily.stdout)['manifest']['tier'], 'daily')

    def test_fetch_refuses_a_key_that_is_not_a_recipient(self):
        self.publish()
        result, _ = self.fetch(identity=self.identity('age1' + 'c' * 58))
        self.assertEqual(result.returncode, 1)
        self.assertIn('ключ не подходит', result.stderr)
        # Второй получатель манифеста тоже может расшифровать копию.
        other, _ = self.fetch(identity=self.identity(RECIPIENT_B))
        self.assertEqual(other.returncode, 0, other.stderr)

    def test_fetch_detects_a_tampered_object_or_manifest(self):
        stamps = self.publish()
        directory = self.remote_dir / 'bucket' / 'prefix' / 'frequent'
        dump = directory / f'{stamps[0]}.dump.age'
        original = dump.read_bytes()
        dump.write_bytes(original + b'x')
        result, _ = self.fetch()
        self.assertEqual(result.returncode, 1)
        self.assertIn('контрольная сумма', result.stderr)
        dump.write_bytes(original)
        manifest = directory / f'{stamps[0]}.manifest.json'
        data = json.loads(manifest.read_text())
        data['dump']['plainSha256'] = '0' * 64
        manifest.write_text(json.dumps(data))
        result, _ = self.fetch()
        self.assertEqual(result.returncode, 1)
        self.assertIn('не совпала с суммой из манифеста', result.stderr)
        manifest.write_text('{"format": 2}')
        result, _ = self.fetch()
        self.assertEqual(result.returncode, 1)
        self.assertIn('манифест не распознан', result.stderr)

    def test_fetch_reports_missing_copies_and_bad_arguments(self):
        result, _ = self.fetch()
        self.assertEqual(result.returncode, 1)
        (self.remote_dir / 'bucket' / 'prefix' / 'frequent').mkdir(parents=True)
        result, _ = self.fetch()
        self.assertEqual(result.returncode, 1)
        self.assertIn('нет ни одной полной копии', result.stderr)
        self.publish()
        result, _ = self.fetch('--stamp', '20200101T000000Z')
        self.assertEqual(result.returncode, 1)
        for bad in (['--stamp', '../etc'], ['--tier', 'weekly']):
            result, _ = self.fetch(*bad)
            self.assertEqual(result.returncode, 2, bad)
        absent = self.run_cli('fetch', '--identity', self.root / 'absent.key')
        self.assertEqual(absent.returncode, 2)
        self.assertIn('файл ключа не читается', absent.stderr)
        self.assertEqual(self.run_cli('fetch').returncode, 2)

    def test_list_shows_complete_copies_newest_first(self):
        stamps = self.publish(2)
        (self.remote_dir / 'bucket' / 'prefix' / 'frequent' / '20991231T235959Z.dump.age').write_bytes(b'incomplete')
        result = self.run_cli('list')
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = [line.split('\t')[0] for line in result.stdout.splitlines()[1:]]
        self.assertEqual(rows, list(reversed(stamps)))
        self.assertEqual(len(self.run_cli('list', '--limit', '1').stdout.splitlines()), 2)

    def test_drill_restores_into_a_scratch_database_compares_counters_and_drops_it(self):
        self.publish()
        result = self.run_cli('drill', '--identity', self.identity())
        self.assertEqual(result.returncode, 0, result.stderr)
        verdict = json.loads(result.stdout)
        self.assertTrue(verdict['ok'])
        self.assertEqual(verdict['problems'], [])
        calls = self.call_log()
        scratch = [line for line in calls if 'CREATE DATABASE' in line]
        self.assertEqual(len(scratch), 1)
        name = scratch[0].split('"')[1]
        self.assertTrue(name.startswith('lidradar_drill_'))
        self.assertTrue(any(f'DROP DATABASE IF EXISTS "{name}"' in line for line in calls))
        restore = [line for line in calls if line.startswith('pg_restore --dbname')][0]
        self.assertIn(f'/{name}?sslmode=disable', restore)
        self.assertIn('--exit-on-error', restore)
        self.assertEqual(list(self.spool.glob('drill-*')), [])

    def test_drill_fails_when_restored_counters_differ_from_the_manifest(self):
        self.publish()
        for name, (restored, expected) in {
            'потеряны сообщения': ('49|000023_unfinished_agreements|1|9|2|3|4', 'messages: 9'),
            'лишние сообщения': ('49|000023_unfinished_agreements|1|11|2|3|4', 'messages: 11'),
            'другая миграция': ('49|000022_membership_invitations|1|10|2|3|4', 'миграция'),
            'другое число таблиц': ('48|000023_unfinished_agreements|1|10|2|3|4', 'таблиц 48'),
        }.items():
            with self.subTest(name):
                result = self.run_cli('drill', '--identity', self.identity(), env={'QA_COUNTS_RESTORED': restored})
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                verdict = json.loads(result.stdout)
                self.assertFalse(verdict['ok'])
                self.assertTrue(any(expected in problem for problem in verdict['problems']), verdict['problems'])
                self.assertTrue(any('DROP DATABASE' in line for line in self.call_log()))

    def test_drill_accepts_counters_that_moved_while_the_dump_was_running(self):
        # Запись шла во время выгрузки: счётчик «после» больше, и восстановленное значение лежит между ними.
        self.env['QA_COUNTS_AFTER'] = '49|000023_unfinished_agreements|1|14|2|3|4'
        self.publish()
        result = self.run_cli('drill', '--identity', self.identity(), env={'QA_COUNTS_RESTORED': '49|000023_unfinished_agreements|1|12|2|3|4'})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_drill_drops_the_scratch_database_even_when_the_restore_fails(self):
        self.publish()
        result = self.run_cli('drill', '--identity', self.identity(), env={'QA_RESTORE_FAIL': '1'})
        self.assertEqual(result.returncode, 1)
        self.assertIn('backup.drill_failed', self.events(result)[0])
        self.assertTrue(any('DROP DATABASE IF EXISTS' in line for line in self.call_log()))
        self.assertEqual(list(self.spool.glob('drill-*')), [])

    def test_unknown_commands_print_usage(self):
        self.assertEqual(self.run_cli().returncode, 2)
        self.assertEqual(self.run_cli('frobnicate').returncode, 2)
        self.assertEqual(self.run_cli('run', 'extra').returncode, 2)


if __name__ == '__main__':
    unittest.main()
