#!/usr/bin/env python3
"""Validate stable selection and ownership contracts without Docker/network."""
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

from tool_images import selection
from update_tool_images import choose_profiler, choose_soot, configurations, extract_profiler
from replay_session import baseline_status

def release(tag, **changes):
    result = {'tag_name': tag, 'draft': False, 'prerelease': False, 'published_at': '2026-01-01T00:00:00Z', 'assets': [{'name': 'async-profiler-' + tag + '-linux-arm64.tar.gz', 'digest': 'sha256:' + 'a' * 64, 'browser_download_url': 'https://github.com/async-profiler/async-profiler/releases/download/' + tag + '/asset.tar.gz'}]}
    result.update(changes)
    return result

class ToolImageTests(unittest.TestCase):
    def test_reproduced_product_symptom_is_not_an_environment_failure(self):
        status = baseline_status(20, 'CLIENT_MODULE_COMPILATION=PASS\nSUCCESSFUL_METADATA_CONTROL=PASS\nDISCONNECTED_METADATA_RESULT=java.lang.NullPointerException\n')
        self.assertEqual(status['status'], 'baseline_ready')
        self.assertEqual(status['scenario_status'], 'registered_symptom_reproduced')
        self.assertEqual(baseline_status(1, 'javac failed')['status'], 'baseline_failed')
    def test_official_stable_selection_excludes_prereleases(self):
        selected = choose_profiler([release('nightly'), release('v99.0', prerelease=True), release('v100.0', draft=True), release('v4.5'), release('v4.4')], 'arm64')
        self.assertEqual(selected['version'], 'v4.5')
        self.assertEqual(len(selected['sha256']), 64)

    def test_missing_official_digest_is_not_fabricated(self):
        value = release('v4.5')
        del value['assets'][0]['digest']
        with self.assertRaises(RuntimeError):
            choose_profiler([value], 'arm64')

    def test_soot_uses_numeric_stable_releases(self):
        self.assertEqual(choose_soot(['4.7.1', '4.10.0', '5.0.0-SNAPSHOT', '5.0.0-RC1']), '4.10.0')
        with self.assertRaises(RuntimeError):
            choose_soot(['5.0.0-SNAPSHOT'])

    def test_context_cannot_reuse_another_docker_registry(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            path = root / '.esa/tool-images/current.json'
            path.parent.mkdir(parents=True)
            path.write_text(json.dumps({'schema_version': 1, 'docker_context': 'a', 'profiles': {'general': {'image_id': 'sha256:' + 'b' * 64}}}))
            with self.assertRaises(RuntimeError):
                selection('general', 'b', root)

    def test_archive_cannot_escape_build_context(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive = root / 'invalid.tar.gz'
            with tarfile.open(archive, 'w:gz') as output:
                info = tarfile.TarInfo('../escape')
                info.size = 1
                output.addfile(info, io.BytesIO(b'x'))
            with self.assertRaises(ValueError):
                extract_profiler(archive, root / 'tools')
            self.assertFalse((root.parent / 'escape').exists())

    def test_declared_policy_preserves_target_jvm_major(self):
        profiles, policy = configurations()
        self.assertEqual(len(profiles), 5)
        self.assertEqual(policy['target_image'], 'eclipse-temurin:8-jdk-noble')
        self.assertEqual(policy['tool_jdk_image'], 'eclipse-temurin:21-jdk-noble')

if __name__ == '__main__':
    unittest.main()
