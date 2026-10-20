#!/usr/bin/env python3
"""Verify real container permissions, MCP tools, reuse and cancellation cleanup."""
import argparse
import json
from pathlib import Path
import signal
import subprocess
import sys
import tempfile

from replay_environment import ROOT, Environment, cleanup_sessions, dependencies, write_json
import mcp_view


def absent(env):
    for kind, name in [('container', env.name), ('container', env.writer), ('volume', env.volume)]:
        args = ['volume', 'inspect'] if kind == 'volume' else ['inspect']
        result = subprocess.run(env.docker + args + [name], capture_output=True)
        assert result.returncode != 0, 'Owned resource leaked: ' + name
    assert not env.directory.exists(), 'Temporary session directory leaked'


def child():
    with Environment() as env:
        print(json.dumps({'directory':str(env.directory), 'name':env.name, 'writer':env.writer, 'volume':env.volume}), flush=True)
        sys.stdin.read()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--child', action='store_true')
    parser.add_argument('--output', default='reports/replay-runtime-check.json')
    args = parser.parse_args()
    if args.child: child(); return
    bundle = ROOT / '.esa/replays/KAFKA-2880/trial-1'
    manifest = json.loads((bundle / 'manifest.json').read_text())
    outcomes = {}
    with tempfile.TemporaryDirectory(prefix='esa-runtime-check-') as temporary:
        directory = Path(temporary)
        with Environment() as env:
            inputs = {'deps/' + name: value['path'] for name, value in dependencies(bundle/'source/build.gradle', env.cache).items()}
            inputs.update({'compile.sh': ROOT/'examples/replay/compile.sh', 'MetadataDisconnectReplay.java': ROOT/'examples/replay/MetadataDisconnectReplay.java'})
            installer = directory / 'install.sh'; installer.write_text('set -eu\njava -version\n')
            inputs['install.sh'] = installer
            env.sync(bundle/'source', manifest['files'], inputs)
            baseline = env.run('sh /input/compile.sh'); env.freeze_build()
            assert 'DISCONNECTED_METADATA_RESULT=java.lang.NullPointerException' in baseline.stdout
            config = directory/'tools-config.json'; audit = directory/'tools.jsonl'
            write_json(config, env.tool_config(manifest['source_commit'], audit))
            process = subprocess.Popen([str(ROOT/'bin/esa-codex'), '-tool-server', '-tools-config', str(config)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            sequence = 0
            def call(name, arguments):
                nonlocal sequence
                sequence += 1
                process.stdin.write(json.dumps({'jsonrpc':'2.0','id':sequence,'method':'tools/call','params':{'name':name,'arguments':arguments}})+'\n'); process.stdin.flush()
                response = json.loads(process.stdout.readline())
                assert 'error' not in response, response
                result = response['result']; assert not result.get('isError'), result
                return mcp_view.decode(result['content'][0]['text'])
            try:
                call('install_environment', {})
                source = 'clients/src/main/java/org/apache/kafka/clients/consumer/internals/Fetcher.java'
                call('read_code', {'path':source,'first_line':1,'line_count':5})
                script = 'set -eu\nif echo changed > /input/source/'+source+'; then exit 91; fi\nif echo changed > /input/classes/MetadataDisconnectReplay.class; then exit 92; fi\nprintf "printf AUXILIARY_SCRIPT=PASS\\\\n\\n" > /workspace/helper.sh\nsh /workspace/helper.sh\ncommand -v jdb\nsh /input/compile.sh\n'
                evidence = [line['text'] for line in call('run', {'script':script})['evidence']['lines']]
                text = '\n'.join(evidence)
                assert 'Read-only file system' in text and 'AUXILIARY_SCRIPT=PASS' in text and 'DISCONNECTED_METADATA_RESULT=java.lang.NullPointerException' in text, text
                call('request_environment_change', {'reason':'Integration check only', 'proposed_change':'Review the registered runtime profile', 'validation':'Rerun the control; no automatic change'})
            finally:
                process.stdin.close(); process.wait(timeout=10)
            records = [json.loads(line) for line in audit.read_text().splitlines()]
            assert records[-1]['request']['state'] == 'requested'
            env.run('touch /workspace/leaked-helper /workspace/..hidden-helper; sleep 600 >/tmp/background.log 2>&1 &')
            env.reset_job()
            assert env.run('set -e; test ! -e /workspace/leaked-helper; test ! -e /workspace/..hidden-helper; test -f /input/classes/.complete; test "$(pgrep -f "^sleep 600$")" = ""').returncode == 0
            delta = env.sync(bundle/'source', manifest['files'], inputs)
            assert delta['source_changed'] == delta['source_deleted'] == delta['input_changed'] == 0
            outcomes['permissions_tools_and_reuse'] = {'state':'passed', 'tools':[x['tool'] for x in records], 'delta':delta}
        absent(env); outcomes['normal_cleanup'] = 'passed'
        for sig, label in [(signal.SIGTERM, 'sigterm_cleanup'), (signal.SIGKILL, 'stale_session_recovery')]:
            log = (directory/(label+'.log')).open('w')
            process = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), '--child'], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=log, text=True)
            saved = json.loads(process.stdout.readline())
            process.send_signal(sig); process.wait(timeout=45); log.close()
            check = Environment(); check.name=saved['name']; check.writer=saved['writer']; check.volume=saved['volume']; check.directory=Path(saved['directory'])
            if sig == signal.SIGKILL: cleanup_sessions()
            absent(check); outcomes[label] = 'passed'
    write_json(ROOT/args.output, outcomes)
    print(json.dumps(outcomes,indent=2))


if __name__ == '__main__':
    try: main()
    except KeyboardInterrupt: raise SystemExit(130)
