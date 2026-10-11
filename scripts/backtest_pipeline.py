"""Two-phase ticket-centered backtest orchestration; decisions live in Go."""
from datetime import datetime
import hashlib
import json
import re
from pathlib import Path
import subprocess
import sys
import uuid

from replay_environment import ROOT, Environment, write_json


def tool(mode, args):
    run = subprocess.run([str(ROOT / 'bin/esa-backtest'), '-mode', mode] + list(args), cwd=ROOT, text=True, capture_output=True)
    if run.returncode:
        raise RuntimeError(run.stderr.strip())
    return json.loads(run.stdout)


def visible_issues(history, ticket, cutoff):
    latest = {}
    for item in history:
        if item['project_id'] == ticket['project_id'] and datetime.fromisoformat(item['observed_at'].replace('Z', '+00:00')) <= datetime.fromisoformat(cutoff.replace('Z', '+00:00')):
            if item['id'] not in latest or datetime.fromisoformat(item['observed_at'].replace('Z', '+00:00')) > datetime.fromisoformat(latest[item['id']]['observed_at'].replace('Z', '+00:00')): latest[item['id']] = item
    return [{k: issue[k] for k in ('id', 'category', 'title', 'ticket_keys')} for issue in latest.values()]


def freeze_import(job, output, predictions):
    p = job['resolved']; key = p['ticket']['source_key']
    directory = output / (re.sub(r'[^A-Za-z0-9_-]', '_', key) + '-' + uuid.uuid4().hex[:8]); directory.mkdir()
    write_json(directory / 'plan.json', p)
    result = {'source_key': key, 'source_commit': p['source_commit'], 'version': job['version'],
              'directory': str(directory), 'model_calls': 0, 'state': 'no_prediction'}
    if key in predictions:
        path = Path(predictions[key]).resolve()
        # Validate the external report against exact historical source lines.
        tool('validate', ['-prediction', str(path), '-baseline', p['source_commit'], '-repo', job['_repo']])
        (directory / 'report.json').write_bytes(path.read_bytes())
        result['prediction_origin'] = 'operator_supplied'
        result['usage'] = json.loads(path.read_text()).get('usage')
        result['state'] = 'diagnosed'
    write_json(directory / 'result.json', result)
    return result


def run_provider(args, directory):
    with (directory / 'provider.log').open('w') as log:
        child = subprocess.Popen(args, cwd=ROOT, text=True, stdout=subprocess.PIPE, stderr=log)
        try:
            stdout, _ = child.communicate()
        except BaseException:
            child.terminate()
            try: child.wait(timeout=45)
            except subprocess.TimeoutExpired: child.kill(); child.wait()
            raise
    if child.returncode:
        raise RuntimeError((directory / 'provider.log').read_text().strip() or 'Agent execution failed')
    return json.loads(stdout)


def future_snapshot(repo, trace, commit, profile, cache):
    scope = 'clients' if profile == 'metadata-disconnect' else 'full'
    directory = cache / 'mainline-v1-source' / (commit + '-' + scope)
    if not (directory / 'manifest.json').exists():
        tool('export', ['-repo', str(repo), '-trace', str(trace), '-commit', commit, '-out', str(directory), '-scope', scope])
    manifest = json.loads((directory / 'manifest.json').read_text())
    if manifest['source_commit'] != commit or manifest['scope'] != scope:
        raise RuntimeError('Evaluation source cache mismatch')
    return directory, manifest


