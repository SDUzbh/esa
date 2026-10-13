#!/usr/bin/env python3
"""Black-box concurrency and revision assertions against an owned ESA process."""
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import tempfile
import time
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parents[1]

def main():
    token = secrets.token_hex(24)
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', 0))
        port = probe.getsockname()[1]
    url = 'http://127.0.0.1:%d' % port
    with tempfile.TemporaryDirectory(prefix='esa-v2-http-') as temporary:
        directory = Path(temporary)
        log = (directory / 'target.log').open('w')
        process = subprocess.Popen(['python3', str(ROOT / 'scripts/esa_v2_fixture_target.py'), '--esa-bin', str(ROOT / 'bin/esa-host'), '--store', str(directory / 'state.json'), '--rules', str(ROOT / 'config/rules.v2-replay.json'), '--listen', '127.0.0.1:%d' % port], env=dict(os.environ, ESA_API_TOKEN=token), stdout=log, stderr=log, start_new_session=True)

        def request(path, value=None, tickets=None):
            headers = {'Authorization': 'Bearer ' + token, 'X-ESA-Project-IDs': '1', 'Content-Type': 'application/json'}
            if tickets is not None:
                headers['X-ESA-Ticket-IDs'] = tickets
            with urlopen(Request(url + path, data=None if value is None else json.dumps(value).encode(), headers=headers), timeout=15) as response:
                return json.load(response)

        def ticket(index):
            return {'id': 'customer-%d' % index, 'source_key': 'customer-%d' % index, 'project_id': '1', 'subject': 'Consumer empty assignment crash', 'description': 'NullPointerException during empty assignment', 'tracker': 'Bug', 'status': 'Open', 'components': ['consumer'], 'versions': ['1.0'], 'environment': {'jdk': '8'}, 'created_at': '2020-01-01T00:00:00Z', 'updated_at': '2020-01-01T00:00:00Z'}

        try:
            for _ in range(100):
                if process.poll() is not None:
                    raise RuntimeError('Owned target failed to start')
                try:
                    request('/healthz')
                    break
                except OSError:
                    time.sleep(0.05)
            else:
                raise RuntimeError('Owned target did not become ready')

            def submit(index):
                return request('/v1/events', {'id': 'event-%d' % index, 'origin': 'black_box_fixture', 'forwarded_by': 'fixture-human', 'forwarded_at': '2020-01-01T00:00:00Z', 'ticket': ticket(index)})
            with ThreadPoolExecutor(max_workers=20) as executor:
                receipts = list(executor.map(submit, range(100)))
            assert len({receipt['run_id'] for receipt in receipts}) == 1, 'duplicate task IDs'
            for _ in range(100):
                row = request('/v1/tickets/customer-0')['ticket']
                if row['latest_run']['state'] == 'awaiting_review':
                    break
                time.sleep(0.05)
            assert row['solution']['state'] == 'proposed'
            assert len(json.loads((directory / 'provider-calls.json').read_text())) == 1, 'duplicate provider spend'
            assert request('/v1/engineering-issues')['issues'][0]['visible_ticket_count'] == 100
            task_id, solution_id = row['investigation']['id'], row['solution']['id']
            owner = row['solution']['source_ticket_id']
            member = next('customer-%d' % i for i in range(100) if 'customer-%d' % i != owner)
            restricted = request('/v1/tickets/' + member, tickets=member)['ticket']
            assert 'solution' not in restricted, 'protected lead report exposed'
            request('/v1/replay/clock', {'time': '2020-01-01T00:00:01Z'})
            request('/v1/investigations/' + task_id + '/review', {'expected_solution_id': solution_id, 'action': 'approve', 'reviewer': 'fixture-engineer', 'resolution': {'cause': 'Empty assignment', 'solution': 'Guard assignment', 'validation': 'Reviewed fixture only'}})
            updated = request('/v1/tickets/' + member)['ticket']
            assert updated['solution']['state'] == 'trusted' and updated['solution']['revision'] == 2
            history = request('/v1/investigations/' + task_id + '/solutions')['solutions']
            assert [revision['state'] for revision in history] == ['proposed', 'trusted']
            assert history[0]['id'] == solution_id
            assert updated['latest_run']['report']['envelope_v2']['causal_verification'] == 'unverified'
            split = request('/v1/tickets/' + member + '/membership/review', {'expected_snapshot_id': updated['snapshot_id'], 'issue_id': '', 'reviewer': 'fixture-engineer', 'reason': 'Independent customer symptom needs separate scope review'})
            assert split['run_id'] != receipts[0]['run_id'], 'split retained the original task'
            print(json.dumps({'status': 'passed', 'concurrent_tickets': 100, 'initial_provider_calls': 1, 'checks': ['deduplication', 'pending_not_trusted', 'visibility', 'immutable_solution_revisions', 'reviewed_split', 'causal_status']}))
        finally:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=12)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
            log.close()

if __name__ == '__main__':
    main()
