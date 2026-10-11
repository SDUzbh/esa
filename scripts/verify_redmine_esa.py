#!/usr/bin/env python3
"""Exercise the real Redmine plugin using synthetic tickets and existing credentials."""
import html
import http.cookiejar
import json
from pathlib import Path
import re
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
BASE = 'http://127.0.0.1:8080'
JAR = http.cookiejar.CookieJar()
CLIENT = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(JAR))


def fetch(path, fields=None, multipart=None):
    headers = {}
    data = None
    if multipart:
        boundary = 'ESAValidationUploadBoundary'
        parts = []
        for key, value in fields.items():
            parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="{key}"\r\n\r\n{value}\r\n'.encode())
        filename, content = multipart
        parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{filename}"\r\nContent-Type: application/octet-stream\r\n\r\n'.encode() + content + b'\r\n')
        parts.append(f'--{boundary}--\r\n'.encode())
        data = b''.join(parts)
        headers['Content-Type'] = f'multipart/form-data; boundary={boundary}'
    elif fields is not None:
        data = urllib.parse.urlencode(fields).encode()
    request = urllib.request.Request(BASE + path, data=data, headers=headers)
    try:
        with CLIENT.open(request, timeout=20) as response:
            return response.status, response.read().decode(), response.url
    except urllib.error.HTTPError as error:
        body = error.read().decode()
        # Report rendered errors without credentials or request bodies.
        messages = re.findall(r'<p class="error">(.*?)</p>', body, re.S)
        raise AssertionError(f'{path}: HTTP {error.code}; ' + '; '.join(html.unescape(v) for v in messages)) from None


def csrf(body):
    match = re.search(r'name="authenticity_token" value="([^"]+)"', body)
    if not match:
        match = re.search(r'name="csrf-token" content="([^"]+)"', body)
    assert match, 'Missing CSRF token'
    return html.unescape(match.group(1))


def wait_ticket(issue_id, required):
    for _ in range(30):
        try:
            _, body, _ = fetch(f'/esa/tickets/{issue_id}')
            if required in body:
                return body
        except AssertionError:
            pass
        time.sleep(1)
    raise AssertionError(f'Issue {issue_id} did not reach {required!r}')


def wait_issue(source_key):
    for _ in range(30):
        _, body, _ = fetch('/esa?' + urllib.parse.urlencode({'q': source_key}))
        links = re.findall(r'href="/esa/tickets/(\d+)"', body)
        if links:
            return links[0]
        time.sleep(1)
    raise AssertionError('Imported issue was not synchronized')


def upload(project_id, filename, content):
    _, body, _ = fetch('/esa/upload')
    _, preview, _ = fetch('/esa/preview', {'authenticity_token': csrf(body), 'project_id': project_id}, (filename, content))
    assert 'Import preview' in preview
    action = re.search(r'action="(/esa/import/\d+)"', preview)
    assert action, 'Missing import action'
    _, result, _ = fetch(action.group(1), {'authenticity_token': csrf(preview)})
    assert 'Imported ' in result
    return result


def main():
    credentials = (ROOT / 'deploy/redmine/credentials.md').read_text()
    password = re.search(r'^Password: (.+)$', credentials, re.M).group(1)
    _, body, _ = fetch('/login')
    _, body, url = fetch('/login', {'authenticity_token': csrf(body), 'username': 'admin', 'password': password})
    assert '/login' not in url, 'Administrator login failed'
    _, body, _ = fetch('/esa')
    assert '<h2>ESA</h2>' in body and 'Upload tickets' in body
    _, body, _ = fetch('/esa/upload')
    project = re.search(r'<option value="(\d+)">ESA Demo</option>', body)
    assert project, 'ESA Demo project missing'
    project_id = project.group(1)
    stamp = str(time.time_ns())
    case = json.loads((ROOT / 'examples/resolved-case.json').read_text())
    case['source_key'] = 'VERIFY-CASE-' + stamp
    case['id'] = case['source_key']
    case['subject'] = f'Consumer crashes with NullPointerException [{stamp}]'
    body = upload(project_id, 'resolved.json', json.dumps(case).encode())
    case_id = wait_issue(case['source_key'])
    body = wait_ticket(case_id, 'Collected case')
    _, body, _ = fetch(f'/esa/tickets/{case_id}/review_case', {'authenticity_token': csrf(body), 'review_action': 'approve', 'note': 'Synthetic validation: confirmed evidence and applicability.'})
    assert 'Trusted' in body and 'Case review recorded' in body
    issue = dict(case)
    issue.update(id='VERIFY-OPEN-' + stamp, source_key='VERIFY-OPEN-' + stamp, subject=f'Consumer crashes with NullPointerException [new {stamp}]', closed=False, status='New', resolution={'reason':'','cause':'','solution':'','validation':''})
    body = upload(project_id, 'new.json', json.dumps(issue).encode())
    new_id = wait_issue(issue['source_key'])
    body = wait_ticket(new_id, 'Case Reuse')
    assert 'Applicable resolved cases were found' in body
    assert re.search(r'Provider calls:</strong>\s*0', body), 'Case reuse spent a provider call'
    _, filtered, _ = fetch('/esa?' + urllib.parse.urlencode({'project_id': project_id, 'q': issue['source_key'], 'route': 'case_reuse', 'sort': 'subject', 'direction': 'asc'}))
    assert issue['source_key'] in filtered and case['source_key'] not in filtered
    # Repeated source keys must not create or overwrite issues.
    body = upload(project_id, 'duplicate.json', json.dumps(issue).encode())
    assert 'Imported 0 tickets; skipped 1' in body
    # Native Redmine update must enter the durable outbox through the hook.
    _, body, _ = fetch(f'/issues/{case_id}/edit')
    status_options = re.search(r'<select[^>]*id="issue_status_id"[^>]*>(.*?)</select>', body, re.S)
    assert status_options, 'Native status selector missing'
    new_status = re.search(r'<option[^>]*value="(\d+)"[^>]*>New</option>', status_options.group(1))
    assert new_status, 'New status missing'
    fetch(f'/issues/{case_id}', {'authenticity_token': csrf(body), '_method': 'patch', 'issue[status_id]': new_status.group(1), 'issue[notes]': 'Synthetic validation: reopen the source case.'})
    body = wait_ticket(new_id, 'Invalidated')
    assert 'withdrawn' in body
    # Exercise the CSV preview/import and non-engineering route.
    csv = (ROOT / 'examples/new-issues.csv').read_text().replace('DEMO-2', 'VERIFY-CSV-2-' + stamp).replace('DEMO-3', 'VERIFY-CSV-3-' + stamp)
    upload(project_id, 'tickets.csv', csv.encode())
    for _ in range(20):
        _, body, _ = fetch('/esa?' + urllib.parse.urlencode({'q':'VERIFY-CSV-3-' + stamp, 'route':'customer_support'}))
        if 'VERIFY-CSV-3-' + stamp in body:
            break
        time.sleep(1)
    else:
        raise AssertionError('Usage question was not routed to customer support')
    evidence = {'checked_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()), 'project_id': project_id, 'case_issue_id': case_id, 'new_issue_id': new_id, 'checks': ['English navigation', 'JSON preview and import', 'Case approval', 'Automatic recommendation without provider calls', 'Server filters and sorting', 'Source-key deduplication', 'Native Redmine update outbox', 'Reopen invalidation', 'CSV preview and import', 'Non-engineering customer-support routing']}
    report = ROOT / 'reports/esa-go-verification.json'
    report.write_text(json.dumps(evidence, indent=2) + '\n')
    print(json.dumps(evidence, indent=2))


if __name__ == '__main__':
    main()
