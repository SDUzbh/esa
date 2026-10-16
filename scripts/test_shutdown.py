#!/usr/bin/env python3
"""Check shutdown ownership and data-retention boundaries without live runtimes."""
from contextlib import redirect_stdout
import io
import json
from pathlib import Path
import signal
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import shutdown


class FakeDocker:
    def __init__(self, dry_run=False):
        self.dry_run = dry_run
        self.actions = []
        self.items = {
            'container': [('redmine', {'com.docker.compose.project': 'esa-redmine'}),
                          ('sandbox', {'esa.replay.session': 'a' * 32}),
                          ('external', {'system.replay.session': 'b' * 32}),
                          ('test', {'com.docker.compose.project': 'esa-deploy-123456abcdef'}),
                          ('unrelated', {'com.docker.compose.project': 'other'})],
            'volume': [('database', {'com.docker.compose.project': 'esa-redmine'}),
                       ('scratch', {'esa.replay.session': 'a' * 32}),
                       ('other-data', {'com.docker.compose.project': 'other'})],
            'network': [('main-net', {'com.docker.compose.project': 'esa-redmine'}),
                        ('test-net', {'com.docker.compose.project': 'esa-deploy-123456abcdef'})]}

    def resources(self, kind):
        yield from list(self.items[kind])

    def read(self, *args):
        if args[:2] == ('image', 'ls'):
            return 'image-id'
        return ''

    def change(self, *args):
        self.actions.append(args)
        if not self.dry_run:
            if args[0] == 'rm':
                self.items['container'] = [row for row in self.items['container'] if row[0] != args[-1]]
            elif args[0] in ('volume', 'network'):
                self.items[args[0]] = [row for row in self.items[args[0]] if row[0] != args[-1]]


