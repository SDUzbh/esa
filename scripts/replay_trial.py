#!/usr/bin/env python3
"""Compile a frozen client revision, capture replay evidence, then evaluate separately.

This is deterministic test orchestration, not the ESA business backend. It uses
only the Python standard library and never modifies the preserved Jira or Git data.
"""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess

from replay_environment import ROOT, Environment, dependencies, digest, write_json as write
BUNDLE = ROOT / '.esa/replays/KAFKA-2880/trial-1'
FUTURE = ROOT / '.esa/evaluation/KAFKA-2880'

def command(args, **kwargs):
    return subprocess.run(args, check=True, text=True, capture_output=True, **kwargs).stdout.strip()

def compile_revision(directory):
    source = directory / 'source'
    hashes = {str(p.relative_to(source)): digest(p) for p in source.rglob('*') if p.is_file()}
    inputs = {'deps/' + p.name: p for p in (directory / 'deps').glob('*.jar')}
    inputs.update({'MetadataDisconnectReplay.java': ROOT / 'examples/replay/MetadataDisconnectReplay.java',
                   'compile.sh': ROOT / 'examples/replay/compile.sh'})
    # One shared runtime implementation; no per-ticket Docker image build.
    with Environment() as env:
        delta = env.sync(source, hashes, inputs)
        run = env.run('sh /input/compile.sh')
        (directory / 'reproduction.log').write_text(run.stdout + run.stderr)
        result = {'jdk_image': env.jdk_digest, 'image_id': env.command(['image', 'inspect', env.jdk_digest, '--format', '{{.Id}}']),
                  'environment_delta': delta, 'exit_code': run.returncode,
                  'client_compilation_passed': 'CLIENT_MODULE_COMPILATION=PASS' in run.stdout,
                  'successful_control_passed': 'SUCCESSFUL_METADATA_CONTROL=PASS' in run.stdout,
                  'null_pointer_reproduced': 'DISCONNECTED_METADATA_RESULT=java.lang.NullPointerException' in run.stdout,
                  'timeout_regression_passed': 'FIXED_REGRESSION=PASS' in run.stdout}
    # Preserve the exact harness for the before/after identity check.
    shutil.copyfile(ROOT / 'examples/replay/MetadataDisconnectReplay.java', directory / 'MetadataDisconnectReplay.java')
    write(directory / 'reproduction.json', result)
    return result


def prepare():
    manifest = json.loads((BUNDLE / 'manifest.json').read_text())
    deps = dependencies(BUNDLE / 'source/build.gradle', ROOT / '.esa/replay-cache')
    (BUNDLE / 'deps').mkdir(exist_ok=True)
    for name, item in deps.items(): shutil.copyfile(item['path'], BUNDLE / 'deps' / name)
    write(BUNDLE / 'dependency-lock.json', {name: {k: v for k, v in item.items() if k != 'path'} for name, item in deps.items()})
    result = compile_revision(BUNDLE)
    if not (result['client_compilation_passed'] and result['successful_control_passed'] and result['null_pointer_reproduced'] and result['exit_code'] == 20):
        raise RuntimeError('Historical replay did not reproduce the expected failure; inspect reproduction.log')
    # These windows follow the reported stack and the client request lifecycle.
    # They contain no post-cutoff code, review discussion, patch or fix metadata.
    windows = [
        ('clients/src/main/java/org/apache/kafka/clients/consumer/internals/Fetcher.java', 185, 234),
        ('clients/src/main/java/org/apache/kafka/clients/consumer/internals/ConsumerNetworkClient.java', 344, 352),
        ('clients/src/main/java/org/apache/kafka/clients/ClientResponse.java', 25, 114),
        ('clients/src/main/java/org/apache/kafka/clients/NetworkClient.java', 398, 417),
        ('clients/src/main/java/org/apache/kafka/common/requests/MetadataResponse.java', 125, 145),
    ]
    documents = []
    for path, first, last in windows:
        text = (BUNDLE / 'source' / path).read_text()
        expected = manifest['files'][path]
        if hashlib.sha256(text.encode()).hexdigest() != expected: raise RuntimeError('Source snapshot changed')
        lines = text.splitlines()
        documents.append({'source': 'git', 'reference_prefix': manifest['source_commit'] + ':' + path + ':', 'first_line': first, 'lines': lines[first-1:last]})
    log = (BUNDLE / 'reproduction.log').read_text()
    documents.append({'source': 'replay', 'reference_prefix': 'reproduction.log:', 'first_line': 1, 'lines': log.splitlines()})
    ticket = json.loads((BUNDLE / 'ticket.json').read_text())
    packet = {'cutoff': manifest['cutoff'], 'source_commit': manifest['source_commit'], 'expected_ticket': ticket, 'documents': documents, 'limitations': manifest['audit']['limitations'] + ['Replay uses a deterministic disconnected ClientResponse from the historical shipped MockClient, with an independent successful-response control.', 'The full clients Java module was compiled; a broker cluster and Kafka Connect process were not started.', 'A frozen historical input cannot eliminate knowledge the model may already have learned during training.']}
    write(BUNDLE / 'evidence.json', packet)
    print(json.dumps({'stage': 'historical_replay', **result, 'evidence': str(BUNDLE / 'evidence.json')}, indent=2))