def replay_future(task, env, repo, runtime_inputs):
    snapshot, manifest = future_snapshot(repo, task['trace_path'], task['commit'], task['job']['profile'], env.cache)
    job = dict(task['job']); job['evaluation'] = True
    delta = env.sync(snapshot / 'source', manifest['files'], runtime_inputs(job, snapshot, env))
    command = job.get('command') or 'set -e; sh /input/install.sh; ' + {
        'metadata-disconnect': 'sh /input/observe.sh', 'source': 'java -version',
        'kafka-server': 'sh /input/start-kafka.sh'}[job['profile']]
    # kafka-server uses release binaries, so it cannot verify arbitrary Git fixes.
    if job['profile'] == 'kafka-server':
        return {'commit': task['commit'], 'state': 'unsupported', 'scope': 'A release binary cannot verify this mainline source commit.'}
    run = env.run(command, job.get('command_timeout', 180))
    log = run.stdout + run.stderr
    record = {'commit': task['commit'], 'exit_code': run.returncode, 'environment_delta': delta,
              'runtime_image': env.jdk_digest, 'profile': job['profile'],
              'scope': 'Registered client observation' if job['profile'] == 'metadata-disconnect' else 'Environment readiness only',
              'log_sha256': hashlib.sha256(log.encode()).hexdigest(), 'log': log}
    env.freeze_build()
    return record


def aggregate_usage(result, reviews, budget):
    usages = [result.get('usage')] + (result['review_attempt_usage'] if 'review_attempt_usage' in result else [r.get('usage') for r in reviews])
    known = [u for u in usages if u and u.get('tokens_reported')]
    input_tokens = sum(u['input_tokens'] for u in known)
    output_tokens = sum(u['output_tokens'] for u in known)
    alerts = []
    for name, actual, limit in [('input', input_tokens, budget['max_input_tokens']),
                                ('output', output_tokens, budget['max_output_tokens']),
                                ('total', input_tokens + output_tokens, budget.get('warn_total_tokens', 0))]:
        if limit and actual > limit:
            alert = {'code': 'ticket_token_' + name + '_high', 'message':
                     f'Ticket aggregate {name} tokens {actual} exceed advisory threshold {limit}; analysis continues.'}
            alerts.append(alert); print('WARNING ' + result['source_key'] + ': ' + alert['message'], file=sys.stderr, flush=True)
    return {'input_tokens': input_tokens, 'output_tokens': output_tokens,
            'scope': 'Measured successful reports only; failed or external calls may be unknown.',
            'all_calls_reported': not result.get('review_errors') and bool(usages) and all(u and u.get('tokens_reported') for u in usages), 'alerts': alerts}