class ShutdownTests(unittest.TestCase):
    def test_only_project_entry_points_and_no_argument_substring_matching(self):
        root = shutdown.ROOT
        for argv, cwd, expected in [
                ([str(root / 'bin/esa-codex'), '-registry', 'private.json'], None, True),
                (['bin/esa-host'], root, True),
                (['bin/esa-host'], Path('/another-checkout'), False),
                (['python3', '-B', 'scripts/replay_session.py'], root, True),
                (['Python', str(root / 'system_replay/runtime_driver.py')], None, True),
                (['go', 'run', './cmd/esa'], root, True),
                (['go', 'run', './cmd/esa'], Path('/another-checkout'), False),
                (['python3', 'scripts/kafka_jira_downloader.py'], root, False),
                (['sh', '-c', 'echo ' + str(root / 'bin/esa-codex')], root, False),
                (['unrelated-app', str(root / 'bin/esa-codex')], root, False)]:
            with self.subTest(argv=argv):
                self.assertEqual(shutdown.runtime_process(argv, cwd), expected)

    def test_cleanup_preserves_database_and_unrelated_resources(self):
        docker = FakeDocker()
        shutdown.clean_docker(docker)
        self.assertIn(('stop', '--time', '30', 'redmine'), docker.actions)
        self.assertIn(('rm', '--force', '--volumes', 'sandbox'), docker.actions)
        self.assertIn(('rm', '--force', '--volumes', 'external'), docker.actions)
        self.assertEqual([row[0] for row in docker.items['volume']], ['database', 'other-data'])
        self.assertEqual([row[0] for row in docker.items['network']], ['main-net'])
        self.assertTrue(all('unrelated' not in args for args in docker.actions))
        self.assertEqual(len([args for args in docker.actions if args[:2] == ('image', 'rm')]), 3)
        for project in ('esa-deploy-user', 'esa-redmine-copy', 'other'):
            self.assertEqual(shutdown.resource_scope({'com.docker.compose.project': project}), 'foreign')

    def test_preview_never_mutates_docker_or_sends_a_signal(self):
        docker = shutdown.Docker('unused', True)
        with patch.object(docker, 'read') as read, redirect_stdout(io.StringIO()):
            docker.change('rm', '--force', 'example')
        read.assert_not_called()
        with patch('shutdown.processes', return_value={999999: (1, str(shutdown.ROOT / 'bin/esa-codex'))}), \
                patch('shutdown.working_directory', return_value=shutdown.ROOT), \
                patch('shutdown.os.kill') as kill, redirect_stdout(io.StringIO()):
            shutdown.stop_processes(True, 1)
        kill.assert_not_called()

    def test_stale_child_is_terminated_even_after_reparenting(self):
        owner = str(shutdown.ROOT / 'bin/esa-codex')
        child = 'codex exec --test-fixture'
        states = [{999990: (1, owner), 999991: (999990, child)}]

        def kill(pid, sig):
            if pid == 999990 and sig == signal.SIGTERM:
                states[0] = {999991: (1, child)}
            elif pid == 999991:
                states[0] = {}

        with patch('shutdown.processes', side_effect=lambda: dict(states[0])), \
                patch('shutdown.working_directory', return_value=shutdown.ROOT), \
                patch('shutdown.os.kill', side_effect=kill) as sent, \
                patch('shutdown.time.sleep'), \
                patch('shutdown.time.monotonic', side_effect=range(100)), redirect_stdout(io.StringIO()):
            shutdown.stop_processes(False, 1)
        sent.assert_any_call(999991, signal.SIGTERM)

    def test_only_stale_selected_context_session_directories_are_removed(self):
        with tempfile.TemporaryDirectory() as temporary, patch('shutdown.ROOT', Path(temporary)):
            parent = Path(temporary) / '.esa/replay-sessions'
            session = 'a' * 32
            directory = parent / (session + '-scratch')
            directory.mkdir(parents=True)
            marker = directory / 'session.json'
            marker.write_text(json.dumps({'session': session, 'pid': 999999, 'context': 'chosen'}))
            with patch('shutdown.os.kill', side_effect=ProcessLookupError), redirect_stdout(io.StringIO()):
                shutdown.clean_session_directories('other-context', False)
                self.assertTrue(directory.exists())
                shutdown.clean_session_directories('chosen', True)
                self.assertTrue(directory.exists())
                shutdown.clean_session_directories('chosen', False)
                self.assertFalse(directory.exists())

    def test_failure_to_remove_runtime_is_not_reported_as_success(self):
        docker = FakeDocker()
        docker.change = lambda *args: None
        with self.assertRaisesRegex(RuntimeError, 'survived cleanup'):
            shutdown.clean_docker(docker)

    def test_stopped_vm_is_not_restarted_for_cleanup(self):
        def run(args, **kwargs):
            if args[0] == 'docker':
                return SimpleNamespace(returncode=1, stdout='')
            self.assertEqual(args, ['colima', 'list', '--json'])
            return SimpleNamespace(returncode=0, stdout='{"name":"redmine","status":"Stopped"}\n')
        with patch('sys.argv', ['shutdown.py']), patch('shutdown.stop_processes'), \
                patch('shutdown.command', side_effect=run), patch('shutdown.shutil.which', return_value='/bin/colima'), \
                patch('shutdown.clean_docker') as clean, redirect_stdout(io.StringIO()):
            self.assertEqual(shutdown.main(), 0)
        clean.assert_not_called()

    def test_foreign_running_container_prevents_vm_shutdown(self):
        docker = FakeDocker()
        docker.prefix = ['docker', '--context', 'chosen']
        docker.read = lambda *args: 'unrelated' if args[:3] == ('container', 'ls', '-q') else ''
        with patch('sys.argv', ['shutdown.py']), patch('shutdown.stop_processes'), \
                patch('shutdown.Docker', return_value=docker), \
                patch('shutdown.command', return_value=SimpleNamespace(returncode=0)) as run, \
                patch('shutdown.clean_docker'), patch('shutdown.clean_session_directories'), \
                redirect_stdout(io.StringIO()):
            self.assertEqual(shutdown.main(), 0)
        self.assertFalse(any(call.args[0][0] == 'colima' for call in run.call_args_list))


if __name__ == '__main__':
    unittest.main()
