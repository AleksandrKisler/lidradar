"""Release image builder tests use a fake docker and a throw-away git repository."""
from pathlib import Path
import os
import shutil
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'build-images.sh'
GO_COMMANDS = ['api', 'worker', 'scheduler', 'migrate', 'ai-agent', 'platform-admin', 'ai-node-register', 'ai-node-manage']


def git(directory, *args):
    return subprocess.run(['git', '-C', str(directory), '-c', 'user.name=t', '-c', 'user.email=t@example.invalid', *args],
                          check=True, capture_output=True, text=True).stdout.strip()


def make_repo(directory, with_dockerfile=False):
    directory.mkdir(parents=True, exist_ok=True)
    git(directory, 'init', '-q')
    (directory / 'file.txt').write_text('x')
    if with_dockerfile:
        (directory / 'Dockerfile').write_text('FROM scratch\n')
    git(directory, 'add', '-A')
    git(directory, 'commit', '-q', '-m', 'initial')
    return git(directory, 'rev-parse', 'HEAD')


class BuildImagesTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / 'backend'
        self.sha = make_repo(self.root)
        (self.root / 'scripts').mkdir()
        shutil.copy(SCRIPT, self.root / 'scripts' / 'build-images.sh')
        git(self.root, 'add', '-A')
        git(self.root, 'commit', '-q', '-m', 'script')
        self.sha = git(self.root, 'rev-parse', 'HEAD')
        self.bin = Path(self.temp.name) / 'bin'
        self.bin.mkdir()
        self.calls = Path(self.temp.name) / 'docker-calls'
        self.fake_docker('echo "docker $*" >> "$QA_CALLS"\n'
                         'if [ "$1" = image ]; then echo "sha256:id-of-${@: -1}"; fi')
        self.env = {**os.environ, 'PATH': f'{self.bin}{os.pathsep}{os.environ["PATH"]}', 'QA_CALLS': str(self.calls)}
        self.env.pop('LIDRADAR_WEB_DIR', None)

    def fake_docker(self, body):
        path = self.bin / 'docker'
        path.write_text('#!/usr/bin/env bash\nset -u\n' + body + '\n')
        path.chmod(0o755)

    def run_script(self, *args):
        return subprocess.run(['bash', str(self.root / 'scripts' / 'build-images.sh'), *args],
                              env=self.env, capture_output=True, text=True)

    def builds(self):
        lines = self.calls.read_text().splitlines() if self.calls.exists() else []
        return [line for line in lines if line.startswith('docker build ')]

    def test_every_command_is_built_from_the_commit_and_tagged_with_its_sha(self):
        result = self.run_script('--no-web')
        self.assertEqual(result.returncode, 0, result.stderr)
        tag = self.sha[:12]
        builds = self.builds()
        self.assertEqual(len(builds), len(GO_COMMANDS) + 1, builds)
        for command, line in zip(GO_COMMANDS, builds):
            self.assertIn(f'--build-arg COMMAND={command} ', line + ' ')
            self.assertIn(f'--build-arg VERSION={tag} ', line)
            self.assertIn(f'--build-arg REVISION={self.sha} ', line)
            self.assertIn(f'-t lidradar-{command}:{tag} ', line)
            self.assertTrue(line.endswith(' .'), line)
        # Образ копий вне хоста собирается из того же коммита по своему Dockerfile.
        backup = builds[-1]
        self.assertIn('-f deploy/backup/Dockerfile ', backup)
        self.assertNotIn('COMMAND=', backup)
        self.assertIn(f'--build-arg VERSION={tag} ', backup)
        self.assertIn(f'--build-arg REVISION={self.sha} ', backup)
        self.assertIn(f'-t lidradar-backup:{tag} ', backup + ' ')
        self.assertTrue(backup.endswith(' .'), backup)
        self.assertIn(f'LIDRADAR_VERSION={tag}', result.stdout)
        self.assertIn(f'lidradar-api:{tag}  sha256:id-of-lidradar-api:{tag}', result.stdout)
        self.assertIn(f'lidradar-backup:{tag}  sha256:id-of-lidradar-backup:{tag}', result.stdout)

    def test_dirty_tree_is_refused_before_docker_is_called(self):
        (self.root / 'untracked.txt').write_text('change')
        result = self.run_script('--no-web')
        self.assertEqual(result.returncode, 2)
        self.assertIn('незафиксированные', result.stderr)
        self.assertFalse(self.calls.exists())

    def test_allow_dirty_marks_the_tag_so_it_cannot_pass_for_a_release(self):
        (self.root / 'untracked.txt').write_text('change')
        result = self.run_script('--no-web', '--allow-dirty', '--only', 'api')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f'-t lidradar-api:{self.sha[:12]}-dirty ', self.builds()[0] + ' ')

    def test_only_limits_the_commands_and_unknown_ones_are_refused(self):
        self.assertEqual(self.run_script('--no-web', '--only', 'api,worker,backup').returncode, 0)
        self.assertEqual(len(self.builds()), 3)
        self.calls.unlink()
        result = self.run_script('--no-web', '--only', 'api,backdoor')
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.calls.exists())

    def test_tag_is_validated(self):
        for tag in ('bad tag', '-leading', 'a/b', ''):
            with self.subTest(tag=tag):
                result = self.run_script('--no-web', '--tag', tag, '--only', 'api')
                self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.calls.exists())

    def test_web_client_needs_a_directory_a_dockerfile_and_a_clean_tree(self):
        self.assertEqual(self.run_script().returncode, 2)  # ни --web-dir, ни --no-web
        web = Path(self.temp.name) / 'web'
        make_repo(web, with_dockerfile=False)
        self.assertEqual(self.run_script('--web-dir', str(web)).returncode, 2)  # нет Dockerfile
        web_sha = make_repo(Path(self.temp.name) / 'web2', with_dockerfile=True)
        web2 = Path(self.temp.name) / 'web2'
        (web2 / 'edited.txt').write_text('x')
        result = self.run_script('--web-dir', str(web2), '--only', 'api')
        self.assertEqual(result.returncode, 2)
        self.assertIn('веб-клиента', result.stderr)
        self.assertFalse(self.calls.exists())
        (web2 / 'edited.txt').unlink()
        result = self.run_script('--web-dir', str(web2), '--only', 'api')
        self.assertEqual(result.returncode, 0, result.stderr)
        web_build = self.builds()[-1]
        tag = self.sha[:12]
        self.assertIn('--build-arg APP_MODE=production', web_build)
        self.assertIn(f'--label org.opencontainers.image.revision={web_sha}', web_build)
        self.assertIn(f'--label org.opencontainers.image.version={tag}', web_build)
        self.assertIn(f'-t lidradar-web:{tag} {web2}', web_build)
        self.assertIn(f'lidradar-web:{tag}  sha256:id-of-lidradar-web:{tag}', result.stdout)

    def test_web_directory_may_come_from_the_environment(self):
        web = Path(self.temp.name) / 'web3'
        make_repo(web, with_dockerfile=True)
        self.env['LIDRADAR_WEB_DIR'] = str(web)
        self.assertEqual(self.run_script('--only', 'api').returncode, 0)
        self.assertIn('lidradar-web:', self.builds()[-1])

    def test_failed_build_stops_the_release(self):
        self.fake_docker('echo "docker $*" >> "$QA_CALLS"\n'
                         'case "$*" in *COMMAND=worker*) exit 1 ;; esac')
        result = self.run_script('--no-web')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.builds()), 2)  # api и упавший worker; дальше не идём (и образ копий тоже)
        self.assertNotIn('LIDRADAR_VERSION=', result.stdout)


if __name__ == '__main__':
    unittest.main()
