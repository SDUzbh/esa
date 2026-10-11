#!/usr/bin/env python3
"""Verify real Kafka mainline tracking and optional neutral runtime observations.

This check makes no model calls and does not assign engineering accuracy labels.
"""
import argparse
import json
from pathlib import Path
import subprocess
import uuid

from backtest_pipeline import replay_future, tool
from replay_environment import ROOT, Environment, write_json
from replay_pipeline import plan, runtime_inputs, source_snapshot


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--runtime', action='store_true', help='Replay baseline, PR parent and repaired source in Docker')
    parser.add_argument('--output', default=str(ROOT / 'reports' / ('mainline-validation-' + uuid.uuid4().hex)))
    args = parser.parse_args()
    out = Path(args.output).resolve(); out.mkdir(parents=True, exist_ok=False)
    repo = ROOT / 'kafka'
    job = plan({'raw': str(ROOT / 'data/jira/raw/KAFKA-2880.json'), 'profile': 'metadata-disconnect'}, repo)
    p = job['resolved']
    issue = {'id': 'issue_KAFKA-2880', 'project_id': p['ticket']['project_id'], 'title': p['ticket']['subject'],
             'category': 'unknown', 'ticket_keys': ['KAFKA-2880'], 'findings': []}
    write_json(out / 'issue.json', issue)
    trace = tool('track', ['-repo', str(repo), '-issue', str(out / 'issue.json'), '-baseline', p['source_commit'],
                           '-mainline', 'origin/trunk', '-cutoff', p['cutoff'], '-versions', '1'])
    write_json(out / 'trace.json', trace)
    candidates = [c for c in trace['candidates'] if '#581' in c['prs']]
    if len(candidates) != 1: raise RuntimeError('Expected mainline PR #581 was not uniquely tracked')
    candidate = candidates[0]
    result = {'tracking': 'PASS', 'commit': candidate['commit'], 'prs': candidate['prs'], 'model_calls': 0,
              'accuracy': 'not_assessed', 'cleanup': 'not_required', 'observations': []}
    if args.runtime:
        env = None
        try:
            with Environment() as env:
                snapshot, manifest = source_snapshot(job, repo, env.cache)
                env.sync(snapshot / 'source', manifest['files'], runtime_inputs(job, snapshot, env))
                baseline = env.run('set -e; sh /input/install.sh; sh /input/observe.sh')
                result['observations'].append({'commit': p['source_commit'], 'exit_code': baseline.returncode,
                                               'log': baseline.stdout + baseline.stderr})
                env.freeze_build(); env.reset_job()
                for commit in (candidate['parent'], candidate['commit']):
                    result['observations'].append(replay_future({'trace_path': out / 'trace.json', 'commit': commit, 'job': job}, env, repo, runtime_inputs))
                    env.reset_job()
            result['cleanup'] = 'completed'
        finally:
            if env and not env.started and env.directory and not env.directory.exists(): result['cleanup'] = 'completed'
            write_json(out / 'validation.json', result)
        observations = result['observations']
        if not all('CLIENT_MODULE_COMPILATION=PASS' in o['log'] and 'SUCCESSFUL_METADATA_CONTROL=PASS' in o['log'] for o in observations):
            raise RuntimeError('Historical compilation or control failed')
        if 'DISCONNECTED_METADATA_RESULT=java.lang.NullPointerException' not in observations[0]['log']:
            raise RuntimeError('Baseline symptom was not observed')
        if 'DISCONNECTED_METADATA_RESULT=java.lang.NullPointerException' not in observations[1]['log']:
            raise RuntimeError('PR parent symptom was not observed')
        if 'DISCONNECTED_METADATA_RESULT=org.apache.kafka.common.errors.TimeoutException' not in observations[2]['log']:
            raise RuntimeError('PR source did not show the repaired disconnect/timeout behavior')
        result['registered_runtime_check'] = 'PASS'
    write_json(out / 'validation.json', result)
    print(json.dumps({'report': str(out / 'validation.json'), 'tracking': result['tracking'],
                      'runtime': result.get('registered_runtime_check', 'not_requested'), 'cleanup': result['cleanup'], 'model_calls': 0}))


if __name__ == '__main__': main()
