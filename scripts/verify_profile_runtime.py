#!/usr/bin/env python3
"""Verify lazy Profile provisioning on a registered packet without model calls."""
import argparse
import json
from pathlib import Path
import subprocess
import tempfile
import mcp_view

ROOT = Path(__file__).resolve().parents[1]

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--evidence', required=True)
    args = parser.parse_args()
    evidence = Path(args.evidence).resolve()
    manifest = json.loads((evidence.parent / 'manifest.json').read_text())
    with tempfile.TemporaryDirectory(prefix='esa-profile-runtime-') as temporary:
        directory = Path(temporary)
        runtime = directory / 'runtime'
        config = {'session': 'profile-fixture', 'source_dir': str(evidence.parent / 'source'), 'source_commit': manifest['source_commit'], 'files': manifest['files'], 'audit_path': str(directory / 'audit.jsonl'),
                  'profiles_root': str(ROOT / 'profiles'), 'allowed_profiles': ['general', 'performance'], 'available_capabilities': ['source', 'runtime', 'debugger', 'temporary_tools'], 'readiness': {'runtime_available': False},
                  'runtime_setup': {'script': str(ROOT / 'scripts/replay_session.py'), 'evidence': str(evidence), 'directory': str(runtime)}}
        path = directory / 'tools.json'
        path.write_text(json.dumps(config))
        process = subprocess.Popen([str(ROOT / 'bin/esa-codex'), '-tool-server', '-tools-config', str(path)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        counter = 0
        def tool(name, arguments):
            nonlocal counter
            counter += 1
            process.stdin.write(json.dumps({'jsonrpc': '2.0', 'id': counter, 'method': 'tools/call', 'params': {'name': name, 'arguments': arguments}}) + '\n')
            process.stdin.flush()
            result = json.loads(process.stdout.readline())['result']
            assert not result['isError'], result
            return mcp_view.decode(result['content'][0]['text'])
        def call(profile, capabilities):
            return tool('capability_request', {'profile': profile, 'version': '2', 'requested_capabilities': capabilities, 'hypothesis_id': 'H1', 'reason': 'Inspect registered behavior', 'expected_observation': 'Source and baseline controls'})
        try:
            denied = call('unregistered', ['runtime'])
            assert denied['decision']['status'] == 'denied' and not runtime.exists()
            source = call('general', ['source'])
            assert source['decision']['status'] == 'granted' and not runtime.exists()
            ready = call('performance', ['runtime'])
            assert ready['decision']['status'] == 'granted', ready
            assert ready['readiness']['prepared_profile'] == 'performance'
            assert ready['readiness']['compiled'] and ready['readiness']['control_passed']
            compiled = tool('run', {'script': '''set -eu
mkdir -p /workspace/diagnostics/classes
cat > /workspace/diagnostics/RuntimeCompileProbe.java <<'JAVA'
public class RuntimeCompileProbe {
    public static void main(String[] args) {
        org.apache.kafka.common.KafkaException value =
            new org.apache.kafka.common.KafkaException("AGENT_COMPILE_RUN=PASS");
        if (!"AGENT_COMPILE_RUN=PASS".equals(value.getMessage())) {
            throw new AssertionError("Unexpected product class behavior");
        }
        System.out.println(value.getMessage());
    }
}
JAVA
javac -cp '/input/classes:/input/deps/*' -d /workspace/diagnostics/classes /workspace/diagnostics/RuntimeCompileProbe.java
java -cp '/input/classes:/input/deps/*:/workspace/diagnostics/classes' RuntimeCompileProbe
''', 'timeout_seconds': 120})
            assert 'AGENT_COMPILE_RUN=PASS' in [line['text'] for line in compiled['evidence']['lines']], compiled
            analyzed = tool('run', {'script': '''set -eu
product=/input/classes/org/apache/kafka/clients/NetworkClient.class
before=$(sha256sum "$product")
soot -f jimple -keep-line-number -d /workspace/diagnostics/soot org.apache.kafka.clients.NetworkClient
test -s /workspace/diagnostics/soot/org.apache.kafka.clients.NetworkClient.jimple
grep -qF handleCompletedReceives /workspace/diagnostics/soot/org.apache.kafka.clients.NetworkClient.jimple
test "$before" = "$(sha256sum "$product")"
printf 'AGENT_SOOT_ANALYSIS=PASS\\n'
''', 'timeout_seconds': 120})
            assert 'AGENT_SOOT_ANALYSIS=PASS' in [line['text'] for line in analyzed['evidence']['lines']], analyzed
            sootup = tool('run', {'script': '''set -eu
product=/input/classes/org/apache/kafka/clients/NetworkClient.class
before=$(sha256sum "$product")
sootup jimple org.apache.kafka.clients.NetworkClient handleCompletedReceives > /workspace/diagnostics/network-client.jimple
sootup cfg org.apache.kafka.clients.NetworkClient handleCompletedReceives > /workspace/diagnostics/network-client.cfg
grep -qF handleCompletedReceives /workspace/diagnostics/network-client.jimple
grep -q -- '->' /workspace/diagnostics/network-client.cfg
test "$before" = "$(sha256sum "$product")"
printf 'AGENT_SOOTUP_ANALYSIS=PASS\\n'
''', 'timeout_seconds': 120})
            assert 'AGENT_SOOTUP_ANALYSIS=PASS' in [line['text'] for line in sootup['evidence']['lines']], sootup
            saved = json.loads((runtime / 'runtime-owned.json').read_text())
        finally:
            process.stdin.close()
            try:
                process.wait(timeout=50)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            subprocess.run(['python3', str(ROOT / 'scripts/replay_session.py'), '--cleanup-owned', '--run-dir', str(runtime)], check=True, timeout=90)
        assert process.returncode == 0, process.stderr.read()
        docker = ['docker', '--context', saved['context']]
        for kind, key in [('container', 'container'), ('container', 'writer'), ('volume', 'volume')]:
            command = ['volume', 'inspect'] if kind == 'volume' else ['inspect']
            assert subprocess.run(docker + command + [saved[key]], capture_output=True).returncode != 0
        print(json.dumps({'status': 'passed', 'profile': 'performance', 'checks': ['denial_without_runtime', 'source_without_runtime', 'selected_profile_provisioning', 'registered_baseline_controls', 'agent_tool_compile_and_run', 'agent_soot_bytecode_analysis', 'agent_sootup_jimple_cfg', 'owned_cleanup']}))

if __name__ == '__main__':
    main()
