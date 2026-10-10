"""Roles bootstrap tests use fake PostgreSQL tools; they never connect to a database."""
from pathlib import Path
import os
import re
import subprocess
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parents[1]
SQL = SCRIPTS / 'sql'
ADMIN_URL = 'postgres://owner@localhost/postgres'


class BootstrapRolesTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.calls = self.root / 'calls'
        self.stdin = self.root / 'stdin'
        self.env = {**os.environ, 'PATH': str(self.bin) + os.pathsep + os.environ['PATH'],
                    'LIDRADAR_BACKUP_MODE': 'local', 'LIDRADAR_ADMIN_DATABASE_URL': ADMIN_URL,
                    'QA_CALLS': str(self.calls), 'QA_STDIN': str(self.stdin)}
        for key in ('LIDRADAR_APP_DB_USER', 'LIDRADAR_BACKUP_USER', 'LIDRADAR_BACKUP_CONTAINER'):
            self.env.pop(key, None)
        # Подставной psql записывает аргументы и весь SQL со стандартного ввода.
        self.psql('echo "psql $*" >> "$QA_CALLS"; cat >> "$QA_STDIN"; echo "=====" >> "$QA_STDIN"')

    def psql(self, body):
        path = self.bin / 'psql'
        path.write_text('#!/usr/bin/env bash\nset -eu\n' + body + '\n')
        path.chmod(0o755)

    def run_script(self, *args):
        return subprocess.run(['bash', str(SCRIPTS / 'bootstrap-roles.sh'), *map(str, args)],
                              env=self.env, capture_output=True, text=True)

    def log(self):
        return self.calls.read_text().splitlines() if self.calls.exists() else []

    def sql_sent(self):
        return self.stdin.read_text() if self.stdin.exists() else ''

    def test_applies_then_verifies_against_the_named_database(self):
        result = self.run_script('restored_db')
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.log()
        self.assertEqual(len(calls), 2, calls)
        self.assertIn(' -1 ', calls[0] + ' ')  # одна транзакция
        self.assertIn(' -At ', calls[1] + ' ')  # вывод только строк проблем
        for call in calls:
            self.assertIn('-v ON_ERROR_STOP=1', call)
            self.assertTrue(call.endswith('postgres://owner@localhost/restored_db'), call)
        applied, verified = self.sql_sent().split('=====\n')[:2]
        self.assertIn('CREATE ROLE', applied)
        self.assertIn('lidradar_platform', applied)
        self.assertIn('tenant_isolation', verified)
        self.assertNotIn('CREATE ROLE', verified)
        self.assertIn('verified', result.stdout)

    def test_default_database_name_matches_restore_helper(self):
        self.assertEqual(self.run_script().returncode, 0)
        self.assertTrue(self.log()[0].endswith('/lidradar_restore'), self.log())

    def test_verify_only_sends_nothing_but_the_read_only_check(self):
        result = self.run_script('--verify-only', 'restored_db')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.log()), 1)
        self.assertNotIn(' -1 ', self.log()[0] + ' ')
        expected = 'SET client_min_messages = warning;\n' + (SQL / 'verify-roles.sql').read_text(encoding='utf-8') + '=====\n'
        self.assertEqual(self.sql_sent(), expected)

    def test_problems_found_by_verification_fail_the_script(self):
        self.psql('echo "psql $*" >> "$QA_CALLS"; cat > /dev/null\n'
                  'case " $* " in *" -At "*) echo "у роли lidradar_app нет SELECT на таблицу users" ;; esac')
        result = self.run_script('restored_db')
        self.assertEqual(result.returncode, 1)
        self.assertIn('FAILED', result.stderr)
        self.assertIn('lidradar_app', result.stderr)
        self.assertEqual(len(self.log()), 2)

    def test_failed_bootstrap_stops_before_verification(self):
        self.psql('echo "psql $*" >> "$QA_CALLS"; cat > /dev/null; exit 3')
        result = self.run_script('restored_db')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.log()), 1)
        self.assertNotIn('verified', result.stdout)

    def test_invalid_inputs_never_reach_psql(self):
        bad_calls = [
            ('qa"; DROP DATABASE prod;--',), ('postgres',), ('template1',), ('a', 'b'), ('--verify-only', 'a', 'b'),
        ]
        for args in bad_calls:
            with self.subTest(args=args):
                self.assertNotEqual(self.run_script(*args).returncode, 0)
                self.assertEqual(self.log(), [])
        for user in ("x'; DROP ROLE y;--", 'two words', '1abc'):
            with self.subTest(app_user=user):
                self.env['LIDRADAR_APP_DB_USER'] = user
                self.assertNotEqual(self.run_script('qa').returncode, 0)
                self.assertEqual(self.log(), [])
        self.env.pop('LIDRADAR_APP_DB_USER')
        self.env['LIDRADAR_ADMIN_DATABASE_URL'] = ADMIN_URL + '?dbname=production'
        self.assertNotEqual(self.run_script('qa').returncode, 0)
        self.env['LIDRADAR_ADMIN_DATABASE_URL'] = 'mysql://owner@localhost/x'
        self.assertNotEqual(self.run_script('qa').returncode, 0)
        del self.env['LIDRADAR_ADMIN_DATABASE_URL']
        self.assertNotEqual(self.run_script('qa').returncode, 0)
        self.env['LIDRADAR_BACKUP_MODE'] = 'tape'
        self.assertNotEqual(self.run_script('qa').returncode, 0)
        self.assertEqual(self.log(), [])

    def test_application_user_is_passed_as_session_setting(self):
        self.env['LIDRADAR_APP_DB_USER'] = 'lidradar_rt'
        self.assertEqual(self.run_script('qa').returncode, 0)
        applied, verified = self.sql_sent().split('=====\n')[:2]
        for sql in (applied, verified):
            self.assertIn("SET lidradar.app_user = 'lidradar_rt';", sql)
            self.assertLess(sql.index('SET lidradar.app_user'), sql.index('lidradar_platform'))
        self.stdin.unlink()
        del self.env['LIDRADAR_APP_DB_USER']
        self.assertEqual(self.run_script('qa').returncode, 0)
        self.assertNotIn('SET lidradar.app_user', self.sql_sent())

    def test_missing_sql_next_to_script_fails_before_psql(self):
        copy = self.root / 'copy'
        copy.mkdir()
        (copy / 'bootstrap-roles.sh').write_bytes((SCRIPTS / 'bootstrap-roles.sh').read_bytes())
        result = subprocess.run(['bash', str(copy / 'bootstrap-roles.sh'), 'qa'], env=self.env, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.log(), [])

    def test_sql_never_revokes_drops_or_weakens_row_level_security(self):
        def statements(name):
            return re.sub(r'--[^\n]*', '', (SQL / name).read_text(encoding='utf-8'))

        forbidden = [r'\bDROP\b', r'\bREVOKE\b', r'\bTRUNCATE\b', r'\bDELETE\s+FROM\b', r'\bNO\s+FORCE\b',
                     r'\bDISABLE\s+ROW\s+LEVEL\b', r'\bBYPASSRLS\b', r'\bSUPERUSER\b', r'\bALTER\s+TABLE\b',
                     r'\bALTER\s+ROLE\b', r'\bCREATE\s+POLICY\b', r'\bALTER\s+POLICY\b', r'\bSET\s+ROLE\b']
        for name in ('bootstrap-roles.sql', 'verify-roles.sql'):
            for pattern in forbidden:
                self.assertIsNone(re.search(pattern, statements(name), re.I), f'{name}: {pattern}')
        # Проверка только читает: ни одного изменяющего оператора в начале строки.
        self.assertIsNone(re.search(r'(?im)^\s*(INSERT|UPDATE|DELETE|ALTER|CREATE|GRANT|DROP|TRUNCATE|SET)\b',
                                    statements('verify-roles.sql')))
        # Роли создаются без права входа.
        self.assertIn("'CREATE ROLE %I NOLOGIN'", statements('bootstrap-roles.sql'))


if __name__ == '__main__':
    unittest.main()
