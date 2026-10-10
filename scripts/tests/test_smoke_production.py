"""Smoke checker tests use a fake curl and a fake docker; nothing leaves the machine."""
from pathlib import Path
import json
import os
import socket
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'smoke-production.sh'

FAKE_CURL = r'''#!/usr/bin/env python3
import os, sys
args = sys.argv[1:]
broken = set(filter(None, os.environ.get("QA_BREAK", "").split(",")))
out = headers = None
method = None
for index, arg in enumerate(args):
    if arg == "-o":
        out = args[index + 1]
    elif arg == "-D":
        headers = args[index + 1]
    elif arg == "-X":
        method = args[index + 1]
url = args[-1]
scheme, rest = url.split("://", 1)
host, _, path = rest.partition("/")
path = "/" + path
method = method or "GET"
page = {"Content-Type": "text/html", "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
        "Content-Security-Policy": "default-src 'self'", "X-Content-Type-Options": "nosniff"}
api = {"Content-Type": "application/json", "Cache-Control": "no-store", "X-Request-Id": "abc",
       "Strict-Transport-Security": "max-age=31536000; includeSubDomains"}

def respond(status, header_map, body, repeat=()):
    lines = ["HTTP/2 %d" % status] + ["%s: %s" % item for item in header_map.items()]
    lines += ["%s: %s" % item for item in repeat]
    if headers:
        open(headers, "w").write("\r\n".join(lines) + "\r\n\r\n")
    if out and out != "/dev/null":
        open(out, "w").write(body)
    print(status, end="")
    sys.exit(0)

if scheme == "http":
    if "no_redirect" in broken:
        respond(200, {}, "")
    respond(308, {"Location": "https://%s%s" % (host, path)}, "")
if path == "/" or path.startswith("/risks/"):
    if "spa_missing" in broken and path != "/":
        respond(404, page, "not found")
    header_map = dict(page)
    if "server_header" in broken:
        header_map["Server"] = "Caddy"
    respond(200, header_map, '<div id="app"></div>')
if path == "/health/live":
    respond(200, api, '{"status":"ok","service":"lidradar-api"}')
if path == "/health/ready":
    if "ready_public" in broken:
        respond(200, api, '{"status":"ready","build":{"revision":"abc"}}')
    respond(404, {"Content-Type": "text/plain"}, "not found")
if "api_down" in broken:
    respond(502, {}, "bad gateway")
repeat = (("Strict-Transport-Security", "max-age=1"),) if "hsts_twice" in broken else ()
if path == "/api/v1/auth/me":
    respond(401, api, '{"error":{"code":"UNAUTHENTICATED","message":"Authentication required"}}', repeat)
if path == "/api/v1/admin/me":
    respond(401, api, '{"error":{"code":"UNAUTHENTICATED"}}')
if path == "/internal/v1/ai/nodes/heartbeat":
    respond(401, api, '{"error":{"code":"UNAUTHORIZED"}}')
if path.startswith("/api/v1/webhooks/"):
    respond(500 if "webhook_500" in broken else 404, api, '{"error":{"code":"NOT_FOUND"}}')
respond(404, {}, "")
'''

ROLES = '2|0|lidradar|0|11'
BACKUP = {'ok': True, 'ageSeconds': 420, 'maxAgeSeconds': 900, 'consecutiveFailures': 0, 'maxGapSeconds': 611, 'lastError': ''}
READY = {'status': 'ready', 'service': 'lidradar-api',
         'build': {'version': 'abc123', 'revision': 'f' * 40, 'modified': False},
         'migrations': {'applied': '000023_unfinished_agreements', 'latest': '000023_unfinished_agreements'}}


class SmokeProductionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.bin = Path(self.temp.name) / 'bin'
        self.bin.mkdir()
        (self.bin / 'curl').write_text(FAKE_CURL)
        (self.bin / 'curl').chmod(0o755)
        self.docker_calls = Path(self.temp.name) / 'docker-calls'
        self.fake_docker(json.dumps(READY))
        self.env = {**os.environ, 'PATH': f'{self.bin}{os.pathsep}{os.environ["PATH"]}', 'QA_DOCKER_CALLS': str(self.docker_calls)}
        self.env.pop('QA_BREAK', None)

    def fake_docker(self, output, status=0, backup=None, backup_status=0, roles=None, roles_status=0):
        # Состояние службы копий читается вызовом exec backup, роли базы — exec postgres; пустая
        # строка вместо ответа имитирует недоступную службу.
        backup_text = '' if backup == '' else json.dumps(BACKUP if backup is None else backup)
        roles_text = ROLES if roles is None else roles
        path = self.bin / 'docker'
        path.write_text(
            "#!/usr/bin/env bash\necho \"docker $*\" >> \"$QA_DOCKER_CALLS\"\n"
            f"case \"$*\" in *' exec -T backup '*) cat <<'JSON'\n{backup_text}\nJSON\nexit {backup_status} ;; esac\n"
            f"case \"$*\" in *' exec -T postgres '*) cat <<'ROLES'\n{roles_text}\nROLES\nexit {roles_status} ;; esac\n"
            f"cat <<'JSON'\n{output}\nJSON\nexit {status}\n")
        path.chmod(0o755)

    def run_script(self, *args, broken=''):
        env = dict(self.env)
        if broken:
            env['QA_BREAK'] = broken
        return subprocess.run(['bash', str(SCRIPT), *args], env=env, capture_output=True, text=True)

    def test_a_healthy_deployment_has_no_remarks(self):
        result = self.run_script('https://app.example.test', '--closed-ports', '8080,5432')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn('FAIL', result.stdout + result.stderr)
        self.assertIn('замечаний нет', result.stdout)
        # 15 проверок страницы, API, живости и перенаправления и 2 закрытых порта:
        # счёт ловит проверку, молча выпавшую из скрипта.
        self.assertEqual(result.stdout.count('ok    '), 17, result.stdout)

    def test_each_defect_is_reported(self):
        cases = {
            'ready_public': '/health/ready виден снаружи',
            'hsts_twice': 'HSTS на ответе API',
            'server_header': 'Server раскрывает ПО',
            'no_redirect': 'HTTP не перенаправляет на HTTPS',
            'api_down': 'GET /api/v1/auth/me через edge',
            'spa_missing': 'глубокая ссылка не открывает веб-клиент',
            'webhook_500': 'вебхук с неизвестным подключением',
        }
        for defect, message in cases.items():
            with self.subTest(defect=defect):
                result = self.run_script('https://app.example.test', '--closed-ports', '', broken=defect)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn(message, result.stderr)
                self.assertIn('замечаний', result.stderr)

    def test_an_open_internal_port_is_a_remark_and_a_closed_one_is_fine(self):
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            listener.listen(1)
            port = listener.getsockname()[1]
            result = self.run_script('https://127.0.0.1', '--closed-ports', str(port))
            self.assertEqual(result.returncode, 1)
            self.assertIn(f'порт {port} на 127.0.0.1 принимает соединения', result.stderr)
        result = self.run_script('https://127.0.0.1', '--closed-ports', str(port))  # слушатель закрыт
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_invalid_arguments_are_refused_before_any_request(self):
        for args in (
            [], ['http://app.example.test'], ['https://app.example.test/path'], ['https://app.example.test', '--bogus'],
            ['https://app.example.test', '--closed-ports', 'a,b'], ['https://app.example.test', 'https://other.example.test'],
            ['https://app.example.test', '--version', 'abc'], ['https://app.example.test', '--compose-dir', 'deploy'],
        ):
            with self.subTest(args=args):
                self.assertEqual(self.run_script(*args).returncode, 2)

    def test_readiness_inside_the_container_is_compared_with_the_expected_release(self):
        base = ['https://app.example.test', '--closed-ports', '', '--version', 'abc123', '--compose-dir', 'deploy/production']
        result = self.run_script(*base)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('api готов изнутри: abc123 000023_unfinished_agreements', result.stdout)
        self.assertIn('exec -T api wget -qO- http://127.0.0.1:8080/health/ready', self.docker_calls.read_text())
        wrong_version = self.run_script(*base[:-4], '--version', 'zzz999', '--compose-dir', 'deploy/production')
        self.assertEqual(wrong_version.returncode, 1)
        self.assertIn("версия сборки 'abc123', ожидалась 'zzz999'", wrong_version.stderr)
        for name, patch in {
            'не готов': {'status': 'starting'},
            'изменённое дерево': {'build': {**READY['build'], 'modified': True}},
            'миграции отстают': {'migrations': {'applied': '000022_membership_invitations', 'latest': '000023_unfinished_agreements'}},
        }.items():
            with self.subTest(name=name):
                self.fake_docker(json.dumps({**READY, **patch}))
                self.assertEqual(self.run_script(*base).returncode, 1)
        self.fake_docker('', status=1)
        broken = self.run_script(*base)
        self.assertEqual(broken.returncode, 1)
        self.assertIn('не удалось прочитать /health/ready изнутри api', broken.stderr)

    def test_database_roles_are_part_of_the_acceptance(self):
        base = ['https://app.example.test', '--closed-ports', '', '--version', 'abc123', '--compose-dir', 'deploy/production']
        healthy = self.run_script(*base)
        self.assertEqual(healthy.returncode, 0, healthy.stdout + healthy.stderr)
        self.assertIn('роли базы: владелец и рабочий логин без суперправ, приложение подключено рабочим логином (11 соединений)', healthy.stdout)
        self.assertIn('exec -T postgres psql', self.docker_calls.read_text())
        cases = {
            'нет ролей': ('0|0|postgres|0|0', 'нет ролей lidradar и lidradar_runtime'),
            'привилегированный логин': ('2|1|lidradar|0|11', 'есть привилегии суперпользователя'),
            'база у суперпользователя': ('2|0|postgres|0|11', "база принадлежит 'postgres', а не lidradar"),
            'подключён суперпользователь': ('2|0|lidradar|2|11', 'к базе подключён суперпользователь (2)'),
            'приложение не под рабочим логином': ('2|0|lidradar|0|0', 'не подключены рабочим логином lidradar_runtime'),
            'несколько нарушений сразу': ('2|1|postgres|1|0', 'привилегии суперпользователя'),
        }
        for name, (roles, message) in cases.items():
            with self.subTest(name):
                self.fake_docker(json.dumps(READY), roles=roles)
                result = self.run_script(*base)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn(message, result.stderr)
        # Внешняя база: сервиса postgres нет, роли проверяются вручную, это не отказ приёмки.
        self.fake_docker(json.dumps(READY), roles='', roles_status=1)
        external = self.run_script(*base)
        self.assertEqual(external.returncode, 0, external.stdout + external.stderr)
        self.assertIn('роли базы не проверены', external.stdout)

    def test_the_age_of_the_off_host_copy_is_part_of_the_acceptance(self):
        base = ['https://app.example.test', '--closed-ports', '', '--version', 'abc123', '--compose-dir', 'deploy/production']
        healthy = self.run_script(*base)
        self.assertEqual(healthy.returncode, 0, healthy.stdout + healthy.stderr)
        self.assertIn('последняя копия вне хоста 7 мин назад, наибольший разрыв между точками 10 мин', healthy.stdout)
        self.assertIn('exec -T backup lidradar-backup status --json', self.docker_calls.read_text())
        cases = {
            'старше допустимого': ({**BACKUP, 'ok': False, 'ageSeconds': 2400, 'lastError': 'upload: 403'}, 'старше допустимого возраста: 40 мин из 15; ошибка: upload: 403'),
            'копий ещё нет': ({'ok': False, 'ageSeconds': None}, 'подтверждённых копий вне хоста ещё нет'),
            'два сбоя подряд': ({**BACKUP, 'consecutiveFailures': 2, 'lastError': 'verify: mismatch'}, 'не создаётся: 2 цикла подряд неудачны; ошибка: verify: mismatch'),
        }
        for name, (backup, message) in cases.items():
            with self.subTest(name):
                self.fake_docker(json.dumps(READY), backup=backup, backup_status=1)  # статус 1 при устаревшей копии — ожидаемое поведение
                result = self.run_script(*base)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn(message, result.stderr)
        self.fake_docker(json.dumps(READY), backup='', backup_status=1)
        absent = self.run_script(*base)
        self.assertEqual(absent.returncode, 1)
        self.assertIn('не удалось прочитать состояние службы копий вне хоста', absent.stderr)


if __name__ == '__main__':
    unittest.main()
