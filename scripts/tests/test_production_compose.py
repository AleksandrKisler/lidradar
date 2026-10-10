"""Static invariants of the production topology (ADR 0050).

The model is rendered by `docker compose config`, so the checks see exactly what
would run. Nothing is started and no image is pulled.
"""
from pathlib import Path
import json
import os
import re
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

DEPLOY = Path(__file__).resolve().parents[2] / 'deploy' / 'production'
COMPOSE = DEPLOY / 'compose.yaml'
CADDYFILE = DEPLOY / 'Caddyfile'
ENV_EXAMPLE = DEPLOY / '.env.example'

COMPLETE_ENV = """\
LIDRADAR_VERSION=ci
LIDRADAR_DOMAIN=app.example.test
LIDRADAR_ACME_EMAIL=ops@example.test
POSTGRES_PASSWORD=0123456789abcdef0123456789abcdef0123456789abcdef
LIDRADAR_DB_OWNER_PASSWORD=fedcba9876543210fedcba9876543210fedcba9876543210
LIDRADAR_DB_RUNTIME_PASSWORD=a1b2c3d4e5f60718a1b2c3d4e5f60718a1b2c3d4e5f60718
LIDRADAR_INTEGRATION_ENCRYPTION_KEY=MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=
LIDRADAR_TELEGRAM_TOKEN=123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
LIDRADAR_TELEGRAM_BOT_USERNAME=LidRadarProdBot
LIDRADAR_BACKUP_REMOTE=offhost:bucket/prefix
LIDRADAR_BACKUP_AGE_RECIPIENTS=age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq
"""
ADMIN_PASSWORD = '0123456789abcdef0123456789abcdef0123456789abcdef'
OWNER_PASSWORD = 'fedcba9876543210fedcba9876543210fedcba9876543210'
RUNTIME_PASSWORD = 'a1b2c3d4e5f60718a1b2c3d4e5f60718a1b2c3d4e5f60718'
POSTGRES_DIR = DEPLOY / 'postgres'
GO_SERVICES = ['api', 'worker', 'scheduler', 'migrate']
TOOLS = ['platform-admin', 'ai-node-register', 'ai-node-manage']


def compose_available():
    if not shutil.which('docker'):
        return False
    return subprocess.run(['docker', 'compose', 'version'], capture_output=True).returncode == 0


def clean_environment():
    # Переменные процесса сильнее файла .env: окружение job в CI (LIDRADAR_ENV, LIDRADAR_DATABASE_URL)
    # или оболочки оператора подменило бы значения, которые проверяют тесты.
    return {name: value for name, value in os.environ.items() if not name.startswith(('LIDRADAR_', 'POSTGRES_', 'COMPOSE_'))}


def render(env_text, *flags):
    with tempfile.TemporaryDirectory() as directory:
        env_file = Path(directory) / 'test.env'
        env_file.write_text(env_text)
        result = subprocess.run(
            ['docker', 'compose', '--env-file', str(env_file), '-f', str(COMPOSE), *flags, 'config', '--format', 'json'],
            capture_output=True, text=True, env={**clean_environment(), 'COMPOSE_PROJECT_NAME': 'lidradar-test'})
    return result


