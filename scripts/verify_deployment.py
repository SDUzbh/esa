#!/usr/bin/env python3
"""Exercise the real Redmine deployment using HTTP sessions and a fixture provider."""
import argparse
from html.parser import HTMLParser
from http.cookiejar import CookieJar
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import subprocess
import time
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import HTTPCookieProcessor, ProxyHandler, Request, build_opener
import uuid

ROOT = Path(__file__).resolve().parents[1]


class Page(HTMLParser):
    """Read Rails CSRF and hidden fields without executing browser scripts."""
    def __init__(self, source):
        super().__init__()
        self.fields, self.csrf = {}, ''
        self.feed(source)

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == 'input' and 'name' in attrs:
            self.fields[attrs['name']] = attrs.get('value', '')
        if tag == 'meta' and attrs.get('name') == 'csrf-token':
            self.csrf = attrs.get('content', '')


class Session:
    def __init__(self, base):
        self.base = base
        self.client = build_opener(ProxyHandler({}), HTTPCookieProcessor(CookieJar()))

    def request(self, path, data=None):
        request = Request(self.base + path, data=None if data is None else urlencode(data).encode())
        try:
            response = self.client.open(request, timeout=20)
        except HTTPError as error:
            response = error
        with response:
            return response.code, response.read().decode(), response.geturl()

    def post(self, path, data, page):
        form = Page(page)
        token = form.csrf or form.fields.get('authenticity_token')
        assert token, 'Rails CSRF token is missing'
        return self.request(path, dict(data, authenticity_token=token))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--context', help='Docker context; defaults to the current context')
    parser.add_argument('--output', required=True, help='New private report directory')
    args = parser.parse_args()
    output = Path(args.output).resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    artifacts, private = output / 'artifacts', output / 'private'
    artifacts.mkdir()
    private.mkdir(mode=0o700)
    project = 'esa-deploy-' + uuid.uuid4().hex[:12]
    docker = ['docker'] + (['--context', args.context] if args.context else [])
    env = dict(os.environ, DOCKER_CONTEXT=args.context or os.environ.get('DOCKER_CONTEXT', ''))
    if not env['DOCKER_CONTEXT']:
        env.pop('DOCKER_CONTEXT')
    password = secrets.token_urlsafe(24)
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', 0))
        port = probe.getsockname()[1]
    settings = dict(REDMINE_DB_PASSWORD=secrets.token_hex(24), ESA_DB_PASSWORD=secrets.token_hex(24), SECRET_KEY_BASE=secrets.token_hex(64),
                    ESA_API_TOKEN=secrets.token_hex(24), ESA_TEST_PASSWORD=password,
                    ESA_UID='1000', ESA_GID='1000', ESA_STATE_URL='', ESA_ANALYZER_URL='',
                    ESA_TEST_PROJECT=project, ESA_TEST_PORT=str(port))
    env.update(settings)
    env_file = private / 'compose.env'
    env_file.write_text('\n'.join(k + '=' + v for k, v in settings.items()) + '\n')
    env_file.chmod(0o600)
    plugin = subprocess.run(docker + ['compose', 'version'], env=env, capture_output=True).returncode == 0
    compose = docker + ['compose'] if plugin else [shutil.which('docker-compose') or 'docker-compose']
    compose += ['--env-file', str(env_file), '-p', project,
                '-f', str(ROOT / 'deploy/redmine/compose.yaml'),
                '-f', str(ROOT / 'deploy/redmine/compose.test.yaml')]
    result = {'status': 'failed', 'project': project, 'real_agent_calls': 0, 'checks': []}

    def run(command, **kwargs):
        completed = subprocess.run(command, cwd=ROOT, env=env, text=True, capture_output=True,
                                   timeout=kwargs.pop('timeout', 90), **kwargs)
        if completed.returncode:
            raise RuntimeError('Command failed: ' + ' '.join(command[:3]) + '\n' + completed.stderr[-3000:])
        return completed.stdout

    def check(name, condition):
        assert condition, name
        result['checks'].append(name)

    def rails(code):
        return run(compose + ['exec', '-T', 'redmine', 'bundle', 'exec', 'rails', 'runner', '-'],
                   input=code, timeout=120)

    def audit():
        text = rails("puts 'ESA_TEST_AUDIT=' + {issues: Issue.count, sources: EsaTicketSource.count, events: EsaEvent.count, acknowledged: EsaEvent.where.not(acknowledged_at: nil).count}.to_json")
        return json.loads(next(line.split('=', 1)[1] for line in text.splitlines() if line.startswith('ESA_TEST_AUDIT=')))

    def provider_calls():
        return json.loads(run(compose + ['exec', '-T', 'fixture', 'cat', '/reports/provider-calls.json']))

    def stopped(*_):
        raise KeyboardInterrupt('Deployment check interrupted')

    signal.signal(signal.SIGTERM, stopped)
    try:
        assert (ROOT / 'bin/esa').is_file(), 'Build ESA with scripts/build_esa.sh first'
        config = json.loads(run(compose + ['config', '--format', 'json']))
        check('isolated_project_and_no_external_agent', config['name'] == project and
              config['services']['esa']['environment']['ESA_ANALYZER_URL'] == 'http://fixture:8091/analyze' and
              config['networks']['default']['internal'] is True and
              all(set(config['services'][service]['networks']) == {'default'} for service in ['db', 'esa-db-init', 'esa', 'fixture']) and
              all(mount['type'] == 'volume' for service in config['services'].values() for mount in service.get('volumes', [])))
        run(compose + ['up', '-d', '--build', '--wait', '--wait-timeout', '360'], timeout=720)
        check('real_services_healthy', True)
        seeded = rails((ROOT / 'scripts/seed_deployment_test.rb').read_text())
        seed = json.loads(next(line.split('=', 1)[1] for line in seeded.splitlines() if line.startswith('ESA_TEST_SEED=')))
        check('plugin_migrations_and_test_roles', True)
        # Docker health checks precede host/VM port-forward readiness on macOS.
        for _ in range(60):
            try:
                if Session('http://127.0.0.1:' + str(port)).request('/login')[0] == 200:
                    break
            except OSError:
                pass
            time.sleep(1)
        else:
            raise AssertionError('Published Redmine HTTP port is unavailable')
        sessions = {}
        for role in ['support', 'engineer', 'outsider']:
            session = Session('http://127.0.0.1:' + str(port))
            status, page, _ = session.request('/login')
            assert status == 200
            status, page, url = session.post('/login', {'username': 'deployment_' + role, 'password': password}, page)
            check(role + '_login', status == 200 and '/login' not in url)
            sessions[role] = session
        support, engineer = sessions['support'], sessions['engineer']
        status, page, _ = support.request('/projects/deployment-check/issues/new')
        assert status == 200
        status, page, url = support.post('/issues', {
            'issue[project_id]': seed['project_id'], 'issue[tracker_id]': seed['tracker_id'],
            'issue[status_id]': seed['status_id'], 'issue[priority_id]': seed['priority_id'],
            'issue[subject]': 'Deployment fixture: empty assignment',
            'issue[description]': 'Synthetic fixture for deployment workflow verification.'}, page)
        match = re.search(r'/issues/(\d+)$', url)
        check('support_creates_issue_through_redmine', status == 200 and bool(match))
        issue = match.group(1)
        ticket = '/esa/tickets/' + issue
        status, page, _ = support.request(ticket)
        check('create_does_not_forward', status == 200 and audit()['events'] == 0)
        metadata = {'components': 'consumer', 'versions': '1.0', 'environment': '{"jdk":"fixture"}'}
        for _ in range(2):
            status, page, _ = support.post(ticket + '/metadata', metadata, page)
            assert status == 200
        saved = audit()
        check('metadata_saves_update_one_record_without_forwarding', saved['sources'] == 1 and saved['events'] == 0)
        status, page, _ = support.post(ticket + '/forward', {}, page)
        check('support_forwards_through_plugin', status == 200)
        for _ in range(90):
            status, body, _ = support.request(ticket + '/status')
            if status == 200 and 'awaiting review' in json.loads(body)['html'].lower():
                break
            time.sleep(1)
        else:
            raise AssertionError('Forwarded issue did not reach Awaiting Review')
        check('live_status_endpoint_shows_report', 'View ESA report' in json.loads(body)['html'])
        status, body, _ = support.request(ticket + '/metadata_suggestion')
        check('metadata_suggestions_available', status == 200 and 'Guard assignment' in json.loads(body)['values']['solution'])
        status, report, _ = engineer.request(ticket + '/report')
        check('engineer_sees_findings_and_review_form', status == 200 and 'Fixture empty assignment hypothesis' in report and 'Publish reviewed solution' in report)
        solution = Page(report).fields['solution_id']
        status, support_report, _ = support.request(ticket + '/report')
        check('support_cannot_approve_solution', support.post(ticket + '/review_investigation', {'solution_id': solution, 'review_action': 'approve'}, support_report)[0] == 403)
        check('engineer_cannot_forward', engineer.post(ticket + '/forward', {}, report)[0] == 403)
        check('outsider_cannot_view_private_project', sessions['outsider'].request(ticket)[0] in (403, 404))
        status, report, _ = engineer.post(ticket + '/review_investigation', {
            'solution_id': solution, 'review_action': 'approve', 'cause': 'Reviewed synthetic cause',
            'solution': 'Reviewed synthetic correction', 'validation': 'Deployment fixture only'}, report)
        assert status == 200
        status, report, _ = engineer.request(ticket + '/report')
        check('engineer_publishes_reviewed_revision', status == 200 and 'Reviewed synthetic correction' in report)
        solution = Page(report).fields['solution_id']
        before = audit()
        check('outbox_acknowledged_once', before['events'] == before['acknowledged'] == 1)
        calls = provider_calls()
        check('fixture_pipeline_called_once', [call['stage'] for call in calls] == ['route', 'engineer', 'assign'])
        run(compose + ['up', '-d', '--force-recreate', '--no-deps', '--wait', '--wait-timeout', '360', 'redmine', 'esa'], timeout=420)
        status, report, _ = engineer.request(ticket + '/report')
        check('recreate_preserves_database_state_and_review', status == 200 and audit() == before and
              Page(report).fields.get('solution_id') == solution and 'Reviewed synthetic correction' in report)
        check('recreate_does_not_repeat_analysis', provider_calls() == calls)
        result['status'] = 'passed'
    except BaseException as error:
        result['error'] = type(error).__name__ + ': ' + str(error)
    finally:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        try:
            logs = run(compose + ['logs', '--no-color'], timeout=30)
            # Test passwords and service tokens never belong in uploaded logs.
            for secret in [password, settings['ESA_API_TOKEN'], settings['REDMINE_DB_PASSWORD'], settings['ESA_DB_PASSWORD'], settings['SECRET_KEY_BASE']]:
                logs = logs.replace(secret, '[redacted]')
            (artifacts / 'compose.log').write_text(logs)
        except Exception as error:
            result['log_error'] = str(error)
        try:
            run(compose + ['down', '--volumes', '--remove-orphans'], timeout=120)
            for kind, command in [('containers', ['ps', '-aq']), ('volumes', ['volume', 'ls', '-q']), ('networks', ['network', 'ls', '-q'])]:
                assert not run(docker + command + ['--filter', 'label=com.docker.compose.project=' + project]).strip(), 'Owned ' + kind + ' survived cleanup'
            for service in ['redmine', 'esa-db-init', 'esa', 'fixture']:
                image = project + '-' + service
                inspected = subprocess.run(docker + ['image', 'inspect', image], env=env, capture_output=True, timeout=30)
                if inspected.returncode == 0:
                    run(docker + ['image', 'rm', image], timeout=30)
            result['cleanup'] = 'completed'
        except Exception as error:
            result['cleanup'], result['cleanup_error'], result['status'] = 'failed', str(error), 'failed'
        (artifacts / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result))
    return 0 if result['status'] == 'passed' else 1


if __name__ == '__main__':
    raise SystemExit(main())
