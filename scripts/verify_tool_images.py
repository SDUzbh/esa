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
    for profile in PROFILES:
        image, _ = selection(profile, docker.context)
        inventory = inspect_image(docker, image['image_id'])
        assert inventory['commands']['asprof'] and inventory['commands']['jdb']
        assert 'version "1.8.' in inventory['target_java']
        assert 'version "21.' in inventory['tool_java']
    with tempfile.TemporaryDirectory(prefix='esa-preset-test-') as temporary:
        source = Path(temporary) / 'source'
        source.mkdir()
        code = source / 'Example.java'
        code.write_text('class Example {}\n')
        with Environment(profile='java-debug') as environment:
            environment.sync(source, {'Example.java': digest(code)}, {})
            result = environment.run('rg --version; jq --version; asprof --version; /opt/esa/tool-jdk/bin/jfr --version; java -version')
            assert result.returncode == 0, result.stderr
            assert environment.run('touch /input/source/Example.java').returncode != 0
            assert environment.run('mkdir -p /workspace/classes; touch /workspace/classes/.complete; printf baseline > /workspace/classes/control.txt').returncode == 0
            environment.freeze_build()
            assert environment.run('cat /input/classes/control.txt').stdout == 'baseline'
            config = Path(temporary) / 'fixture.config'
            config.write_text('changed runtime input\n')
            environment.sync(source, {'Example.java': digest(code)}, {'fixture.config': config})
            assert environment.run('test ! -e /input/classes/control.txt').returncode == 0
            name, writer, volume = environment.name, environment.writer, environment.volume
        for kind, resource in [('container', name), ('container', writer), ('volume', volume)]:
            args = ['volume', 'inspect', resource] if kind == 'volume' else ['inspect', resource]
            checked = subprocess.run(docker.command + args, capture_output=True, timeout=15)
            assert checked.returncode != 0, 'Owned resource survived cleanup: ' + resource
    print(json.dumps({'status': 'passed', 'profile_images': 5, 'checks': ['installed_versions', 'separate_target_tool_jvms', 'offline_sandbox', 'readonly_product_source', 'frozen_baseline_and_delta', 'owned_resource_cleanup']}))

if __name__ == '__main__':
    main()