@unittest.skipUnless(compose_available(), 'docker compose is not available')
class ProductionComposeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        result = render(COMPLETE_ENV)
        assert result.returncode == 0, result.stderr
        cls.model = json.loads(result.stdout)
        cls.services = cls.model['services']
        tools = render(COMPLETE_ENV, '--profile', 'tools')
        assert tools.returncode == 0, tools.stderr
        cls.with_tools = json.loads(tools.stdout)['services']

    def test_only_the_edge_publishes_ports_and_only_http_and_https(self):
        published = {name: service.get('ports', []) for name, service in self.with_tools.items() if service.get('ports')}
        self.assertEqual(list(published), ['edge'])
        self.assertEqual(sorted(int(port['target']) for port in published['edge']), [80, 443])
        self.assertTrue(all(port['protocol'] == 'tcp' for port in published['edge']))

    def test_network_segmentation(self):
        networks = {name: sorted(service.get('networks', {})) for name, service in self.with_tools.items()}
        self.assertEqual(networks['edge'], ['edge'])
        self.assertEqual(networks['web'], ['edge'])
        self.assertEqual(networks['api'], ['data', 'edge'])
        self.assertEqual(networks['worker'], ['data', 'egress'])
        self.assertEqual(networks['backup'], ['data', 'egress'])  # база и хранилище, без входящего пути
        for name in ['scheduler', 'migrate', 'postgres', *TOOLS]:
            self.assertEqual(networks[name], ['data'], name)
        self.assertTrue(self.model['networks']['data'].get('internal'), 'сеть данных должна быть внутренней')
        self.assertFalse(self.model['networks']['edge'].get('internal'))

    def test_api_trusts_exactly_the_pinned_edge_address(self):
        edge_ip = self.services['edge']['networks']['edge']['ipv4_address']
        subnet = self.model['networks']['edge']['ipam']['config'][0]['subnet']
        self.assertEqual(self.services['api']['environment']['LIDRADAR_TRUSTED_PROXIES'], f'{edge_ip}/32')
        prefix = subnet.split('/')[0].rsplit('.', 1)[0]
        self.assertTrue(edge_ip.startswith(prefix + '.'), f'{edge_ip} вне подсети {subnet}')
        self.assertNotEqual(edge_ip.rsplit('.', 1)[1], '1', 'адрес шлюза сети занят Docker')
        # Остальные контейнеры получают адреса из ip_range; закреплённый адрес edge
        # в него не входит, иначе Docker мог бы отдать его, например, веб-клиенту.
        dynamic = self.model['networks']['edge']['ipam']['config'][0]['ip_range']
        import ipaddress
        self.assertTrue(ipaddress.ip_network(dynamic).subnet_of(ipaddress.ip_network(subnet)), dynamic)
        self.assertNotIn(ipaddress.ip_address(edge_ip), ipaddress.ip_network(dynamic))

    def test_images_are_immutable_and_pinned(self):
        for name, service in self.with_tools.items():
            self.assertNotIn('build', service, f'{name}: в production нет сборки на месте')
            image = service['image']
            self.assertNotIn(':latest', image, name)
            self.assertRegex(image, r':[^:/]+$', f'{name}: у образа нет тега')
            if image.startswith('lidradar-'):
                self.assertTrue(image.endswith(':ci'), f'{name}: образ выпуска должен брать LIDRADAR_VERSION, а не {image}')
        self.assertTrue(self.services['edge']['image'].startswith('caddy:2.'))
        self.assertTrue(self.services['postgres']['image'].startswith('postgres:18'))

    def test_every_service_is_hardened_and_bounded(self):
        for name, service in self.with_tools.items():
            self.assertIn('no-new-privileges:true', service['security_opt'], name)
            self.assertEqual(service['logging']['driver'], 'json-file', name)
            self.assertIn('max-size', service['logging']['options'], name)
            self.assertIn('max-file', service['logging']['options'], name)
            self.assertTrue(service.get('mem_limit'), f'{name}: нет предела памяти')
            if name in TOOLS or name == 'migrate':
                self.assertEqual(service['restart'], 'no', name)
            else:
                self.assertEqual(service['restart'], 'unless-stopped', name)
        for name in [*GO_SERVICES, *TOOLS, 'web', 'edge', 'backup']:
            service = self.with_tools[name]
            self.assertTrue(service.get('read_only'), f'{name}: корневая ФС должна быть только для чтения')
            self.assertEqual(service['cap_drop'], ['ALL'], name)
        self.assertEqual(self.services['edge']['cap_add'], ['NET_BIND_SERVICE'])

    def test_secrets_reach_only_the_services_that_need_them(self):
        def keys(name):
            return set(self.services[name].get('environment', {}))
        token, key = 'LIDRADAR_TELEGRAM_TOKEN', 'LIDRADAR_INTEGRATION_ENCRYPTION_KEY'
        self.assertIn(token, keys('worker'))
        self.assertNotIn(token, keys('api'))  # токен бота уведомлений API не читает
        self.assertIn(key, keys('api'))
        for name in ['worker', 'scheduler', 'migrate', 'web', 'edge']:
            self.assertNotIn(key, keys(name), name)
        self.assertEqual(keys('web'), set())
        self.assertEqual(keys('edge'), {'LIDRADAR_DOMAIN', 'LIDRADAR_ACME_EMAIL'})
        # Пароли базы (ADR 0052): администратор — только базе, владелец — базе и migrate, рабочий
        # логин — базе, процессам приложения, копиям и инструментам; не больше, чем нужно каждому.
        def holders(secret):
            return {name for name, service in self.with_tools.items() if secret in json.dumps(service.get('environment', {}))}
        self.assertEqual(holders(ADMIN_PASSWORD), {'postgres'})
        self.assertEqual(holders(OWNER_PASSWORD), {'postgres', 'migrate'})
        self.assertEqual(holders(RUNTIME_PASSWORD), {'postgres', 'api', 'worker', 'scheduler', 'backup', *TOOLS})
        for name, service in self.with_tools.items():
            if name != 'postgres':
                self.assertFalse({k for k in service.get('environment', {}) if k in ('POSTGRES_PASSWORD', 'POSTGRES_USER', 'LIDRADAR_DB_OWNER_PASSWORD', 'LIDRADAR_DB_RUNTIME_PASSWORD')}, name)
        # Копии: у службы нет ни токена бота, ни ключа шифрования приложения, а ключ хранилища
        # не попадает больше никуда.
        for name in ('LIDRADAR_TELEGRAM_TOKEN', key):
            self.assertNotIn(name, keys('backup'))
        self.assertIn('LIDRADAR_BACKUP_S3_SECRET_ACCESS_KEY', keys('backup'))
        for name in self.services:
            if name != 'backup':
                self.assertFalse({k for k in keys(name) if k.startswith('LIDRADAR_BACKUP_')}, name)

    def test_no_process_logs_in_to_the_database_as_a_superuser_or_as_the_owner_except_the_migrator(self):
        def user(name):
            url = self.with_tools[name]['environment']['LIDRADAR_DATABASE_URL']
            return re.match(r'postgres://([^:]+):', url).group(1)
        self.assertEqual(user('migrate'), 'lidradar')
        for name in ['api', 'worker', 'scheduler', *TOOLS]:
            self.assertEqual(user(name), 'lidradar_runtime', name)
        backup = self.services['backup']['environment']['LIDRADAR_BACKUP_DATABASE_URL']
        self.assertTrue(backup.startswith('postgres://lidradar_runtime:'), backup)
        self.assertNotIn('postgres://postgres:', json.dumps(self.with_tools))

    def test_postgres_takes_its_roles_and_network_rules_from_the_files(self):
        service = self.services['postgres']
        self.assertEqual(service['command'], ['postgres', '-c', 'hba_file=/etc/lidradar/pg_hba.conf'])
        self.assertEqual(service['environment']['POSTGRES_DB'], 'lidradar')
        self.assertNotIn('POSTGRES_USER', service['environment'])  # администратор — postgres, как в образе
        mounts = {v['target']: v for v in service['volumes']}
        self.assertEqual(Path(mounts['/etc/lidradar/pg_hba.conf']['source']), POSTGRES_DIR / 'pg_hba.conf')
        self.assertEqual(Path(mounts['/docker-entrypoint-initdb.d/10-roles.sql']['source']), POSTGRES_DIR / '10-roles.sql')
        for target in ('/etc/lidradar/pg_hba.conf', '/docker-entrypoint-initdb.d/10-roles.sql'):
            self.assertTrue(mounts[target].get('read_only'), target)
        self.assertNotIn('ports', service)

    def test_postgres_is_healthy_only_after_the_volume_initialisation_is_over(self):
        # Временный сервер инициализации слушает только сокет и принимает pg_isready до конца
        # 10-roles.sql: готовность проверяется по TCP, и логином, которого касается hba (без записи
        # об отказе в журнале на каждую проверку).
        command = ' '.join(self.services['postgres']['healthcheck']['test'])
        self.assertIn('pg_isready -h 127.0.0.1 -U lidradar', command)
        self.assertNotIn('-U postgres', command)

    def test_the_model_does_not_depend_on_the_callers_environment(self):
        # В CI у job заданы LIDRADAR_ENV=test и LIDRADAR_DATABASE_URL: переменные процесса сильнее .env.
        ambient = {
            'LIDRADAR_ENV': 'test',
            'LIDRADAR_DATABASE_URL': 'postgres://lidradar:lidradar@127.0.0.1:5432/lidradar_frontend?sslmode=disable',
            'LIDRADAR_DB_OWNER_PASSWORD': 'from-the-callers-shell',
            'POSTGRES_PASSWORD': 'from-the-callers-shell',
            'COMPOSE_PROJECT_NAME': 'elsewhere',
        }
        with mock.patch.dict(os.environ, ambient):
            rendered = render(COMPLETE_ENV)
        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        self.assertEqual(json.loads(rendered.stdout), self.model)
        self.assertNotIn('from-the-callers-shell', rendered.stdout)

    def test_database_transport_is_an_explicit_decision(self):
        # Без явного LIDRADAR_DATABASE_ALLOW_PLAINTEXT открытый канал запрещён.
        silent = render(COMPLETE_ENV)
        self.assertEqual(json.loads(silent.stdout)['services']['api']['environment']['LIDRADAR_DATABASE_ALLOW_PLAINTEXT'], 'false')
        opted_in = json.loads(render(COMPLETE_ENV + 'LIDRADAR_DATABASE_ALLOW_PLAINTEXT=true\n').stdout)
        self.assertEqual(opted_in['services']['api']['environment']['LIDRADAR_DATABASE_ALLOW_PLAINTEXT'], 'true')

    def test_public_origin_and_urls_follow_the_domain(self):
        environment = self.services['api']['environment']
        self.assertEqual(environment['LIDRADAR_PUBLIC_BASE_URL'], 'https://app.example.test')
        self.assertEqual(environment['LIDRADAR_ALLOWED_ORIGINS'], 'https://app.example.test')
        self.assertEqual(environment['LIDRADAR_ENV'], 'production')
        custom = json.loads(render(COMPLETE_ENV + 'LIDRADAR_PUBLIC_ORIGIN=https://app.example.test:8443\n').stdout)
        self.assertEqual(custom['services']['api']['environment']['LIDRADAR_PUBLIC_BASE_URL'], 'https://app.example.test:8443')

    def test_tools_exist_only_under_their_profile(self):
        for name in TOOLS:
            self.assertNotIn(name, self.services, name)
            self.assertIn(name, self.with_tools, name)

    def test_required_settings_fail_with_a_readable_message(self):
        required = {
            'LIDRADAR_VERSION': 'LIDRADAR_VERSION', 'LIDRADAR_DOMAIN': 'LIDRADAR_DOMAIN',
            'LIDRADAR_ACME_EMAIL': 'LIDRADAR_ACME_EMAIL', 'POSTGRES_PASSWORD': 'POSTGRES_PASSWORD',
            'LIDRADAR_INTEGRATION_ENCRYPTION_KEY': 'LIDRADAR_INTEGRATION_ENCRYPTION_KEY',
            'LIDRADAR_TELEGRAM_TOKEN': 'LIDRADAR_TELEGRAM_TOKEN', 'LIDRADAR_TELEGRAM_BOT_USERNAME': 'LIDRADAR_TELEGRAM_BOT_USERNAME',
            'LIDRADAR_BACKUP_REMOTE': 'LIDRADAR_BACKUP_REMOTE', 'LIDRADAR_BACKUP_AGE_RECIPIENTS': 'LIDRADAR_BACKUP_AGE_RECIPIENTS',
            'LIDRADAR_DB_OWNER_PASSWORD': 'LIDRADAR_DB_OWNER_PASSWORD', 'LIDRADAR_DB_RUNTIME_PASSWORD': 'LIDRADAR_DB_RUNTIME_PASSWORD',
        }
        for variable in required:
            with self.subTest(variable=variable):
                lines = [line for line in COMPLETE_ENV.splitlines() if not line.startswith(variable + '=')]
                result = render('\n'.join(lines) + '\n')
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(variable, result.stderr)

    def test_env_template_matches_the_variables_the_stack_requires(self):
        compose_text = COMPOSE.read_text(encoding='utf-8')
        caddy_text = CADDYFILE.read_text(encoding='utf-8')
        template = ENV_EXAMPLE.read_text(encoding='utf-8')
        assigned = set(re.findall(r'^([A-Z][A-Z0-9_]*)=', template, re.M))
        commented = set(re.findall(r'^# ([A-Z][A-Z0-9_]*)=', template, re.M))
        required = set(re.findall(r'\$\{([A-Z][A-Z0-9_]*):\?', compose_text))
        self.assertTrue(required)
        self.assertLessEqual(required, assigned | commented, 'в .env.example нет обязательных переменных')
        for variable in required - {'LIDRADAR_DATABASE_URL'}:
            self.assertIn(variable, assigned, f'{variable} обязателен и должен быть в шаблоне раскомментированным')
        used = set(re.findall(r'\$\{([A-Z][A-Z0-9_]*)[:}]', compose_text)) | set(re.findall(r'\{\$([A-Z][A-Z0-9_]*)', caddy_text))
        for variable in (assigned | commented) - used:
            self.fail(f'{variable} есть в .env.example, но не используется ни compose, ни Caddyfile')

    def test_no_secret_values_are_written_in_the_files(self):
        text = '\n'.join(path.read_text(encoding='utf-8') for path in (COMPOSE, CADDYFILE, ENV_EXAMPLE))
        self.assertIsNone(re.search(r'\d{6,12}:[A-Za-z0-9_-]{30,}', text), 'похоже на токен бота')
        for variable in ('POSTGRES_PASSWORD', 'LIDRADAR_DB_OWNER_PASSWORD', 'LIDRADAR_DB_RUNTIME_PASSWORD',
                         'LIDRADAR_INTEGRATION_ENCRYPTION_KEY', 'LIDRADAR_TELEGRAM_TOKEN',
                         'LIDRADAR_BACKUP_S3_ACCESS_KEY_ID', 'LIDRADAR_BACKUP_S3_SECRET_ACCESS_KEY'):
            self.assertRegex(ENV_EXAMPLE.read_text(encoding='utf-8'), re.compile(rf'^{variable}=$', re.M), msg=f'{variable} в шаблоне должен быть пустым')
        # Приватный ключ age не должен попадать ни в стек, ни в шаблон: его хранит только оператор.
        self.assertNotIn('AGE-SECRET-KEY-', text)

    def test_the_backup_service_has_only_its_own_spool_and_no_host_access(self):
        service = self.services['backup']
        self.assertEqual([(v['type'], v['source'], v['target']) for v in service['volumes']], [('volume', 'backup-spool', '/spool')])
        self.assertTrue(service['init'])
        self.assertIn('lidradar-backup', service['healthcheck']['test'])
        self.assertNotIn('ports', service)
        self.assertNotIn('docker.sock', json.dumps(service))

    def test_the_backup_database_follows_the_application_unless_overridden(self):
        default = self.services['backup']['environment']['LIDRADAR_BACKUP_DATABASE_URL']
        self.assertTrue(default.startswith('postgres://lidradar_runtime:'))
        self.assertTrue(default.endswith('@postgres:5432/lidradar?sslmode=disable'))
        external = 'postgres://owner:secret@db.example.test:5432/lidradar?sslmode=verify-full'
        followed = json.loads(render(COMPLETE_ENV + f'LIDRADAR_DATABASE_URL={external}\n').stdout)
        self.assertEqual(followed['services']['backup']['environment']['LIDRADAR_BACKUP_DATABASE_URL'], external)
        own = 'postgres://backup:secret@db.example.test:5432/lidradar?sslmode=verify-full'
        override = json.loads(render(COMPLETE_ENV + f'LIDRADAR_DATABASE_URL={external}\nLIDRADAR_BACKUP_DATABASE_URL={own}\n').stdout)
        self.assertEqual(override['services']['backup']['environment']['LIDRADAR_BACKUP_DATABASE_URL'], own)

    def test_the_backup_schedule_matches_the_recovery_point_target(self):
        environment = self.services['backup']['environment']
        interval = {'10m': 600}[environment['LIDRADAR_BACKUP_INTERVAL']]
        max_age = {'15m': 900}[environment['LIDRADAR_BACKUP_MAX_AGE']]
        # RPO 15 минут (RG-DR): интервал плюс время копирования обязан укладываться в допустимый возраст.
        self.assertLess(interval, max_age)
        self.assertLessEqual(max_age, 900)


