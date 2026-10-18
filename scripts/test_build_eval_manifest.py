#!/usr/bin/env python3
"""Check the evaluation-set builder on a synthetic repository and export."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import build_eval_manifest as build


def git(repo, *args, date=None):
    env = dict(os.environ, GIT_AUTHOR_DATE=date or '2020-01-01T00:00:00Z', GIT_COMMITTER_DATE=date or '2020-01-01T00:00:00Z')
    return subprocess.run(['git', '-C', str(repo), *args], env=env, capture_output=True, text=True, check=True).stdout.strip()


def ticket(root, key, created, kind='Bug', resolution='Fixed', versions=('1.0',), components=('core',), duplicates=(), edited=False):
    day = created[:10].replace('-', '/')
    path = Path(root, day, key + '.json')
    path.parent.mkdir(parents=True, exist_ok=True)
    changelog = [{'created': created, 'items': [{'field': 'Version', 'fromString': None, 'toString': '1.0'}]}] if edited else []
    links = [{'type': {'name': 'Duplicate'}, 'outwardIssue': {'key': d}} for d in duplicates]
    meta = {'history_complete': True, 'history_reported_total': len(changelog)}
    path.write_text(json.dumps({'metadata': meta, 'changelog': changelog, 'issue': {'key': key, 'fields': {
        'issuetype': {'name': kind}, 'resolution': {'name': resolution}, 'created': created, 'versions': [{'name': v} for v in versions],
        'components': [{'name': c} for c in components], 'issuelinks': links, 'summary': key, 'description': ''}}}))


class BuildTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        root = Path(self.dir.name)
        self.repo, self.raw = root / 'repo', root / 'raw'
        self.repo.mkdir()
        git(self.repo, 'init', '-q', '-b', 'trunk')
        git(self.repo, 'config', 'user.email', 'test@example.invalid')
        git(self.repo, 'config', 'user.name', 'Test')

        def commit(message, date, files):
            for name, text in files.items():
                Path(self.repo, name).parent.mkdir(parents=True, exist_ok=True)
                Path(self.repo, name).write_text(text)
            git(self.repo, 'add', '.')
            git(self.repo, 'commit', '-q', '-m', message, date=date)
        commit('base', '2020-01-01T00:00:00Z', {'src/A.java': 'a\n', 'src/test/ATest.java': 't\n'})
        git(self.repo, 'tag', '1.0')
        commit('KAFKA-1: fix source', '2020-02-01T00:00:00Z', {'src/A.java': 'b\n'})
        commit('KAFKA-2: tests only', '2020-02-02T00:00:00Z', {'src/test/ATest.java': 'u\n'})
        commit('KAFKA-5: new file only', '2020-02-03T00:00:00Z', {'src/B.java': 'n\n'})
        git(self.repo, 'tag', '1.1')
        commit('KAFKA-6: too late', '2020-04-01T00:00:00Z', {'src/A.java': 'c\n'})
        git(self.repo, 'tag', '1.2')
        git(self.repo, 'update-ref', 'refs/remotes/origin/trunk', 'HEAD')
        os.symlink('A.java', self.repo / 'src/link')
        commit('symlink', '2020-04-02T00:00:00Z', {})
        git(self.repo, 'tag', '1.3')
        ticket(self.raw, 'KAFKA-1', '2020-01-15T00:00:00.000+0000')
        ticket(self.raw, 'KAFKA-2', '2020-01-15T00:00:00.000+0000')
        ticket(self.raw, 'KAFKA-3', '2020-01-15T00:00:00.000+0000', edited=True)
        ticket(self.raw, 'KAFKA-4', '2020-01-20T00:00:00.000+0000', resolution='Duplicate', duplicates=['KAFKA-1'])
        ticket(self.raw, 'KAFKA-5', '2020-01-15T00:00:00.000+0000')
        ticket(self.raw, 'KAFKA-6', '2020-01-15T00:00:00.000+0000')
        ticket(self.raw, 'KAFKA-7', '2019-12-01T00:00:00.000+0000')  # its version is released later

    def tearDown(self):
        self.dir.cleanup()

    def run_build(self, *extra):
        out = Path(self.dir.name, 'out-%d' % len(list(Path(self.dir.name).glob('out-*'))))
        subprocess.run(['python3', '-B', str(Path(build.__file__)), '--raw', str(self.raw), '--repo', str(self.repo), '--versions', '1',
                        '--bugs', '5', '--support', '2', '--output', str(out), *extra], check=True, capture_output=True)
        self.releases = json.loads((out / 'releases.json').read_text())['releases']
        return json.loads((out / 'manifest.json').read_text()), json.loads((out / 'labels.json').read_text())

    def test_selection_labels_and_modes(self):
        manifest, labels = self.run_build()
        self.assertEqual(labels['meta']['eligible_bugs'], 3)  # 1, 2 and 5; 6 lands after the window
        jobs = labels['jobs']
        # Bugs whose fix changes only tests or adds files have no ground truth.
        self.assertEqual({k: v['expected_route'] for k, v in jobs.items()},
                         {'KAFKA-1': 'analyze', 'KAFKA-4': 'reuse', 'SUPPORT-1': 'return_to_support', 'SUPPORT-2': 'return_to_support'})
        self.assertEqual((jobs['KAFKA-4']['original'], jobs['KAFKA-1']['selected']), ('KAFKA-1', 'duplicate_original'))
        support = next(j for j in manifest['jobs'] if j['id'] == 'SUPPORT-1')
        self.assertEqual((support['ref'], support['ticket']['versions']), ('refs/tags/1.0', ['1.0']))
        self.assertIn('esa_fixture_target.py', ' '.join(manifest['target']['start']['args']))
        self.assertEqual(manifest['target']['extract']['route'][0], '/ticket/latest_run/report/routing/action')
        self.assertTrue({'returned_to_support', 'joined', 'awaiting_information'} <= set(manifest['target']['terminal_states']))
        self.assertEqual([(r['version'], r['ref'], r['available_at']) for r in self.releases], [('1.0', 'refs/tags/1.0', '2020-01-01T00:00:00Z')])
        self.assertEqual(self.run_build(), (manifest, labels))
        provider, _ = self.run_build('--mode', 'provider', '--issues-directory', self.dir.name)
        env = provider['target']['start']['env']
        self.assertNotIn('ESA_ANALYZER_TOKEN', env)
        self.assertNotIn('ESA_ANALYZER_URL', env)
        self.assertEqual(provider['target']['timeout_seconds'], 1800)

    def test_helpers(self):
        history = build.History(self.repo, 'origin/trunk', 'KAFKA')
        self.assertEqual((history.exportable('1.0'), history.exportable('1.3')), (True, False))
        self.assertEqual(build.allocate({'a': [1] * 6, 'b': [1] * 3, 'c': [1]}, 5), {'a': 3, 'b': 2, 'c': 0})
        self.assertEqual(sum(build.allocate({'a': [1] * 2, 'b': [1]}, 10).values()), 3)
        for name, expected in [('clients/src/test/java/FooTest.java', True), ('tests/kafkatest/x.py', True), ('core/src/main/A.scala', False)]:
            self.assertEqual(build.test_file(name), expected)
        for name, expected in [('core/src/main/A.scala', True), ('README.md', False), ('build.gradle', True), ('contrib/LICENSE', False), ('docs/upgrade.html', False), ('clients/src/test/java/FooTest.java', False)]:
            self.assertEqual(build.fix_file(name), expected)


if __name__ == '__main__':
    unittest.main()
