#!/usr/bin/env python3
"""Plan and run historical tickets in version order with deterministic environment reuse."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import sys
import uuid

from replay_environment import ROOT, Environment, cleanup_sessions, dependencies, kafka_binary, write_json


def version_key(value):
    match = re.fullmatch(r'(\d+(?:\.\d+){1,3})(?:[-.]?(.*))?', value)
    if not match: raise ValueError('An explicit numeric version label is required: ' + value)
    numbers = tuple(int(n) for n in match.group(1).split('.'))
    suffix = match.group(2) or ''
    return numbers + (0,) * (4 - len(numbers)), 0 if suffix else 1, tuple(int(x) if x.isdigit() else x.lower() for x in re.split(r'(\d+)', suffix))


def build_tools():
    go = ROOT / '.tools/go/bin/go'
    if not go.exists(): go = 'go'
    import os
    env = dict(os.environ, GOCACHE=str(ROOT / '.tools/go-cache'), GOTOOLCHAIN='local')
    for name in ('esa-replay', 'esa-codex'):
        subprocess.run([str(go), 'build', '-trimpath', '-o', str(ROOT / 'bin' / name), './cmd/' + name], cwd=ROOT, env=env, check=True)


def plan(job, repo):
    raw = Path(job['raw'])
    if not raw.is_absolute(): raw = ROOT / raw
    args = [str(ROOT / 'bin/esa-replay'), '-plan', '-raw', str(raw), '-repo', str(repo)]
    for key, flag in [('ref', '-ref'), ('at', '-at'), ('project', '-project')]:
        if job.get(key): args += [flag, str(job[key])]
    if job.get('environment'): args += ['-environment', json.dumps(job['environment'])]
    result = subprocess.run(args, text=True, capture_output=True)
    if result.returncode: raise ValueError(result.stderr.strip())
    result = json.loads(result.stdout)
    version = job.get('version') or result['source_ref']
    # A development commit needs an operator-provided version label for ordering.
    version_key(version)
    return dict(job, raw=str(raw), resolved=result, version=version)


def source_snapshot(job, repo, cache):
    p = job['resolved']; scope = 'clients' if job['profile'] == 'metadata-disconnect' else 'full'
    directory = cache / 'source' / (p['source_commit'] + '-' + scope)
    marker = directory / 'manifest.json'
    if not marker.exists():
        args = [str(ROOT / 'bin/esa-replay'), '-raw', job['raw'], '-repo', str(repo), '-ref', p['source_commit'],
                '-at', p['cutoff'], '-out', str(directory), '-scope', scope, '-project', p['ticket']['project_id']]
        subprocess.run(args, check=True, text=True, capture_output=True)
    manifest = json.loads(marker.read_text())
    if manifest['source_commit'] != p['source_commit'] or manifest['scope'] != scope: raise RuntimeError('Source cache registration mismatch')
    return directory, manifest


def runtime_inputs(job, snapshot, env):
    inputs = {}
    if job['profile'] == 'metadata-disconnect':
        if job['version'] not in ('0.9.0.0', '0.9.0.1'):
            raise RuntimeError('The metadata harness is registered only for 0.9.0.0 and 0.9.0.1')
        deps = dependencies(snapshot / 'source/build.gradle', env.cache)
        inputs.update({'deps/' + name: item['path'] for name, item in deps.items()})
        inputs['MetadataDisconnectReplay.java'] = ROOT / 'examples/replay/MetadataDisconnectReplay.java'
        inputs['compile.sh'] = ROOT / 'examples/replay/compile.sh'
    if job['profile'] == 'kafka-server':
        product = kafka_binary(job['version'], job.get('scala_version', '2.11'), env.cache)
        inputs.update({'product/' + str(path.relative_to(product)): path for path in product.rglob('*') if path.is_file()})
        inputs['start-kafka.sh'] = ROOT / 'examples/replay/start-kafka.sh'
    installer = env.directory / 'install.sh'
    installer.write_text('set -eu\nmkdir -p /workspace/lib\n' +
                         ('cp /input/deps/*.jar /workspace/lib/\n' if job['profile'] == 'metadata-disconnect' else '') +
                         ('mkdir -p /workspace/config\ncp /input/product/config/*.properties /workspace/config/\n' if job['profile'] == 'kafka-server' else '') +
                         'java -version\nprintf "ENVIRONMENT_INSTALLED=PASS\\n"\n')
    inputs['install.sh'] = installer
    return inputs


def run_job(job, env, repo, output, agent):
    p = job['resolved']; key = p['ticket']['source_key']
    safe_key = re.sub(r'[^A-Za-z0-9_-]', '_', key)
    directory = output / (safe_key + '-' + uuid.uuid4().hex[:8]); directory.mkdir(parents=True, exist_ok=False)
    write_json(directory / 'plan.json', p)
    snapshot, manifest = source_snapshot(job, repo, env.cache)
    delta = env.sync(snapshot / 'source', manifest['files'], runtime_inputs(job, snapshot, env))
    command = job.get('command')
    if not command:
        command = 'set -e; sh /input/install.sh; ' + {'metadata-disconnect':'sh /input/compile.sh', 'kafka-server':'sh /input/start-kafka.sh', 'source':'java -version'}[job['profile']]
    run = env.run(command, job.get('command_timeout', 180))
    log = run.stdout + run.stderr; (directory / 'reproduction.log').write_text(log)
    env.freeze_build()
    result = {'source_key': key, 'version': job['version'], 'source_commit': p['source_commit'],
              'profile': job['profile'], 'runtime_image': env.jdk_digest, 'container': env.name,
              'environment_delta': delta, 'exit_code': run.returncode,
              'client_compilation_passed': 'CLIENT_MODULE_COMPILATION=PASS' in log,
              'null_pointer_reproduced': 'DISCONNECTED_METADATA_RESULT=java.lang.NullPointerException' in log,
              'fixed_regression_passed': 'FIXED_REGRESSION=PASS' in log,
              'server_ready': 'KAFKA_SERVER_READY=PASS' in log,
              'scope': 'Registered metadata request replay' if job['profile']=='metadata-disconnect' else 'Environment readiness only; no issue reproduction is claimed'}
    expected_failure = (job['profile'] == 'metadata-disconnect' and run.returncode == 20 and
                        result['client_compilation_passed'] and result['null_pointer_reproduced'] and
                        'SUCCESSFUL_METADATA_CONTROL=PASS' in log)
    if run.returncode != 0 and not expected_failure:
        result['error'] = 'Registered environment command failed; inspect reproduction.log'
    packet = {'cutoff': p['cutoff'], 'source_commit': p['source_commit'], 'expected_ticket': p['ticket'],
              'documents': [{'source': 'replay', 'reference_prefix': 'reproduction.log:', 'first_line': 1, 'lines': log.splitlines()}],
              'limitations': p['audit']['limitations'] + [result['scope'], 'Runtime is an explicit test profile, not an inferred customer environment.', 'Pretrained model knowledge cannot be excluded.']}
    write_json(directory / 'evidence.json', packet)
    config = env.tool_config(p['source_commit'], directory / 'tools.jsonl')
    write_json(directory / 'tools-config.json', config)
    if agent:
        args = [str(ROOT / 'bin/esa-codex'), '-once', '-evidence', str(directory / 'evidence.json'),
                '-tools-config', str(directory / 'tools-config.json'), '-runs', str(directory / 'codex-runs'),
                '-run-id', 'run_' + uuid.uuid4().hex]
        # Parent cancellation sends TERM to the provider; the environment manager
        # removes every process in the owned container before proceeding.
        # Persist warnings while the provider runs, without buffering stderr in memory.
        with (directory / 'provider.log').open('w') as provider_log:
            process = subprocess.Popen(args, cwd=ROOT, text=True, stdout=subprocess.PIPE, stderr=provider_log)
            try:
                stdout, _ = process.communicate()
            except BaseException:
                process.terminate()
                try: process.wait(timeout=45)
                except subprocess.TimeoutExpired: process.kill(); process.wait()
                raise
        stderr = (directory / 'provider.log').read_text()
        if process.returncode: result['agent_error'] = stderr.strip()
        else:
            report = json.loads(stdout); write_json(directory / 'report.json', report)
            result['usage'] = report['usage']; result['alerts'] = report.get('alerts', [])
            result['environment_requests'] = report.get('environment_requests', [])
            for alert in result['alerts']:
                print('WARNING ' + key + ': ' + alert['message'], file=sys.stderr, flush=True)
        # All CLI events, warnings and tool scripts are retained in the audit files.
    write_json(directory / 'result.json', result)
    print(json.dumps(result), flush=True)
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('tickets', nargs='*', help='Jira keys or preserved JSON paths')
    parser.add_argument('--pipeline', action='store_true', help='Sort by numeric version and reuse one environment')
    parser.add_argument('--manifest', help='JSON list of operator-defined jobs')
    parser.add_argument('--profile', choices=['source', 'metadata-disconnect', 'kafka-server'], default='source')
    parser.add_argument('--ref'); parser.add_argument('--at'); parser.add_argument('--version')
    parser.add_argument('--agent', action='store_true', help='Run Codex with only registered replay tools')
    parser.add_argument('--plan', action='store_true', help='Resolve and sort without Docker or model calls')
    parser.add_argument('--repo', default=str(ROOT / 'kafka'))
    parser.add_argument('--output', default=str(ROOT / '.esa/pipeline-runs' / uuid.uuid4().hex))
    parser.add_argument('--cache', default=str(ROOT / '.esa/replay-cache'))
    parser.add_argument('--context', default='colima-redmine')
    parser.add_argument('--build-tools', action='store_true', help='Build project-local Go commands first')
    parser.add_argument('--cleanup', action='store_true', help='Clean owned stale sessions left by an untrappable kill')
    args = parser.parse_args()
    if args.cleanup:
        print(json.dumps(cleanup_sessions(),indent=2));return 0
    if args.build_tools or not all((ROOT / 'bin' / name).exists() for name in ('esa-replay', 'esa-codex')): build_tools()
    jobs = json.loads(Path(args.manifest).read_text()) if args.manifest else [
        {'raw': str(ROOT / 'data/jira/raw' / (x + '.json')) if re.fullmatch(r'KAFKA-\d+', x) else x} for x in args.tickets]
    if not jobs: parser.error('Provide tickets or a job manifest')
    if not args.pipeline and len(jobs) != 1: parser.error('Multiple tickets require --pipeline')
    prepared = []; deferred = []
    for job in jobs:
        job = dict(job); job.setdefault('profile', args.profile)
        if job['profile'] not in ('source', 'metadata-disconnect', 'kafka-server'): parser.error('Unknown replay profile')
        for field in ('ref', 'at', 'version'):
            if getattr(args, field) and not job.get(field): job[field] = getattr(args, field)
        try: prepared.append(plan(job, Path(args.repo)))
        except ValueError as error: deferred.append({'raw': job['raw'], 'state': 'needs_clarification', 'reason': str(error)})
    if args.pipeline: prepared.sort(key=lambda x: (version_key(x['version']), x['resolved']['source_commit'], x['resolved']['ticket']['source_key']))
    output = Path(args.output).resolve(); output.mkdir(parents=True, exist_ok=False)
    summary = {'jobs': [{k: j[k] for k in ('raw','version','profile')} for j in prepared], 'deferred': deferred, 'results': [], 'cleanup': 'not_started'}
    write_json(output / 'pipeline.json', summary)
    if args.plan:
        print(json.dumps(summary, indent=2)); return 0
    env = None
    try:
        if prepared:
            with Environment(args.cache, args.context) as env:
                for job in prepared:
                    try:
                        summary['results'].append(run_job(job, env, Path(args.repo), output, args.agent))
                    except (RuntimeError, subprocess.SubprocessError, OSError, ValueError) as error:
                        summary['results'].append({'source_key': job['resolved']['ticket']['source_key'], 'state':'failed', 'error':str(error)})
                        print(str(error), file=sys.stderr, flush=True)
                    finally:
                        env.reset_job()
                        write_json(output / 'pipeline.json', summary)
            summary['cleanup'] = 'completed'
    finally:
        if env and not env.started and env.directory and not env.directory.exists(): summary['cleanup']='completed'
        elif summary['cleanup']=='not_started': summary['cleanup']='interrupted_or_setup_failed'
        write_json(output / 'pipeline.json', summary)
    print(json.dumps({'report':str(output/'pipeline.json'),'cleanup':summary['cleanup'],'deferred':len(deferred)}))
    return 1 if deferred or any('error' in r or 'agent_error' in r for r in summary['results']) else 0


if __name__ == '__main__':
    try: raise SystemExit(main())
    except KeyboardInterrupt: print('Replay interrupted; owned runtime resources were cleaned.', file=sys.stderr); raise SystemExit(130)
