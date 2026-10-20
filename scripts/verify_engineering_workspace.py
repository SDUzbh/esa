#!/usr/bin/env python3
"""Verify a fresh engineering workspace through real MCP tools, without model calls."""
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import mcp_view

ROOT = Path(__file__).resolve().parents[1]


def main():
    with tempfile.TemporaryDirectory(prefix='esa-engineering-workspace-') as temporary:
        folder = Path(temporary)
        source = folder / 'source'
        source.mkdir()
        code = source / 'WorkspaceExample.java'
        code.write_text('public class WorkspaceExample { public static void main(String[] args) { System.out.println(args[0].length()); } }\n')
        hashes = {code.name: hashlib.sha256(code.read_bytes()).hexdigest()}
        manifest = folder / 'manifest.json'
        manifest.write_text(json.dumps({'source_commit': 'workspace-test', 'files': hashes}))
        runtime = folder / 'runtime'
        config = {'session': 'workspace-test', 'source_dir': str(source), 'source_commit': 'workspace-test', 'files': hashes,
                  'audit_path': str(folder / 'tools.jsonl'), 'profiles_root': str(ROOT / 'profiles'),
                  'allowed_profiles': ['general', 'java-debug'], 'available_capabilities': ['source', 'runtime', 'debugger', 'temporary_tools'],
                  'runtime_setup': {'script': str(ROOT / 'scripts/replay_session.py'), 'evidence': str(manifest), 'directory': str(runtime), 'workspace': True}}
        path = folder / 'tools.json'
        path.write_text(json.dumps(config))
        process = subprocess.Popen([str(ROOT / 'bin/esa-codex'), '-tool-server', '-tools-config', str(path)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        counter = 0

        def call(name, args):
            nonlocal counter
            counter += 1
            process.stdin.write(json.dumps({'jsonrpc': '2.0', 'id': counter, 'method': 'tools/call', 'params': {'name': name, 'arguments': args}}) + '\n')
            process.stdin.flush()
            result = json.loads(process.stdout.readline())['result']
            assert not result['isError'], result
            return mcp_view.decode(result['content'][0]['text'])

        try:
            first = call('read_code', {'path': code.name, 'first_line': 1, 'line_count': 175})['evidence']
            assert first['first_line'] == first['last_line'] == first['lines'][0]['line'] == 1
            assert first['lines'][0]['id'].startswith(Path(code.name).name + ':1#') and not runtime.exists()
            ready = call('capability_request', {'profile': 'java-debug', 'version': '2', 'requested_capabilities': ['runtime'], 'hypothesis_id': 'H1', 'reason': 'Compile unchanged source and test empty arguments', 'expected_observation': 'A passing argument control and a failing empty-argument case'})
            assert ready['decision']['status'] == 'granted', ready
            assert ready['readiness']['status'] == 'workspace_ready' and not ready['readiness']['compiled']
            result = call('run', {'script': '''set -eu
test ! -e /input/compile.sh
test ! -e /input/MetadataDisconnectReplay.java
test ! -e /input/classes
test ! -e /input/deps
if echo changed > /input/source/WorkspaceExample.java; then exit 91; fi
mkdir -p /workspace/classes
javac -d /workspace/classes /input/source/WorkspaceExample.java
test "$(java -cp /workspace/classes WorkspaceExample hello)" = 5
if java -cp /workspace/classes WorkspaceExample > /workspace/failure.log 2>&1; then exit 92; fi
grep -q ArrayIndexOutOfBoundsException /workspace/failure.log
# The generic workspace deliberately has no /input/classes. All Soot aliases
# must accept one complete scratch classpath, including paths containing spaces.
cp -R /workspace/classes '/workspace/classes with spaces'
for option in -cp -soot-class-path -soot-classpath; do
    output="/workspace/soot-$option"
    soot "$option" '/workspace/classes with spaces:/opt/java/openjdk/jre/lib/rt.jar' -f jimple -keep-line-number -d "$output" WorkspaceExample
    test -s "$output/WorkspaceExample.jimple"
    grep -q 'length' "$output/WorkspaceExample.jimple"
done
# Soot may return zero for an option error, so inspect its diagnostic.
soot -cp >/workspace/invalid-soot.log 2>&1 || :
grep -qF 'No value given for option -cp' /workspace/invalid-soot.log
sootup cfg WorkspaceExample main --classpath /workspace/classes:/opt/java/openjdk/jre/lib/rt.jar > /workspace/graph.txt
grep -q -- '->' /workspace/graph.txt
printf 'WORKSPACE_COMPILE_RUN_SOOT_SOOTUP=PASS\n'
''', 'timeout_seconds': 120})
            assert any(line['text'] == 'WORKSPACE_COMPILE_RUN_SOOT_SOOTUP=PASS' for line in result['evidence']['lines']), result
            assert hashes[code.name] == hashlib.sha256(code.read_bytes()).hexdigest()
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
        assert json.loads((runtime / 'environment-cleanup.json').read_text())['state'] == 'completed'
        print(json.dumps({'status': 'passed', 'checks': ['no_prior_evidence_or_reproducer', 'lazy_runtime', 'numbered_evidence_ids', 'unchanged_source_compilation', 'success_failure_controls', 'soot_scratch_classpath_aliases_and_spaces', 'soot_missing_classpath_diagnostic', 'sootup_cfg', 'read_only_source', 'owned_cleanup']}))


if __name__ == '__main__':
    main()
