#!/usr/bin/env python3
"""Keep an owned replay runtime alive for one Codex request; EOF cleans it up."""
import argparse
import json
from pathlib import Path
import sys
from replay_environment import Environment, ROOT, dependencies, write_json


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--evidence', required=True)
    parser.add_argument('--run-dir', required=True)
    args = parser.parse_args()
    packet_path = Path(args.evidence).resolve(); bundle = packet_path.parent
    packet = json.loads(packet_path.read_text()); manifest = json.loads((bundle / 'manifest.json').read_text())
    if packet['source_commit'] != manifest['source_commit']: raise RuntimeError('Packet/source registration mismatch')
    directory = Path(args.run_dir).resolve()
    with Environment() as env:
        deps = dependencies(bundle / 'source/build.gradle', env.cache)
        inputs = {'deps/' + name: item['path'] for name, item in deps.items()}
        inputs.update({'compile.sh': ROOT / 'examples/replay/compile.sh',
                       'MetadataDisconnectReplay.java': ROOT / 'examples/replay/MetadataDisconnectReplay.java'})
        installer = env.directory / 'install.sh'
        installer.write_text('set -eu\nmkdir -p /workspace/lib\ncp /input/deps/*.jar /workspace/lib/\njava -version\n')
        inputs['install.sh'] = installer
        delta = env.sync(bundle / 'source', manifest['files'], inputs)
        # Warm the deterministic fixture once. The agent can rerun or debug it.
        baseline = env.run('sh /input/compile.sh')
        env.freeze_build()
        (directory / 'environment.log').write_text(baseline.stdout + baseline.stderr)
        write_json(directory / 'environment.json', {'runtime_image': env.jdk_digest, 'delta': delta, 'baseline_exit': baseline.returncode})
        config_path = directory / 'tools-config.json'
        write_json(config_path, env.tool_config(packet['source_commit'], directory / 'tools.jsonl'))
        print(json.dumps({'tools_config': str(config_path)}), flush=True)
        # The Go adapter closes this pipe in a defer on success, failure or cancellation.
        sys.stdin.read()
    write_json(directory / 'environment-cleanup.json', {'state':'completed'})


if __name__ == '__main__':
    try: main()
    except KeyboardInterrupt: raise SystemExit(130)
