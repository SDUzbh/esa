#!/usr/bin/env python3
"""Exercise preset images in owned offline/read-only runtimes and verify removal."""
import json
from pathlib import Path
import subprocess
import tempfile

from replay_environment import Environment, digest
from update_tool_images import Docker, PROFILES, inspect_image
from tool_images import selection

def main():
    docker = Docker('colima-redmine')
    shared_image = None
    for profile in PROFILES:
        image, _ = selection(profile, docker.context)
        shared_image = shared_image or image['image_id']
        assert image['image_id'] == shared_image, 'Profiles must share one verified toolkit'
    inventory = inspect_image(docker, shared_image)
    assert inventory['commands']['asprof'] and inventory['commands']['jdb']
    assert inventory['commands']['soot'] and 'Soot version' in inventory['soot']
    assert inventory['commands']['sootup'] and inventory['sootup'].startswith('SootUp ')
    assert 'version "1.8.' in inventory['target_java']
    assert 'version "21.' in inventory['tool_java']
    with tempfile.TemporaryDirectory(prefix='esa-preset-test-') as temporary:
        source = Path(temporary) / 'source'
        source.mkdir()
        code = source / 'Example.java'
        code.write_text('class Example { static int abs(int value) { return helper(value); } static int helper(int value) { if (value < 0) return -value; return value; } }\n')
        with Environment(profile='java-debug') as environment:
            environment.sync(source, {'Example.java': digest(code)}, {})
            result = environment.run('rg --version; jq --version; asprof --version; /opt/esa/tool-jdk/bin/jfr --version; java -version')
            assert result.returncode == 0, result.stderr
            assert environment.run('touch /input/source/Example.java').returncode != 0
            assert environment.run('set -eu; mkdir -p /workspace/classes; javac -g -d /workspace/classes /input/source/Example.java; touch /workspace/classes/.complete; printf baseline > /workspace/classes/control.txt').returncode == 0
            environment.freeze_build()
            assert environment.run('cat /input/classes/control.txt').stdout == 'baseline'
            analyzed = environment.run('''set -eu
before=$(sha256sum /input/classes/Example.class)
sootup jimple Example helper > /workspace/example.jimple
sootup cfg Example helper > /workspace/example.cfg
grep -q 'return' /workspace/example.jimple
grep -q -- '->' /workspace/example.cfg
if sootup jimple Example nonexistent >/dev/null 2>&1; then exit 1; fi
test "$before" = "$(sha256sum /input/classes/Example.class)"
cat > /workspace/CallGraphProbe.java <<'JAVA'
import java.util.List;
import sootup.core.model.SourceType;
import sootup.java.bytecode.frontend.inputlocation.JavaClassPathAnalysisInputLocation;
import sootup.java.core.views.JavaView;
import sootup.callgraph.ClassHierarchyAnalysisAlgorithm;
public class CallGraphProbe {
    public static void main(String[] args) {
        var view = new JavaView(List.of(
            new JavaClassPathAnalysisInputLocation("/input/classes", SourceType.Application),
            new JavaClassPathAnalysisInputLocation("/opt/java/openjdk/jre/lib/rt.jar", SourceType.Library)));
        var entry = view.getIdentifierFactory().parseMethodSignature("<Example: int abs(int)>");
        var graph = new ClassHierarchyAnalysisAlgorithm(view).initialize(List.of(entry));
        if (graph.callTargetsFrom(entry).stream().noneMatch(target -> target.getName().equals("helper"))) {
            throw new AssertionError("Expected Example.abs -> Example.helper");
        }
        System.out.println("SOOTUP_CALLGRAPH=PASS");
    }
}
JAVA
/opt/esa/tool-jdk/bin/javac --release 17 -cp '/opt/esa/sootup/lib/*' -d /workspace /workspace/CallGraphProbe.java
/opt/esa/tool-jdk/bin/java -Xmx1024m -cp '/workspace:/opt/esa/sootup/lib/*' CallGraphProbe
''', timeout=120)
            assert analyzed.returncode == 0 and 'SOOTUP_CALLGRAPH=PASS' in analyzed.stdout, (analyzed.stdout, analyzed.stderr)
            config = Path(temporary) / 'fixture.config'
            config.write_text('changed runtime input\n')
            environment.sync(source, {'Example.java': digest(code)}, {'fixture.config': config})
            assert environment.run('test ! -e /input/classes/control.txt').returncode == 0
            name, writer, volume = environment.name, environment.writer, environment.volume
        for kind, resource in [('container', name), ('container', writer), ('volume', volume)]:
            args = ['volume', 'inspect', resource] if kind == 'volume' else ['inspect', resource]
            checked = subprocess.run(docker.command + args, capture_output=True, timeout=15)
            assert checked.returncode != 0, 'Owned resource survived cleanup: ' + resource
    print(json.dumps({'status': 'passed', 'toolkit_images': 1, 'profiles': 5, 'checks': ['installed_versions', 'separate_target_tool_jvms', 'offline_sandbox', 'readonly_product_source', 'sootup_jimple_cfg_and_callgraph_sdk', 'frozen_baseline_and_delta', 'owned_resource_cleanup']}))

if __name__ == '__main__':
    main()