def longitudinal(args, prepared, output, summary, run_job, runtime_inputs):
    """Freeze every diagnosis before reading future commits; share a single runtime."""
    repo = Path(args.repo).resolve()
    predictions = json.loads(Path(args.predictions).read_text()) if args.predictions else {}
    history = json.loads(Path(args.issue_history).read_text()) if getattr(args, 'issue_history', None) else []
    latest = {}
    for revision in sorted(history, key=lambda x: datetime.fromisoformat(x['observed_at'].replace('Z', '+00:00'))):
        if not revision.get('observed_at'): raise ValueError('Historical issue revisions require an observation time')
        latest[revision['id']] = {k: v for k, v in revision.items() if k != 'observed_at'}
    registry = list(latest.values()); entries = []
    summary['phase'] = 'diagnosis'; summary['design'] = 'ticket-centered-mainline-pr-review-v1'
    env = None
    def save(): write_json(output / 'pipeline.json', summary)
    def initial(env):
        nonlocal registry
        for job in prepared:
            key = job['resolved']['ticket']['source_key']; p = job['resolved']
            job['_repo'] = str(repo)
            known = visible_issues(history, p['ticket'], p['cutoff'])
            try:
                if args.track_only or key in predictions:
                    result = freeze_import(job, output, predictions)
                else:
                    result = run_job(job, env, repo, output, args.agent, known)
                directory = Path(result['directory']); report_path = directory / 'report.json'
                report = json.loads(report_path.read_text()) if report_path.exists() else {}
                if not report and result.get('agent_error'):
                    for audit_path in (directory / 'codex-runs').glob('*/audit.json'):
                        result['usage'] = json.loads(audit_path.read_text()).get('usage')
                if report:
                    report_path.chmod(0o400)
                    result['report_sha256'] = hashlib.sha256(report_path.read_bytes()).hexdigest()
                    write_json(directory / 'ticket.json', p['ticket'])
                    write_json(output / 'issues.json', registry)
                    assigned = tool('assign', ['-registry', str(output / 'issues.json'), '-ticket', str(directory / 'ticket.json'), '-prediction', str(report_path)])
                    registry = assigned['issues']
                    issue = assigned['issue']; revision = dict(issue, observed_at=p['cutoff']); history.append(revision)
                    result['issue_id'] = issue['id']
                else:
                    issue = {'id': 'issue_' + key, 'project_id': p['ticket']['project_id'], 'title': p['ticket']['subject'],
                             'category': 'unknown', 'ticket_keys': [key], 'findings': [], 'created_at': p['cutoff']}
                    existing = next((i for i in registry if key in i['ticket_keys'] and i['project_id'] == p['ticket']['project_id']), None)
                    if existing: issue = existing
                    else: registry.append(issue)
                    result['issue_id'] = issue['id']
                entries.append({'job': job, 'result': result, 'directory': directory, 'prediction': report, 'issue': issue})
                summary['results'].append(result)
            except (RuntimeError, OSError, ValueError, subprocess.SubprocessError) as error:
                summary['results'].append({'source_key': key, 'state': 'failed', 'error': str(error)})
            finally:
                if env: env.reset_job()
                save()
        return registry
    def evaluation(env, registry):
        summary['phase'] = 'version_tracking'; save()
        tasks = {}; order_text = subprocess.run(['git', '-C', str(repo), 'rev-list', '--first-parent', '--reverse', args.mainline], check=True, text=True, capture_output=True).stdout
        order = {commit: i for i, commit in enumerate(order_text.splitlines())}
        for entry in entries:
            job, result, directory = entry['job'], entry['result'], entry['directory']; p = job['resolved']
            if entry['prediction'].get('agent_classification', {}).get('category') in ('configuration', 'usage_question', 'feature_request'):
                result['evaluation_state'] = 'customer_support'; continue
            # Trace the final issue's scope, retaining each finding's original commit
            # so Go can map different tickets' locations into this mainline graph.
            issue = dict(next(i for i in registry if i['id'] == entry['issue']['id']))
            issue['findings'] = issue.get('findings', [])
            write_json(directory / 'issue.json', issue)
            params = ['-repo', str(repo), '-issue', str(directory / 'issue.json'), '-baseline', p['source_commit'],
                      '-mainline', args.mainline, '-cutoff', p['cutoff'], '-versions', str(args.versions)]
            if args.until: params += ['-until', args.until]
            try: trace = tool('track', params)
            except RuntimeError as error:
                result['evaluation_state'] = 'failed'; result['evaluation_error'] = str(error); continue
            write_json(directory / 'trace.json', trace); entry['trace'] = trace; entry['reviews'] = []
            result['candidate_count'] = len(trace['candidates']); result['evaluation_state'] = trace['state']
            if not args.review_fixes or not entry['prediction']: continue
            # Schedule every version observation and candidate before/after point
            # globally in mainline order, applying only environment differences.
            commits = {v['mainline_commit'] for v in trace['versions']}
            commits.update(c[k] for c in trace['candidates'] for k in ('parent', 'commit'))
            for commit in commits:
                tasks.setdefault(commit, []).append({'commit': commit, 'job': job, 'entry': entry, 'trace_path': directory / 'trace.json'})
        summary['phase'] = 'retrospective_review'; save()
        for commit in sorted(tasks, key=lambda c: order.get(c, -1)):
            for task in tasks[commit]:
                entry = task['entry']; directory = entry['directory']; result = entry['result']
                runtime_dir = directory / 'runtime' / commit; runtime_dir.mkdir(parents=True, exist_ok=True)
                try:
                    runtime = replay_future(task, env, repo, runtime_inputs)
                    write_json(runtime_dir / 'observation.json', runtime)
                    for candidate in entry['trace']['candidates']:
                        if candidate['commit'] != commit: continue
                        review_dir = directory / 'reviews' / commit; review_dir.mkdir(parents=True, exist_ok=False)
                        packet = tool('packet', ['-repo', str(repo), '-trace', str(directory / 'trace.json'), '-prediction', str(directory / 'report.json'),
                                                 '-source-key', result['source_key'], '-commit', commit])
                        before = directory / 'runtime' / candidate['parent'] / 'observation.json'
                        packet['runtime'] = {'before': json.loads(before.read_text()) if before.exists() else {'state': 'unavailable'}, 'after': runtime}
                        for label, observation in packet['runtime'].items():
                            if 'log' in observation:
                                packet['documents'].append({'source': 'replay', 'reference_prefix': label + ':' + observation['commit'] + ':',
                                                            'first_line': 1, 'lines': observation['log'].splitlines()})
                        write_json(review_dir / 'packet.json', packet)
                        review = run_provider([str(ROOT / 'bin/esa-codex'), '-review', str(review_dir / 'packet.json'),
                                               '-runs', str(review_dir / 'codex-runs'), '-run-id', 'run_' + uuid.uuid4().hex], review_dir)
                        result.setdefault('review_attempt_usage', []).append(review.get('usage'))
                        entry['reviews'].append(review); write_json(review_dir / 'review.json', review)
                        for alert in review.get('alerts', []): print('WARNING ' + result['source_key'] + ': ' + alert['message'], file=sys.stderr, flush=True)
                        budget = json.loads((ROOT / 'config/rules.json').read_text())['budget']
                        result['aggregate_usage'] = aggregate_usage(result, entry['reviews'], budget)
                except (RuntimeError, OSError, ValueError, subprocess.SubprocessError) as error:
                    result.setdefault('review_errors', []).append({'commit': commit, 'error': str(error)})
                    audit_dir = directory / 'reviews' / commit / 'codex-runs'
                    for audit_path in audit_dir.glob('*/audit.json'):
                        if not (audit_path.parent / 'review.json').exists():
                            result.setdefault('review_attempt_usage', []).append(json.loads(audit_path.read_text()).get('usage'))
                    budget = json.loads((ROOT / 'config/rules.json').read_text())['budget']
                    result['aggregate_usage'] = aggregate_usage(result, entry.get('reviews', []), budget)
                finally:
                    env.reset_job(); save()
        summary['phase'] = 'assessment'
        for entry in entries:
            if 'trace' not in entry: continue
            directory = entry['directory']; result = entry['result']
            if not entry['prediction']:
                result['evaluation_state'] = 'agent_execution_failed' if result.get('agent_error') else 'no_prediction'; continue
            reviews = entry.get('reviews', []); write_json(directory / 'reviews.json', reviews)
            if hashlib.sha256((directory / 'report.json').read_bytes()).hexdigest() != result['report_sha256']:
                raise RuntimeError('Frozen prediction changed during retrospective review')
            evaluation = tool('evaluate', ['-repo', str(repo), '-trace', str(directory / 'trace.json'), '-prediction', str(directory / 'report.json'),
                                          '-reviews', str(directory / 'reviews.json'), '-source-key', result['source_key'], '-packets', str(directory / 'reviews')])
            write_json(directory / 'evaluation.json', evaluation); result['evaluation_state'] = evaluation['state']
            result['assessment'] = {k: evaluation[k] for k in ('correct_findings', 'incorrect_findings', 'unknown_findings', 'pending_candidates')}
            write_json(directory / 'result.json', result)
        save()
    needs_runtime = not args.track_only and (args.review_fixes or any(j['resolved']['ticket']['source_key'] not in predictions for j in prepared))
    try:
        if needs_runtime and prepared:
            with Environment(args.cache, args.context) as env:
                registry = initial(env); write_json(output / 'issues.json', registry)
                write_json(output / 'issue-history.json', history)
                evaluation(env, registry)
            summary['cleanup'] = 'completed'
        else:
            registry = initial(None); write_json(output / 'issues.json', registry)
            write_json(output / 'issue-history.json', history)
            evaluation(None, registry); summary['cleanup'] = 'not_required'
        summary['phase'] = 'completed'
    finally:
        if env and not env.started and env.directory and not env.directory.exists(): summary['cleanup'] = 'completed'
        save()
    return 1 if summary['deferred'] or any(r.get('error') or r.get('agent_error') or r.get('evaluation_error') or r.get('review_errors') for r in summary['results']) else 0