class DatabaseFilesTests(unittest.TestCase):
    """Инварианты файлов ролей и сетевого доступа PostgreSQL (ADR 0052); поведение проверяет e2e-db-roles.sh."""

    def setUp(self):
        self.hba = [line.split() for line in (POSTGRES_DIR / 'pg_hba.conf').read_text(encoding='utf-8').splitlines()
                    if line.strip() and not line.lstrip().startswith('#')]
        self.sql = (POSTGRES_DIR / '10-roles.sql').read_text(encoding='utf-8')
        self.code = re.sub(r'--.*', '', self.sql)

    def test_the_administrator_cannot_log_in_over_the_network_and_nobody_gets_trust_over_tcp(self):
        for rule in self.hba:
            if rule[0] == 'local':
                continue
            self.assertEqual(rule[0], 'host', rule)
            self.assertNotIn('trust', rule, rule)
            self.assertNotIn('postgres', rule[2].split(','), rule)
        users = [rule[2] for rule in self.hba if rule[0] == 'host' and rule[-1] == 'scram-sha-256']
        self.assertEqual(users, ['lidradar,lidradar_runtime'])
        self.assertEqual(self.hba[-1], ['host', 'all', 'all', 'all', 'reject'])  # всё остальное закрыто последним правилом
        self.assertEqual([rule for rule in self.hba if rule[0] == 'local'], [['local', 'all', 'all', 'trust']])

    def role_definition(self, name):
        match = re.search(rf'ALTER ROLE {name} WITH ([^;]+);', self.code)
        self.assertIsNotNone(match, name)
        return match.group(1).split()

    def test_the_owner_and_the_runtime_login_are_not_privileged(self):
        owner, runtime = self.role_definition('lidradar'), self.role_definition('lidradar_runtime')
        for attributes in (owner, runtime):
            for forbidden in ('SUPERUSER', 'CREATEROLE', 'BYPASSRLS', 'REPLICATION'):
                self.assertNotIn(forbidden, attributes)  # без приставки NO атрибут бы включался
            for required in ('NOSUPERUSER', 'NOCREATEROLE', 'NOBYPASSRLS', 'NOREPLICATION', 'LOGIN'):
                self.assertIn(required, attributes)
        self.assertIn('CREATEDB', owner)
        self.assertIn('NOCREATEDB', runtime)
        self.assertNotIn('CREATEDB', runtime)

    def test_memberships_and_ownership(self):
        self.assertIn('GRANT lidradar_app, lidradar_worker, lidradar_platform TO lidradar WITH ADMIN OPTION;', self.code)
        self.assertIn('GRANT lidradar_app, lidradar_worker, lidradar_platform TO lidradar_runtime;', self.code)
        self.assertEqual(self.code.count('ADMIN OPTION'), 1)  # администрировать роли может только владелец
        self.assertIn("ALTER DATABASE %I OWNER TO lidradar", self.code)
        for role in ('lidradar_app', 'lidradar_worker', 'lidradar_platform'):
            self.assertIn(role, self.code)
        self.assertIn('NOLOGIN', self.code)

    def test_passwords_come_from_the_environment_and_are_never_written_down(self):
        self.assertIn('\\getenv owner_password LIDRADAR_DB_OWNER_PASSWORD', self.sql)
        self.assertIn('\\getenv runtime_password LIDRADAR_DB_RUNTIME_PASSWORD', self.sql)
        self.assertIn("PASSWORD :'owner_password'", self.code)
        self.assertIn("PASSWORD :'runtime_password'", self.code)
        self.assertNotRegex(self.code, r"PASSWORD\s+'[^']")
        # Пустой или незаданный пароль останавливает инициализацию, а не создаёт вход без пароля.
        self.assertIn('RAISE EXCEPTION', self.sql)
        self.assertIn('ON_ERROR_STOP', self.sql)


