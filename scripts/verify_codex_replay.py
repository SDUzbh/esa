#!/usr/bin/env python3
"""Import one frozen real Jira ticket through Redmine and verify ESA's live run."""
import argparse
import html
import json
from pathlib import Path
import re
import time
import urllib.parse
import verify_redmine_esa as ui

ROOT = Path(__file__).resolve().parents[1]
BUNDLE = ROOT / '.esa/replays/KAFKA-2880/trial-1'

def main(retry=False):
    credentials = (ROOT / 'deploy/redmine/credentials.md').read_text()
    password = re.search(r'^Password: (.+)$', credentials, re.M).group(1)
    _, body, _ = ui.fetch('/login')
    _, body, url = ui.fetch('/login', {'authenticity_token': ui.csrf(body), 'username': 'admin', 'password': password})
    assert '/login' not in url, 'Administrator login failed'
    _, body, _ = ui.fetch('/esa/upload')
    project = re.search(r'<option value="(\d+)">ESA Demo</option>', body)
    assert project and project.group(1) == '1', 'Replay packet project does not match ESA Demo'
    _, listing, _ = ui.fetch('/esa?' + urllib.parse.urlencode({'q': 'KAFKA-2880'}))
    ids = re.findall(r'href="/esa/tickets/(\d+)"', listing)
    if not ids:
        ui.upload(project.group(1), 'KAFKA-2880-historical-snapshot.json', (BUNDLE / 'ticket.json').read_bytes())
        issue_id = ui.wait_issue('KAFKA-2880')
    else:
        issue_id = ids[0]
    print(json.dumps({'stage': 'ticket_imported', 'issue_id': issue_id, 'url': ui.BASE + '/esa/tickets/' + issue_id}), flush=True)
    if retry:
        _, body, _ = ui.fetch('/esa/tickets/' + issue_id)
        assert 'Awaiting Review' not in body, 'A completed diagnosis must not be retried'
        ui.fetch('/esa/tickets/' + issue_id + '/analyze', {'authenticity_token': ui.csrf(body), 'force': '1'})
    started = time.monotonic()
    while time.monotonic() - started < 230:
        _, body, _ = ui.fetch('/esa/tickets/' + issue_id)
        if 'Awaiting Review' in body:
            break
        if re.search(r'<strong>ESA state:</strong>\s*Failed', body):
            errors = re.findall(r'<p class="warning">(.*?)</p>', body, re.S)
            raise RuntimeError('Live run failed: ' + '; '.join(html.unescape(x) for x in errors))
        time.sleep(2)
    else: raise RuntimeError('Codex diagnosis did not reach engineer review')
    assert '<strong>Category:</strong> Code Defect' in body, 'Automatic classification failed'
    assert 'Engineering Analysis' in body, 'Engineering routing failed'
    assert re.search(r'Provider calls:</strong>\s*1', body), 'Expected exactly one provider invocation'
    assert 'Root Cause Candidates' in body and 'Evidence' in body and 'Verification Steps' in body
    (BUNDLE / 'redmine-report.html').write_text(body)
    evidence = {'source_key': 'KAFKA-2880', 'issue_id': issue_id, 'url': ui.BASE + '/esa/tickets/' + issue_id, 'checks': ['Frozen historical ticket imported through the Redmine plugin', 'Automatic Code Defect classification without human override', 'Engineering Analysis routing', 'One Codex provider invocation', 'Evidence and verification steps rendered', 'Awaiting engineer review; original issue remains open']}
    (ROOT / 'reports/codex-replay-integration.json').write_text(json.dumps(evidence, indent=2) + '\n')
    print(json.dumps(evidence, indent=2), flush=True)

if __name__ == '__main__':
    parser=argparse.ArgumentParser();parser.add_argument('--retry',action='store_true');args=parser.parse_args();main(args.retry)
