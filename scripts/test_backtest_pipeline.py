import json
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

import backtest_pipeline as pipeline


class PipelineTests(unittest.TestCase):
    def test_issue_visibility_is_temporal_and_project_scoped(self):
        history = [{'id': '1', 'project_id': 'A', 'category': 'code_defect', 'title': 'Old', 'ticket_keys': ['A-1'], 'observed_at': '2020-01-01T00:00:00Z'},
                   {'id': '1', 'project_id': 'A', 'category': 'code_defect', 'title': 'Future', 'ticket_keys': ['A-1', 'A-2'], 'observed_at': '2020-02-01T00:00:00Z'},
                   {'id': '2', 'project_id': 'B', 'category': 'code_defect', 'title': 'Other', 'ticket_keys': ['B-1'], 'observed_at': '2020-01-01T00:00:00Z'}]
        visible = pipeline.visible_issues(history, {'project_id': 'A'}, '2020-01-10T00:00:00Z')
        self.assertEqual(visible[0]['title'], 'Old')
        self.assertEqual(len(visible), 1)
        self.assertEqual(visible[0]['ticket_keys'], ['A-1'])

    def test_ticket_token_warnings_are_advisory_and_failed_usage_unknown(self):
        result = {'source_key': 'BUG-1', 'usage': {'input_tokens': 6, 'output_tokens': 4, 'tokens_reported': True}, 'review_errors': [{'error': 'timeout'}]}
        usage = pipeline.aggregate_usage(result, [{'usage': {'input_tokens': 6, 'output_tokens': 4, 'tokens_reported': True}}],
                                         {'max_input_tokens': 10, 'max_output_tokens': 5, 'warn_total_tokens': 15})
        self.assertEqual(len(usage['alerts']), 3)
        self.assertFalse(usage['all_calls_reported'])
        self.assertEqual(usage['input_tokens'], 12)

    def test_all_predictions_freeze_before_future_read_and_cleanup_on_failure(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); events = []; count = 0
            class Runtime:
                def __init__(self, *args):
                    self.started = True; self.directory = root / 'session'; self.directory.mkdir()
                def __enter__(self): return self
                def reset_job(self): events.append('reset')
                def __exit__(self, *args):
                    events.append('cleanup'); self.started = False; self.directory.rmdir()
            def run_job(job, env, repo, output, agent, known):
                key = job['resolved']['ticket']['source_key']; events.append('diagnose:' + key)
                directory = output / key; directory.mkdir()
                report = {'agent_classification': {'category': 'code_defect'}, 'issue': {'existing_id': '', 'title': key, 'reason': 'New'}, 'findings': []}
                (directory / 'report.json').write_text(json.dumps(report))
                return {'source_key': key, 'directory': str(directory), 'usage': {'calls': 1, 'tokens_reported': False}}
            def go(mode, args):
                nonlocal count
                if mode == 'assign':
                    ticket = json.loads(Path(args[args.index('-ticket') + 1]).read_text())
                    items = json.loads(Path(args[args.index('-registry') + 1]).read_text())
                    issue = {'id': ticket['source_key'], 'project_id': 'A', 'category': 'code_defect', 'title': 'Defect', 'ticket_keys': [ticket['source_key']], 'findings': []}
                    return {'issue': issue, 'issues': items + [issue]}
                if mode == 'track':
                    self.assertEqual(sum(e.startswith('diagnose:') for e in events), 2)
                    for key in ('BUG-1', 'BUG-2'):
                        self.assertFalse((root / key / 'report.json').stat().st_mode & 0o200)
                    events.append('future'); count += 1
                    if count == 2: raise RuntimeError('Git unavailable')
                    return {'candidates': [], 'versions': [], 'state': 'window_exhausted'}
                if mode == 'evaluate': return {'state': 'unresolved', 'correct_findings': 0, 'incorrect_findings': 0, 'unknown_findings': 0, 'pending_candidates': 0}
                raise AssertionError(mode)
            args = SimpleNamespace(repo=str(root), predictions=None, track_only=False, review_fixes=False, agent=True,
                                   mainline='main', versions=2, until=None, cache=str(root), context='test')
            jobs = [{'resolved': {'ticket': {'source_key': key, 'project_id': 'A', 'subject': key}, 'cutoff': '2020-01-01T00:00:00Z', 'source_commit': 'old'}, 'version': '1.0', 'profile': 'source'} for key in ('BUG-1', 'BUG-2')]
            summary = {'results': [], 'deferred': [], 'cleanup': 'not_started'}
            with patch.object(pipeline, 'Environment', Runtime), patch.object(pipeline, 'tool', go), patch.object(pipeline.subprocess, 'run', return_value=SimpleNamespace(stdout='old\n')):
                code = pipeline.longitudinal(args, jobs, root, summary, run_job, None)
            self.assertEqual(code, 1)
            self.assertEqual(summary['cleanup'], 'completed')
            self.assertEqual(events[-1], 'cleanup')
            self.assertIn('future', events)
            self.assertEqual(summary['results'][1]['evaluation_state'], 'failed')


if __name__ == '__main__': unittest.main()
