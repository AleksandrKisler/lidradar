"""restore-offhost.sh на подставных docker, restore.sh и bootstrap-roles.sh: порядок шагов и защитные проверки."""
from pathlib import Path
import os
import shutil
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'restore-offhost.sh'
STAMP = '20261010T080000Z'

FAKE_DOCKER = r'''#!/usr/bin/env bash
set -u
echo "docker $*" >> "$QA_CALLS"
case "$*" in
  *pg_isready*) [ -z "${QA_PG_DOWN:-}" ] || exit 2; exit 0 ;;
  *"ps --status running --services"*) printf '%s' "${QA_RUNNING:-}"; exit 0 ;;
  *"compose run"*)
    [ -z "${QA_FETCH_FAIL:-}" ] || { echo '{"event":"backup.fetch_failed"}' >&2; exit 1; }
    mount=""
    for arg in "$@"; do case "$arg" in *:/restore) mount="${arg%%:/restore}" ;; esac; done
    echo "restore-dir=$mount" >> "$QA_CALLS"
    [ -n "${QA_NO_DUMP:-}" ] || echo "PLAINTEXT-DUMP-OF-ALL-DATA" > "$mount/lidradar-${QA_STAMP}.dump"
    echo "{\"dump\":\"/restore/lidradar-${QA_STAMP}.dump\",\"manifest\":{\"stamp\":\"${QA_STAMP}\"}}"
    exit 0 ;;
  *psql*) echo "sql" >> "$QA_CALLS"; cat > "$QA_CALLS.sql"; exit 0 ;;
esac
'''

FAKE_RESTORE = r'''#!/usr/bin/env bash
set -eu
echo "restore.sh $*" >> "$QA_CALLS"
grep -q PLAINTEXT-DUMP "$1" || { echo 'дамп отсутствует в момент восстановления' >&2; exit 1; }
'''

FAKE_ROLES = '#!/usr/bin/env bash\nset -eu\necho "bootstrap-roles.sh $*" >> "$QA_CALLS"\n'


class RestoreOffhostTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.scripts = root / 'scripts'
        self.scripts.mkdir()
        shutil.copy(SCRIPT, self.scripts / 'restore-offhost.sh')
        for name, body in {'restore.sh': FAKE_RESTORE, 'bootstrap-roles.sh': FAKE_ROLES}.items():
            (self.scripts / name).write_text(body)
            (self.scripts / name).chmod(0o755)
        self.bin = root / 'bin'
        self.bin.mkdir()
        (self.bin / 'docker').write_text(FAKE_DOCKER)
        (self.bin / 'docker').chmod(0o755)
        self.calls = root / 'calls'
        self.identity = root / 'age.key'
        self.identity.write_text('AGE-SECRET-KEY-test\n')
        self.env = {**os.environ, 'PATH': f'{self.bin}{os.pathsep}{os.environ["PATH"]}', 'QA_CALLS': str(self.calls), 'QA_STAMP': STAMP}
        for name in ('LIDRADAR_RESTORE_S3_ACCESS_KEY_ID', 'LIDRADAR_RESTORE_S3_SECRET_ACCESS_KEY', 'LIDRADAR_BACKUP_DB', 'LIDRADAR_BACKUP_USER'):
            self.env.pop(name, None)

    def run_script(self, *args, env=None):
        return subprocess.run(['bash', str(self.scripts / 'restore-offhost.sh'), *map(str, args)],
                              env={**self.env, **(env or {})}, capture_output=True, text=True)

    def log(self):
        return self.calls.read_text().splitlines() if self.calls.exists() else []

    def test_restore_into_a_new_database_in_order_and_the_live_database_is_untouched(self):
        result = self.run_script('--identity', self.identity)
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.log()
        steps = [line.split()[0] for line in calls if not line.startswith('restore-dir')]
        self.assertEqual(steps, ['docker', 'docker', 'restore.sh', 'bootstrap-roles.sh'])
        fetch = [line for line in calls if 'compose run' in line][0]
        for expected in ('--rm', '-T', '--no-deps', f'--user {os.getuid()}:{os.getgid()}', ':/run/identity:ro', '-e LIDRADAR_BACKUP_SPOOL=/restore/.spool',
                         'backup fetch --identity /run/identity --tier frequent --output /restore'):
            self.assertIn(expected, fetch)
        restore_dir = [line.split('=', 1)[1] for line in calls if line.startswith('restore-dir')][0]
        self.assertTrue(any(line.startswith(f'restore.sh {restore_dir}/lidradar-{STAMP}.dump lidradar_restored') for line in calls))
        self.assertIn('bootstrap-roles.sh lidradar_restored', calls)
        self.assertNotIn('sql', calls)  # без --replace-database базы не переименовываются
        self.assertIn('рабочая база lidradar не тронута', result.stdout)
        self.assertIn('--replace-database', result.stdout)
        self.assertIn(STAMP, result.stdout)
        # Расшифрованный открытый текст не остаётся на диске.
        self.assertFalse(Path(restore_dir).exists())

    def test_readiness_is_checked_over_tcp_so_an_initialising_volume_is_not_taken_for_ready(self):
        # Временный сервер инициализации тома слушает только сокет; готов тот, что принимает TCP.
        result = self.run_script('--identity', self.identity)
        self.assertEqual(result.returncode, 0, result.stderr)
        ready = [line for line in self.log() if 'pg_isready' in line]
        self.assertEqual(len(ready), 1, ready)
        self.assertIn('pg_isready -q -h 127.0.0.1 -U lidradar', ready[0])

    def test_tier_and_stamp_are_passed_to_the_fetch(self):
        result = self.run_script('--identity', self.identity, '--tier', 'daily', '--stamp', STAMP, '--target-db', 'lidradar_drill')
        self.assertEqual(result.returncode, 0, result.stderr)
        fetch = [line for line in self.log() if 'compose run' in line][0]
        self.assertIn(f'--tier daily --output /restore --stamp {STAMP}', fetch)
        self.assertIn('bootstrap-roles.sh lidradar_drill', self.log())

    def test_replace_database_renames_after_the_restore_and_only_with_the_services_stopped(self):
        refused = self.run_script('--identity', self.identity, '--replace-database', env={'QA_RUNNING': 'api\nbackup\npostgres\n'})
        self.assertEqual(refused.returncode, 1)
        self.assertIn('api', refused.stderr)
        self.assertFalse(any('compose run' in line for line in self.log()))  # отказ до скачивания копии
        self.calls.unlink(missing_ok=True)
        result = self.run_script('--identity', self.identity, '--replace-database', env={'QA_RUNNING': 'postgres\nedge\nweb\n'})
        self.assertEqual(result.returncode, 0, result.stderr)
        sql = Path(str(self.calls) + '.sql').read_text().splitlines()
        self.assertEqual(len(sql), 2, sql)
        self.assertRegex(sql[0], r'^ALTER DATABASE "lidradar" RENAME TO "lidradar_replaced_\d{14}";$')
        self.assertEqual(sql[1], 'ALTER DATABASE "lidradar_restored" RENAME TO "lidradar";')
        # Удаления нет, переименование — после восстановления и проверки ролей.
        names = [line.split()[0] for line in self.log()]
        self.assertLess(names.index('bootstrap-roles.sh'), names.index('sql'))

    def test_a_failed_download_restores_nothing_and_leaves_no_plaintext(self):
        result = self.run_script('--identity', self.identity, env={'QA_FETCH_FAIL': '1'})
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(line.startswith(('restore.sh', 'bootstrap-roles.sh', 'sql')) for line in self.log()))

    def test_a_missing_dump_after_the_fetch_is_an_error(self):
        result = self.run_script('--identity', self.identity, env={'QA_NO_DUMP': '1'})
        self.assertEqual(result.returncode, 1)
        self.assertIn('не оставил расшифрованную выгрузку', result.stderr)
        self.assertFalse(any(line.startswith('restore.sh') for line in self.log()))

    def test_postgres_must_be_running_before_anything_else(self):
        result = self.run_script('--identity', self.identity, env={'QA_PG_DOWN': '1'})
        self.assertEqual(result.returncode, 1)
        self.assertIn('docker compose up -d --wait postgres', result.stderr)
        self.assertFalse(any('compose run' in line for line in self.log()))

    def test_restore_credentials_travel_by_name_never_by_value(self):
        env = {'LIDRADAR_RESTORE_S3_ACCESS_KEY_ID': 'restore-key-id', 'LIDRADAR_RESTORE_S3_SECRET_ACCESS_KEY': 'restore-secret-value'}
        result = self.run_script('--identity', self.identity, env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        fetch = [line for line in self.log() if 'compose run' in line][0]
        self.assertIn('-e LIDRADAR_BACKUP_S3_ACCESS_KEY_ID -e LIDRADAR_BACKUP_S3_SECRET_ACCESS_KEY', fetch)
        self.assertNotIn('restore-secret-value', fetch + result.stdout + result.stderr)
        self.assertNotIn('restore-key-id', fetch)
        # Секрет без идентификатора — ошибка настройки, а не молчаливое использование ключа записи.
        broken = self.run_script('--identity', self.identity, env={'LIDRADAR_RESTORE_S3_ACCESS_KEY_ID': 'only-id'})
        self.assertNotEqual(broken.returncode, 0)

    def test_arguments_are_validated_before_any_docker_call(self):
        cases = [
            [], ['--identity'], ['--identity', self.scripts / 'absent.key'],
            ['--identity', self.identity, '--tier', 'weekly'], ['--identity', self.identity, '--stamp', '../x'],
            ['--identity', self.identity, '--target-db', 'bad name'], ['--identity', self.identity, '--target-db', 'lidradar'],
            ['--identity', self.identity, '--target-db', 'postgres'], ['--identity', self.identity, '--bogus'],
        ]
        for args in cases:
            with self.subTest(args=[str(a) for a in args]):
                self.assertEqual(self.run_script(*args).returncode, 2)
        self.assertEqual(self.log(), [])


if __name__ == '__main__':
    unittest.main()
