#!/usr/bin/env python3
"""Keep an owned replay runtime alive for one Codex request; EOF cleans it up."""
import argparse
import json
from pathlib import Path
import sys
from replay_environment import Environment, ROOT, dependencies, write_json, remove_owned

def baseline_status(exit_code, output):
    """Separate compilation/control readiness from the registered failure or healthy outcome."""
    compiled = 'CLIENT_MODULE_COMPILATION=PASS' in output
    control = 'SUCCESSFUL_METADATA_CONTROL=PASS' in output
    scenario = 'unassessed'
    if compiled and control:
        if exit_code == 0 and 'FIXED_REGRESSION=PASS' in output:
            scenario = 'healthy_registered_scenario'
        elif exit_code == 20 and 'DISCONNECTED_METADATA_RESULT=java.lang.NullPointerException' in output:
            scenario = 'registered_symptom_reproduced'
        else:
            scenario = 'scenario_failed'
    return {'status': 'baseline_ready' if compiled and control else 'baseline_failed', 'compiled': compiled, 'control_passed': control, 'scenario_status': scenario, 'baseline_exit': exit_code}


def main():
    """Register frozen source and lazily provision one owned environment.

    Workspace mode supplies no build or reproducer. Scenario mode compiles the
    registered baseline. Closing stdin exits the context and removes owned resources."""
    parser = argparse.ArgumentParser()
    parser.add_argument('--evidence')
    parser.add_argument('--cleanup-owned', action='store_true')
    parser.add_argument('--run-dir', required=True)
    parser.add_argument('--profile', default='java-debug', help='Prebuilt environment for the registered Java scenario')
    parser.add_argument('--workspace', action='store_true', help='Prepare an empty tool workspace from a source manifest, without a product build or reproducer')
    args = parser.parse_args()
    directory = Path(args.run_dir).resolve()
    if args.cleanup_owned:
        marker = directory / 'runtime-owned.json'
        if marker.exists():
            saved = json.loads(marker.read_text())
            docker = ['docker'] + (['--context', saved['context']] if saved['context'] else [])
            for kind, key in [('container','container'),('container','writer'),('volume','volume')]:
                remove_owned(docker, kind, saved[key], saved['session'])
        return
    if not args.evidence:
        raise ValueError('A registered evidence packet is required')
    packet_path = Path(args.evidence).resolve(); bundle = packet_path.parent
    packet = json.loads(packet_path.read_text()); manifest = json.loads((bundle / 'manifest.json').read_text())
    if packet['source_commit'] != manifest['source_commit']: raise RuntimeError('Packet/source registration mismatch')
    directory = Path(args.run_dir).resolve()
    with Environment(profile=args.profile) as env:
        write_json(directory / 'runtime-owned.json', {'container': env.name, 'writer': env.writer, 'volume': env.volume, 'session': env.session, 'context': env.context})
        inputs = {}
        if not args.workspace:
            deps = dependencies(bundle / 'source/build.gradle', env.cache)
            inputs = {'deps/' + name: item['path'] for name, item in deps.items()}
            inputs.update({'compile.sh': ROOT / 'examples/replay/compile.sh',
                           'MetadataDisconnectReplay.java': ROOT / 'examples/replay/MetadataDisconnectReplay.java'})
        installer = env.directory / 'install.sh'
        installer.write_text('set -eu\nmkdir -p /workspace/lib\n' + ('' if args.workspace else 'cp /input/deps/*.jar /workspace/lib/\n') + 'java -version\njavac -version\n')
        inputs['install.sh'] = installer
        delta = env.sync(bundle / 'source', manifest['files'], inputs)
        if args.workspace:
            readiness = {'status': 'workspace_ready', 'compiled': False, 'control_passed': False, 'scenario_status': 'unassessed',
                         'instructions': 'No prebuilt product or reproducer is supplied. Build unchanged /input/source into /workspace/classes. Install required pinned artifacts through tool_install; raw JAR artifacts may be copied unchanged into /workspace/lib with a .jar extension. Write diagnostic helpers in scratch. This image supplies target JDK 8; request an environment change for incompatible products. Do not replace or shadow product classes.'}
        else:
            # Only explicitly registered scenarios receive a deterministic fixture.
            baseline = env.run('sh /input/compile.sh')
            readiness = baseline_status(baseline.returncode, baseline.stdout)
            env.freeze_build()
            (directory / 'environment.log').write_text(baseline.stdout + baseline.stderr)
        write_json(directory / 'environment.json', {'runtime_image': env.jdk_digest, 'preset': env.preset, 'delta': delta, **readiness})
        config_path = directory / 'tools-config.json'
        tool_config = env.tool_config(packet['source_commit'], directory / 'tools.jsonl')
        tool_config['available_capabilities'] = ['source', 'runtime', 'debugger', 'temporary_tools']
        tool_config['readiness'] = {'runtime_image': env.jdk_digest, 'runtime_available': True, 'prepared_profile': args.profile, **readiness}
        preset = {key: value for key, value in env.preset.items() if key != 'inventory'}
        preset['tools'] = {key: value for key, value in env.preset['inventory'].items() if key != 'packages'}
        tool_config['readiness']['preset'] = preset
        write_json(config_path, tool_config)
        print(json.dumps({'tools_config': str(config_path)}), flush=True)
        # The Go adapter closes this pipe in a defer on success, failure or cancellation.
        sys.stdin.read()
    write_json(directory / 'environment-cleanup.json', {'state':'completed'})


if __name__ == '__main__':
    try: main()
    except KeyboardInterrupt: raise SystemExit(130)
