#!/usr/bin/env python3
"""Standalone runtime protocol: prepare, observe, reset, close. No SUT logic."""
import argparse
import json
from pathlib import Path
import sys

from runtime_environment import Environment, dependencies, cleanup_sessions

ROOT = Path(__file__).resolve().parent


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--context', default='colima-redmine')
    parser.add_argument('--cache')
    parser.add_argument('--cleanup', action='store_true')
    args = parser.parse_args()
    if args.cleanup:
        print(json.dumps(cleanup_sessions())); return
    env = None
    try:
        for line in sys.stdin:
            request = json.loads(line)
            try:
                action = request['action']
                if action == 'prepare':
                    if env is None:
                        env = Environment(args.cache, args.context)
                        env.__enter__()
                    source = Path(request['source_dir'])
                    profile = request['profile']
                    inputs = {}
                    installer = env.directory / 'install.sh'
                    if profile == 'metadata-disconnect':
                        resolved = dependencies(source / 'build.gradle', env.cache)
                        inputs.update({'deps/' + name: item['path'] for name, item in resolved.items()})
                        inputs['MetadataDisconnectObservation.java'] = ROOT / 'assets/MetadataDisconnectObservation.java'
                        inputs['observe.sh'] = ROOT / 'assets/observe.sh'
                        installer.write_text('set -eu\nmkdir -p /workspace/lib\ncp /input/deps/*.jar /workspace/lib/\njava -version\n')
                    elif profile == 'command':
                        installer.write_text('set -eu\njava -version\n')
                    else:
                        raise ValueError('Register metadata-disconnect or command; environment readiness is not a bug replay')
                    inputs['install.sh'] = installer
                    hashes = request['files']
                    if profile == 'metadata-disconnect':
                        hashes = {k: v for k, v in hashes.items() if k.startswith('clients/src/main/java/') or
                                  k == 'clients/src/test/java/org/apache/kafka/clients/MockClient.java' or k in ('build.gradle', 'LICENSE', 'NOTICE')}
                    delta = env.sync(source, hashes, inputs)
                    result = {'ready': True, 'runtime_image': env.jdk_digest, 'environment_delta': delta}
                elif action == 'observe':
                    if env is None: raise ValueError('Prepare before observe')
                    command = request.get('command') or 'set -e; sh /input/install.sh; sh /input/observe.sh'
                    run = env.run(command, request.get('timeout_seconds', 180))
                    env.freeze_build()
                    result = {'exit_code': run.returncode, 'stdout': run.stdout, 'stderr': run.stderr,
                              'log': run.stdout + run.stderr, 'runtime_image': env.jdk_digest}
                elif action == 'reset':
                    if env is not None: env.reset_job()
                    result = {'reset': True}
                elif action == 'close':
                    if env is not None: env.close(); env = None
                    result = {'cleanup': 'completed'}
                else: raise ValueError('Unknown runtime action')
                print(json.dumps({'ok': True, 'result': result}), flush=True)
                if action == 'close': break
            except Exception as error:
                print(json.dumps({'ok': False, 'error': str(error)}), flush=True)
    finally:
        if env is not None: env.close()


if __name__ == '__main__':
    try: main()
    except KeyboardInterrupt: raise SystemExit(130)
