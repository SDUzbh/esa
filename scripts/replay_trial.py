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

def evaluate(*args, **kwargs):
    raise RuntimeError('Imported Agent reports are no longer a system replay test. Use scripts/replay_pipeline.py --manifest system_replay/examples/esa-api-kafka.json --pipeline with an isolated target.')

if __name__ == '__main__':
    parser = argparse.ArgumentParser(); parser.add_argument('stage', choices=['prepare', 'evaluate']); parser.add_argument('--report'); parser.add_argument('--versions', type=int, default=5); parser.add_argument('--review-fixes', action='store_true')
    args = parser.parse_args()
    if args.stage == 'prepare': prepare()
    else: evaluate(args.report, args.versions, args.review_fixes)
