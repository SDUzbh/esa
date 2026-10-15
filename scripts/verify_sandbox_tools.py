#!/usr/bin/env python3
"""Exercise approved temporary installation against an owned offline sandbox."""
import json
from pathlib import Path
import subprocess
import tempfile

from replay_environment import Environment, digest, write_json

ROOT = Path(__file__).resolve().parents[1]

def main():
    with tempfile.TemporaryDirectory(prefix='esa-tool-test-') as temporary:
        directory = Path(temporary)
        source = directory / 'source'
        source.mkdir()
        code = source / 'Example.java'
        code.write_text('assignment.size();\n')
        tool = directory / 'tool.sh'
        tool.write_text('#!/bin/sh\nprintf diagnostic-tool-v1\\n\n')
        with Environment() as environment:
            environment.sync(source, {'Example.java': digest(code)}, {})
            config = environment.tool_config('fixture', directory / 'audit.jsonl')
            config.update(profiles_root=str(ROOT / 'profiles'), allowed_profiles=['general'], available_capabilities=['source', 'runtime', 'temporary_tools'], tools={'diagnostic': {'version': '1', 'file': str(tool), 'sha256': digest(tool)}})
            path = directory / 'tools.json'
            write_json(path, config)
            process = subprocess.Popen([str(ROOT / 'bin/esa-codex'), '-tool-server', '-tools-config', str(path)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            counter = 0

            def call(name, arguments):
                nonlocal counter
                counter += 1
                process.stdin.write(json.dumps({'jsonrpc': '2.0', 'id': counter, 'method': 'tools/call', 'params': {'name': name, 'arguments': arguments}}) + '\n')
                process.stdin.flush()
                response = json.loads(process.stdout.readline())['result']
                if response['isError']:
                    raise RuntimeError(response['content'][0]['text'])
                return json.loads(response['content'][0]['text'])

            try:
                grant = call('capability_request', {'profile': 'general', 'version': '2', 'requested_capabilities': ['runtime', 'temporary_tools'], 'hypothesis_id': 'H1', 'reason': 'Inspect a diagnostic trace', 'expected_observation': 'Run a pinned auxiliary diagnostic tool'})
                assert grant['decision']['status'] == 'granted'
                args = {'tool_id': 'diagnostic', 'version': '1', 'reason': 'Collect trace', 'hypothesis_id': 'H1', 'expected_observation': 'Fixture trace'}
                first = call('tool_install', args)
                assert first['status'] == 'installed' and not first['cache_hit']
                again = call('tool_install', args)
                assert again['cache_hit'], 'tool reinstall was not avoided'
                result = call('run', {'script': 'sh ' + first['path'], 'timeout_seconds': 5})
                assert 'diagnostic-tool-v1' in '\n'.join(line['text'] for line in result['evidence']['lines']), result
                denied = call('tool_install', dict(args, version='future'))
                assert denied['status'] == 'denied'
                assert environment.run('touch /input/source/Example.java').returncode != 0, 'product source became writable'
                assert digest(code) == config['files']['Example.java']
                print(json.dumps({'status': 'passed', 'checks': ['owned_offline_sandbox', 'approved_install', 'pinned_hash', 'tool_cache', 'policy_denial', 'immutable_source', 'cleanup']}))
            finally:
                process.stdin.close()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()

if __name__ == '__main__':
    main()
