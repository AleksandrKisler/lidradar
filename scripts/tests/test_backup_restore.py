"""Safety tests use fake PostgreSQL tools; never connect to a database."""
from pathlib import Path
import os
import subprocess
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parents[1]


class RecoveryHelpersTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.calls = self.root / 'calls'
        self.backups = self.root / 'backups'
        self.dump = self.root / 'input.dump'
        self.dump.write_bytes(b'fixture')
        self.env = {**os.environ, 'PATH': str(self.bin) + os.pathsep + os.environ['PATH'],
                    'LIDRADAR_BACKUP_MODE': 'local', 'LIDRADAR_BACKUP_DIR': str(self.backups),
                    'LIDRADAR_BACKUP_KEEP': '2', 'LIDRADAR_DATABASE_URL': 'postgres://test@localhost/source',
                    'LIDRADAR_ADMIN_DATABASE_URL': 'postgres://test@localhost/postgres',
                    'QA_CALLS': str(self.calls)}
        self.env.pop('LIDRADAR_RESTORE_DATABASE_URL', None)
        self.tool('pg_dump', 'head -c 2048 /dev/zero')
        self.tool('pg_restore', 'echo "restore $*" >> "$QA_CALLS"')
        self.tool('psql', 'echo "psql $*" >> "$QA_CALLS"')

    def tool(self, name, body):
        path = self.bin / name
        path.write_text('#!/usr/bin/env bash\nset -eu\n' + body + '\n')
        path.chmod(0o755)

    def run_script(self, name, *args):
        return subprocess.run(['bash', str(SCRIPTS / name), *map(str, args)],
                              env=self.env, capture_output=True, text=True)

    def log(self):
        return self.calls.read_text() if self.calls.exists() else ''

    def test_failed_dump_never_published_or_rotates_good_copy(self):
        self.backups.mkdir()
        old = self.backups / 'lidradar-old.dump'
        old.write_bytes(b'old')
        self.tool('pg_dump', 'head -c 2048 /dev/zero; exit 1')
        result = self.run_script('backup.sh')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(list(self.backups.iterdir()), [old])
        self.assertEqual(old.read_bytes(), b'old')

    def test_complete_dumps_private_unique_and_rotation_after_success(self):
        for _ in range(3):
            result = self.run_script('backup.sh')
            self.assertEqual(result.returncode, 0, result.stderr)
        dumps = list(self.backups.glob('*.dump'))
        self.assertEqual(len(dumps), 2)
        for dump in dumps:
            self.assertEqual(dump.stat().st_size, 2048)
            self.assertEqual(dump.stat().st_mode & 0o777, 0o600)
        self.assertEqual(len(list(self.backups.iterdir())), 2)

    def test_bad_keep_fails_before_tools_or_files(self):
        self.env['LIDRADAR_BACKUP_KEEP'] = '0'
        self.assertNotEqual(self.run_script('backup.sh').returncode, 0)
        self.assertFalse(self.backups.exists())

    def test_invalid_restore_inputs_never_create_database(self):
        for dump, target in [(self.root / 'missing', 'qa'), (self.dump, 'qa"; DROP DATABASE prod;--'),
                             (self.dump, 'postgres')]:
            with self.subTest(target=target):
                self.assertNotEqual(self.run_script('restore.sh', dump, target).returncode, 0)
                self.assertEqual(self.log(), '')
        self.tool('pg_restore', 'exit 1')
        self.assertNotEqual(self.run_script('restore.sh', self.dump, 'qa').returncode, 0)
        self.assertEqual(self.log(), '')

    def test_restore_validates_then_creates_without_drop(self):
        result = self.run_script('restore.sh', self.dump, 'qa_restore')
        self.assertEqual(result.returncode, 0, result.stderr)
        lines = self.log().splitlines()
        self.assertIn('--list', lines[0])
        self.assertIn('CREATE DATABASE "qa_restore"', lines[1])
        self.assertIn('--dbname=postgres://test@localhost/qa_restore', lines[2])
        self.assertNotIn('DROP', self.log())

    def test_existing_database_is_not_overwritten(self):
        self.tool('psql', 'echo "psql $*" >> "$QA_CALLS"; exit 1')
        self.assertNotEqual(self.run_script('restore.sh', self.dump, 'exists').returncode, 0)
        self.assertNotIn('--dbname=', self.log())
        self.assertNotIn('DROP', self.log())

    def test_disagreeing_restore_url_fails_before_database_tools(self):
        self.env['LIDRADAR_RESTORE_DATABASE_URL'] = 'postgres://test@localhost/production'
        self.assertNotEqual(self.run_script('restore.sh', self.dump, 'qa').returncode, 0)
        self.assertEqual(self.log(), '')

    def test_query_override_cannot_redirect_restore(self):
        self.env['LIDRADAR_ADMIN_DATABASE_URL'] += '?dbname=production'
        self.assertNotEqual(self.run_script('restore.sh', self.dump, 'qa').returncode, 0)
        self.assertEqual(self.log(), '')


if __name__ == '__main__':
    unittest.main()