class EdgeConfigurationTests(unittest.TestCase):
    """Text-level guards for the reference Caddyfile (validated by Caddy in CI)."""

    def setUp(self):
        self.text = CADDYFILE.read_text(encoding='utf-8')

    def block(self, opening):
        start = self.text.index(opening)
        depth, index = 0, self.text.index('{', start)
        for position in range(index, len(self.text)):
            if self.text[position] == '{':
                depth += 1
            elif self.text[position] == '}':
                depth -= 1
                if depth == 0:
                    return self.text[start:position + 1]
        self.fail(f'блок {opening} не закрыт')

    def test_api_route_streams_and_is_not_compressed(self):
        api = self.block('handle @api')
        self.assertIn('flush_interval -1', api)
        self.assertNotIn('encode', api)
        self.assertIn('reverse_proxy api:8080', api)
        self.assertIn('max_size', api)

    def test_only_liveness_is_public(self):
        self.assertIn('handle /health/live', self.text)
        self.assertRegex(self.text, r'handle /health/\*\s*\{\s*respond "not found" 404')
        self.assertNotIn('/health/ready', re.sub(r'#.*', '', self.text))

    def test_client_supplied_forwarding_headers_are_not_trusted(self):
        code = re.sub(r'#.*', '', self.text)
        self.assertNotIn('trusted_proxies', code)
        self.assertNotIn('X-Forwarded', code)  # Caddy переписывает их сам
        self.assertIn('protocols h1 h2', code)

    def test_static_site_is_compressed_and_everything_else_goes_to_the_web_client(self):
        default = self.block('\thandle {\n\t\tencode')
        self.assertIn('encode zstd gzip', default)
        self.assertIn('reverse_proxy web:8080', default)


if __name__ == '__main__':
    unittest.main()
