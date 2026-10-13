#!/usr/bin/env python3
"""Owned test target with a deterministic HTTP provider; never calls a model."""
import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import signal
import subprocess
import threading


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--esa-bin', required=True)
    parser.add_argument('--store', required=True)
    parser.add_argument('--rules', required=True)
    parser.add_argument('--listen', required=True)
    parser.add_argument('--replay-at', default='')
    parser.add_argument('--replay-projects', default='')
    args = parser.parse_args()
    token = os.environ['ESA_API_TOKEN']
    directory = Path(args.store).parent
    directory.mkdir(parents=True, exist_ok=True)
    calls, lock = [], threading.Lock()

    class Provider(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            if self.path != '/analyze' or self.headers.get('Authorization') != 'Bearer ' + token:
                self.send_error(401)
                return
            size = int(self.headers.get('Content-Length', '0'))
            if size > 1024 * 1024:
                self.send_error(413)
                return
            packet = json.loads(self.rfile.read(size))
            assert packet['read_only'] and packet['task_contract']['read_only']
            with lock:
                calls.append({'run_id': packet['run_id'], 'snapshot_id': packet['task_contract']['snapshot_id'], 'cutoff': packet['task_contract']['observation_cutoff']})
                (directory / 'provider-calls.json').write_text(json.dumps(calls))
            evidence = {'source': 'git', 'reference': 'fixture:Example.java:10', 'quote': 'assignment.size()'}
            result = {'summary': 'Fixture empty assignment hypothesis', 'issue': {'existing_id': packet['known_issues'][0]['id'], 'title': 'Fixture assignment', 'reason': 'Deterministic test input'},
                      'findings': [{'commit': 'fixture', 'file': 'Example.java', 'start_line': 10, 'end_line': 10, 'problem': 'Assignment may be absent', 'solution': 'Guard assignment before use', 'evidence': [evidence]}],
                      'evidence': [evidence], 'recommendations': ['Guard assignment'], 'verification_steps': ['Exercise empty assignment'], 'limitations': ['Synthetic provider; no causal or source mapping validation'],
                      'usage': {'calls': 1, 'input_tokens': 20, 'output_tokens': 10, 'tokens_reported': True}}
            encoded = json.dumps(result).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

    provider = ThreadingHTTPServer(('127.0.0.1', 0), Provider)
    thread = threading.Thread(target=provider.serve_forever, daemon=True)
    thread.start()
    environment = dict(os.environ, ESA_REDMINE_URL='', ESA_ANALYZER_URL='http://127.0.0.1:%d/analyze' % provider.server_port, ESA_ANALYZER_TOKEN=token)
    command = [args.esa_bin, '-listen', args.listen, '-store', args.store, '-rules', args.rules]
    if args.replay_at or args.replay_projects:
        command += ['-replay-at', args.replay_at, '-replay-projects', args.replay_projects]
    child = subprocess.Popen(command, env=environment)
    stop = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    try:
        while child.poll() is None and not stop.wait(0.1):
            pass
    finally:
        if child.poll() is None:
            child.terminate()
        try:
            child.wait(timeout=8)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait()
        provider.shutdown()
        provider.server_close()
        thread.join(timeout=3)

if __name__ == '__main__':
    main()