def evaluate(report_path):
    # Evaluation is deliberately a separate invocation after the diagnosis is frozen.
    report_path = Path(report_path).resolve()
    if not report_path.exists(): raise RuntimeError('Freeze a completed Agent report before evaluation')
    diagnosis_bytes = report_path.read_bytes()
    diagnosis = json.loads(diagnosis_bytes)
    FUTURE.mkdir(parents=True, exist_ok=True)
    (FUTURE / 'frozen-report.json').write_bytes(diagnosis_bytes)
    # Choose the earliest merged source revision that mentions this Jira key.
    entries = command(['git', '-C', str(ROOT / 'kafka'), 'log', '--all', '--format=%ct %H', '--grep=KAFKA-2880']).splitlines()
    commit = sorted(entries)[0].split()[1]
    stamp = command(['git', '-C', str(ROOT / 'kafka'), 'show', '-s', '--format=%cI', commit])
    patch = command(['git', '-C', str(ROOT / 'kafka'), 'show', '--format=fuller', commit, '--', 'clients/src/main/java/org/apache/kafka/clients/consumer/internals/Fetcher.java', 'clients/src/main/java/org/apache/kafka/clients/consumer/internals/ConsumerNetworkClient.java'])
    (FUTURE / 'upstream-fix.patch').write_text(patch + '\n')
    # Export this revision into a separate evaluation directory, never the Agent bundle.
    import io, tarfile
    archive = subprocess.run(['git', '-C', str(ROOT / 'kafka'), 'archive', commit, 'clients/src/main/java', 'clients/src/test/java/org/apache/kafka/clients/MockClient.java'], check=True, capture_output=True).stdout
    with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
        for item in tar:
            if item.isdir(): continue
            if not item.isfile() or item.name.startswith('/') or '..' in Path(item.name).parts: raise RuntimeError('Unsafe archive')
            target = FUTURE / 'source' / item.name; target.parent.mkdir(parents=True, exist_ok=True); target.write_bytes(tar.extractfile(item).read())
    # Build each revision with its own historical dependency versions. The source
    # and test are unchanged; a later dependency upgrade must not masquerade as a
    # missing-symbol regression or be exposed to the historical Agent input.
    gradle=command(['git','-C',str(ROOT/'kafka'),'show',commit+':build.gradle'])
    historical_build = FUTURE / 'build.gradle'
    historical_build.write_text(gradle)
    resolved = dependencies(historical_build, ROOT / '.esa/replay-cache')
    deps = FUTURE / 'deps'; deps.mkdir(exist_ok=True)
    for path in deps.glob('*.jar'):
        if path.name not in resolved: path.unlink()  # Derived evaluation files only.
    for name, item in resolved.items(): shutil.copyfile(item['path'], deps / name)
    write(FUTURE / 'dependency-lock.json', {name: {k: v for k, v in item.items() if k != 'path'} for name, item in resolved.items()})
    result = compile_revision(FUTURE)
    if not (result['client_compilation_passed'] and result['successful_control_passed'] and result['timeout_regression_passed'] and result['exit_code'] == 0):
        raise RuntimeError('Upstream regression check failed; inspect evaluation reproduction.log')
    prose = '\n'.join([diagnosis['summary']] + diagnosis['root_cause_candidates']).lower()
    recommendations = '\n'.join(diagnosis['recommendations']).lower()
    checks = {
        'root_cause_describes_disconnected_response': 'disconnect' in prose and ('null' in prose or 'responsebody' in prose),
        'locates_consumer_network_completion': any('ConsumerNetworkClient.java:' in x['reference'] for x in diagnosis['evidence']),
        'locates_fetcher_metadata_path': any('Fetcher.java:' in x['reference'] for x in diagnosis['evidence']),
        'recommends_disconnect_failure_and_retry': 'disconnect' in recommendations and ('fail' in recommendations or 'exception' in recommendations) and 'retry' in recommendations,
        'mentions_timeout_verification': 'timeout' in '\n'.join(diagnosis['verification_steps']).lower(),
        'explicitly_recommends_timeout_exception': 'timeoutexception' in recommendations or ('throw' in recommendations and 'timeout' in recommendations),
        'historical_failure_and_fixed_pass': result['timeout_regression_passed'],
    }
    evaluation = {'source_key': 'KAFKA-2880', 'report_sha256': hashlib.sha256(diagnosis_bytes).hexdigest(), 'ground_truth_commit': commit, 'ground_truth_committed_at': stamp, 'fixed_replay': result, 'diagnostic_checks': checks, 'review': 'The diagnosis identifies the disconnect-to-successful-future defect and matches the upstream completion-handler correction. It mentions bounded timeout verification but does not explicitly recommend the upstream change to throw TimeoutException instead of returning empty metadata.', 'limitations': ['One selected, stack-rich public bug; not an accuracy estimate for the Jira archive.', 'Source windows were selected by the operator from the reported stack and request lifecycle; autonomous source search was not evaluated.', 'Keyword checks are transparent diagnostic assertions, not a semantic accuracy score.', 'Historical input isolation cannot exclude pretrained model knowledge.']}
    write(ROOT / 'reports/KAFKA-2880-evaluation.json', evaluation)
    print(json.dumps(evaluation, indent=2))

if __name__ == '__main__':
    parser = argparse.ArgumentParser(); parser.add_argument('stage', choices=['prepare', 'evaluate']); parser.add_argument('--report')
    args = parser.parse_args()
    if args.stage == 'prepare': prepare()
    else: evaluate(args.report)
