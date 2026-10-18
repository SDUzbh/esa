#!/usr/bin/env python3
"""Run a provider-mode Agent evaluation set against the real Codex provider.

Starts bin/esa-codex with the set's release registry, the given tool catalog
and the Issue directory the manifest's ESA uses, then runs system-replay on
the manifest and stops the provider. A fresh analyzer token lives only in
the two processes' environments. ESA_DATABASE_URL must be set.

THIS MAKES REAL MODEL CALLS FOR EVERY TICKET. Build the set with
scripts/build_eval_manifest.py --mode provider. To keep a long run alive
after the terminal closes, start it with nohup. SIGTERM or Ctrl-C stops the
replay after cleanup, then the provider.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parent.parent


def listening(address):
    host, port = address.rsplit(':', 1)
    try:
        with socket.create_connection((host, int(port)), timeout=1):
            return True
    except OSError:
        return False


def stop(process, sig, seconds):
    if process.poll() is None:
        process.send_signal(sig)
        try:
            process.wait(timeout=seconds)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--set', required=True, help='Directory written by build_eval_manifest.py --mode provider')
    parser.add_argument('--manifest', default='manifest.json', help='Manifest file inside --set, for example a resumed subset')
    parser.add_argument('--output', required=True, help='New directory for the replay, provider audit and logs')
    catalog = parser.add_mutually_exclusive_group(required=True)
    catalog.add_argument('--tool-catalog', help='Pinned dependency catalog for the provider (esa-codex -tool-catalog)')
    catalog.add_argument('--no-tool-catalog', action='store_true', help='Run without one: the Agent cannot install build dependencies')
    parser.add_argument('--codex', default='codex', help='Codex CLI executable')
    parser.add_argument('--listen', default='127.0.0.1:8091', help='Provider address')
    parser.add_argument('--concurrency', default='1', help='Provider engineering concurrency (1..3)')
    parser.add_argument('--bin', default=str(ROOT / 'bin'), help='Directory with esa-codex and system-replay')
    args = parser.parse_args()
    if not os.environ.get('ESA_DATABASE_URL'):
        parser.error('ESA_DATABASE_URL must point at the Postgres the managed ESA uses')
    if args.tool_catalog and not Path(args.tool_catalog).is_file():
        parser.error('tool catalog not found: ' + args.tool_catalog)
    sets = Path(args.set).resolve()
    manifest = json.loads((sets / args.manifest).read_text())
    start = manifest['target']['start']
    env_keys = set(start.get('env', {}))
    if 'ESA_ANALYZER_URL' in env_keys or 'ESA_ANALYZER_TOKEN' in env_keys:
        parser.error('not a provider-mode set: its manifest configures the analyzer itself')
    command = start['args']
    if '-issues-directory' not in command:
        parser.error('not a provider-mode set: the manifest has no -issues-directory')
    issues = Path(command[command.index('-issues-directory') + 1])
    if listening(args.listen):
        parser.error(args.listen + ' is already in use; stop the other provider first')
    out = Path(args.output).resolve()
    out.mkdir(parents=True)
    os.chmod(out, 0o700)
    issues.mkdir(parents=True, exist_ok=True)
    token = secrets.token_hex(24)
    env = dict(os.environ, ESA_ANALYZER_TOKEN=token)
    provider_args = [str(Path(args.bin) / 'esa-codex'), '-codex', args.codex, '-registry', str(sets / 'releases.json'),
                     '-profiles', str(ROOT / 'profiles'), '-issues-directory', str(issues), '-runs', str(out / 'codex-runs'),
                     '-listen', args.listen, '-concurrency', args.concurrency]
    if args.tool_catalog:
        provider_args += ['-tool-catalog', str(Path(args.tool_catalog).resolve())]
    record = {'set': str(sets), 'manifest': args.manifest, 'tool_catalog': args.tool_catalog and str(Path(args.tool_catalog).resolve()),
              'codex': args.codex, 'listen': args.listen, 'concurrency': args.concurrency, 'started_at': time.strftime('%Y-%m-%dT%H:%M:%S%z')}
    (out / 'launcher.json').write_text(json.dumps(record, indent=2) + '\n')
    with open(out / 'provider.log', 'ab') as plog, open(out / 'replay.log', 'ab') as rlog:
        provider = subprocess.Popen(provider_args, cwd=ROOT, env=env, stdout=plog, stderr=subprocess.STDOUT)
        replay = None
        stopping = []

        def interrupt(*_):
            stopping.append(True)
            if replay is not None and replay.poll() is None:
                replay.send_signal(signal.SIGTERM)
        signal.signal(signal.SIGTERM, interrupt)
        signal.signal(signal.SIGINT, interrupt)
        try:
            deadline = time.monotonic() + 120
            while not listening(args.listen):
                if provider.poll() is not None or time.monotonic() > deadline or stopping:
                    print('provider did not start; see ' + str(out / 'provider.log'), file=sys.stderr)
                    return 1
                time.sleep(0.5)
            replay_env = dict(env, ESA_ANALYZER_URL='http://%s/analyze' % args.listen)
            replay = subprocess.Popen([str(Path(args.bin) / 'system-replay'), '-pipeline', '-order', 'time', '-manifest', str(sets / args.manifest),
                                       '-output', str(out / 'run')], cwd=ROOT, env=replay_env, stdout=rlog, stderr=subprocess.STDOUT)
            if stopping:
                replay.send_signal(signal.SIGTERM)
            code = replay.wait()
        finally:
            if replay is not None:
                stop(replay, signal.SIGTERM, 120)
            # Ctrl-C is the provider's owned cleanup.
            stop(provider, signal.SIGINT, 120)
    record.update(finished_at=time.strftime('%Y-%m-%dT%H:%M:%S%z'), replay_exit=code, stopped=bool(stopping))
    (out / 'launcher.json').write_text(json.dumps(record, indent=2) + '\n')
    print(json.dumps({'run': str(out / 'run' / 'pipeline.json'), 'replay_exit': code, 'stopped': bool(stopping)}))
    return code


if __name__ == '__main__':
    sys.exit(main())
