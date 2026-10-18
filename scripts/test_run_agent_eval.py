#!/usr/bin/env python3
"""Check the Agent evaluation launcher with stand-in binaries; no model calls."""
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest

LAUNCHER = Path(__file__).resolve().parent / 'run_agent_eval.py'
# Records its arguments and a digest of the token it was given, never the token.
RECORD = '''import hashlib, json, os, sys
def record(name, **extra):
    token = os.environ.get('ESA_ANALYZER_TOKEN', '')
    with open(os.path.join(os.environ['FAKE_DIR'], name + '.json'), 'w') as f:
        json.dump(dict(argv=sys.argv[1:], token=hashlib.sha256(token.encode()).hexdigest() if token else '', **extra), f)
'''
PROVIDER = RECORD + '''import signal, socket
args = sys.argv[1:]
host, port = args[args.index('-listen') + 1].rsplit(':', 1)
server = socket.socket(); server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); server.bind((host, int(port))); server.listen()
record('provider')
def done(*_):
    record('provider', stopped=True); sys.exit(0)
signal.signal(signal.SIGINT, done)
while True:
    server.accept()[0].close()
'''
REPLAY = RECORD + '''import signal, time
record('replay', url=os.environ.get('ESA_ANALYZER_URL'))
def done(*_):
    record('replay', url=os.environ.get('ESA_ANALYZER_URL'), stopped=True); sys.exit(1)
signal.signal(signal.SIGTERM, done)
if os.environ.get('FAKE_REPLAY_WAIT'):
    while True: time.sleep(0.1)
'''


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


class LauncherTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        root = Path(self.dir.name)
        self.bin = root / 'bin'
        self.bin.mkdir()
        for name, body in (('esa-codex', PROVIDER), ('system-replay', REPLAY)):
            path = self.bin / name
            path.write_text('#!%s\n%s' % (sys.executable, body))
            path.chmod(0o755)
        self.set = root / 'set'
        self.set.mkdir()
        manifest = {'target': {'start': {'args': ['esa-host', '-issues-directory', str(root / 'issues')], 'env': {'ESA_API_TOKEN': 'api-{{token}}'}}}}
        (self.set / 'manifest.json').write_text(json.dumps(manifest))
        (self.set / 'releases.json').write_text('{"releases": []}')
        self.catalog = root / 'catalog.json'
        self.catalog.write_text('{}')
        self.listen = '127.0.0.1:%d' % free_port()
        self.env = dict(os.environ, ESA_DATABASE_URL='postgres://fixture', FAKE_DIR=str(root))

    def tearDown(self):
        self.dir.cleanup()

    def launch(self, *extra, env=None):
        return subprocess.Popen([sys.executable, '-B', str(LAUNCHER), '--set', str(self.set), '--output', str(Path(self.dir.name, 'out')),
                                 '--bin', str(self.bin), '--listen', self.listen, *extra], env=env or self.env,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    def recorded(self, name):
        return json.loads(Path(self.dir.name, name + '.json').read_text())

    def test_catalog_token_and_shutdown(self):
        launcher = self.launch('--tool-catalog', str(self.catalog))
        _, err = launcher.communicate(timeout=60)
        self.assertEqual(launcher.returncode, 0, err)
        provider, replay = self.recorded('provider'), self.recorded('replay')
        args = provider['argv']
        self.assertEqual(args[args.index('-tool-catalog') + 1], str(self.catalog.resolve()))
        self.assertEqual(args[args.index('-issues-directory') + 1], str(Path(self.dir.name, 'issues')))
        self.assertTrue(provider['token'] and provider['token'] == replay['token'])
        self.assertEqual(replay['url'], 'http://%s/analyze' % self.listen)
        self.assertTrue(provider['stopped'])
        record = json.loads(Path(self.dir.name, 'out', 'launcher.json').read_text())
        self.assertEqual((record['replay_exit'], record['stopped']), (0, False))
        self.assertNotIn(provider['token'], Path(self.dir.name, 'out', 'launcher.json').read_text())

    def test_sigterm_stops_replay_then_provider(self):
        launcher = self.launch('--no-tool-catalog', env=dict(self.env, FAKE_REPLAY_WAIT='1'))
        path = Path(self.dir.name, 'replay.json')
        deadline = time.monotonic() + 30
        while not path.exists() and time.monotonic() < deadline:
            time.sleep(0.1)
        launcher.send_signal(signal.SIGTERM)
        launcher.communicate(timeout=60)
        self.assertTrue(self.recorded('replay').get('stopped'))
        self.assertTrue(self.recorded('provider').get('stopped'))
        self.assertNotIn('-tool-catalog', self.recorded('provider')['argv'])

    def test_refuses_unsafe_setups(self):
        missing = self.launch('--tool-catalog', str(self.catalog), env={k: v for k, v in self.env.items() if k != 'ESA_DATABASE_URL'})
        self.assertNotEqual(missing.wait(timeout=30), 0)
        catalog_choice = self.launch()
        self.assertNotEqual(catalog_choice.wait(timeout=30), 0)
        manifest = json.loads((self.set / 'manifest.json').read_text())
        manifest['target']['start']['env']['ESA_ANALYZER_TOKEN'] = 'analyzer-{{token}}'
        (self.set / 'manifest.json').write_text(json.dumps(manifest))
        fixture = self.launch('--no-tool-catalog')
        self.assertNotEqual(fixture.wait(timeout=30), 0)
        for process in (missing, catalog_choice, fixture):
            process.communicate()
        self.assertFalse(Path(self.dir.name, 'provider.json').exists())


if __name__ == '__main__':
    unittest.main()
