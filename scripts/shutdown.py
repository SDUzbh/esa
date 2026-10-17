#!/usr/bin/env python3
"""Stop this checkout's ESA runtimes and remove owned temporary Docker resources."""
import argparse
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[1]
SESSION_LABELS = ('esa.replay.session', 'system.replay.session')
# scripts/test_postgres.sh labels its disposable test database with this.
TEST_DATABASE_LABEL = 'esa.test.database'
TEST_DATABASE_FILES = ROOT / '.tools/test-postgres'
DEPLOYMENT = re.compile(r'esa-deploy-[0-9a-f]{12}\Z')
BINARIES = {'esa', 'esa-host', 'esa-codex', 'esa-replay', 'system-replay'}


def command(args, check=True, timeout=60):
    """Capture output privately; never print process argv or deployment secrets."""
    result = subprocess.run(args, text=True, capture_output=True, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError(args[0] + ' ' + args[1] + ' failed; shutdown is incomplete')
    return result


def processes():
    rows = command(['ps', '-axo', 'pid=,ppid=,command=']).stdout.splitlines()
    return {int(p[0]): (int(p[1]), p[2]) for row in rows
            if len(p := row.strip().split(None, 2)) == 3}


def working_directory(pid):
    if sys.platform.startswith('linux'):
        try:
            return Path(os.readlink('/proc/%s/cwd' % pid))
        except OSError:
            return None
    result = command(['lsof', '-a', '-p', str(pid), '-d', 'cwd', '-Fn'], check=False)
    return next((Path(line[1:]) for line in result.stdout.splitlines() if line.startswith('n')), None)


def runtime_process(argv, cwd):
    """Match executable/script positions, never arbitrary argument substrings."""
    if not argv:
        return False
    entry = argv[0]
    name = Path(entry).name
    go_run = name == 'go' and len(argv) > 2 and argv[1] == 'run'
    if go_run:
        entry = argv[2]
    if name.lower().startswith('python') or name in ('sh', 'bash', 'zsh'):
        rest = argv[1:]
        if '-c' in rest or '-m' in rest:
            return False
        entry = next((part for part in rest if not part.startswith('-')), '')
    path = Path(entry)
    if not entry or (not path.is_absolute() and cwd is None):
        return False
    path = (path if path.is_absolute() else cwd / path).resolve()
    if go_run:
        return path.parent == ROOT / 'cmd' and path.name in BINARIES
    if path.parent == ROOT / 'bin':
        return path.name in BINARIES
    if path.parent == ROOT / 'scripts':
        return (path.name in {'analyze_ticket.py', 'replay_pipeline.py', 'replay_session.py',
                             'replay_trial.py', 'esa_fixture_target.py', 'check_workflow.sh'}
                or (path.name.startswith('verify_') and path.suffix == '.py'))
    return path == ROOT / 'system_replay/runtime_driver.py'


def stop_processes(dry_run, timeout):
    table = processes()
    ancestors = {os.getpid()}
    parent = os.getpid()
    while parent in table and table[parent][0] not in ancestors:
        parent = table[parent][0]
        ancestors.add(parent)
    owned = set()
    for pid, (_, text) in table.items():
        if pid in ancestors:
            continue
        try:
            argv = shlex.split(text)
        except ValueError:
            continue
        # Read cwd only for possible ESA entry points, not every application.
        if not any(Path(arg).name in BINARIES or arg.endswith(('.py', 'check_workflow.sh')) for arg in argv[:4]):
            continue
        if runtime_process(argv, working_directory(pid)):
            owned.add(pid)
    roots = set(owned)
    while True:
        children = {pid for pid, (parent, _) in table.items() if parent in owned and pid not in ancestors}
        if children <= owned:
            break
        owned.update(children)
    print(('Would stop' if dry_run else 'Stopping') + ' ESA host process IDs: ' +
          (', '.join(map(str, sorted(owned))) or 'none'), flush=True)
    if dry_run or not owned:
        return

    def alive():
        current = processes()
        # Children may be reparented when their owner exits.
        return {pid for pid in owned if pid in current and current[pid][1] == table[pid][1]}

    def send(pids, sig):
        for pid in pids & alive():
            try:
                os.kill(pid, sig)
            except ProcessLookupError:
                pass

    def wait(seconds):
        end = time.monotonic() + seconds
        while alive() and time.monotonic() < end:
            time.sleep(.2)

    # Give owners time to cancel model calls and close their environment helpers.
    send(roots, signal.SIGTERM)
    wait(timeout)
    send(alive(), signal.SIGTERM)
    wait(5)
    if alive():
        print('Force-stopping unresponsive ESA child processes.', flush=True)
        send(alive(), signal.SIGKILL)
        wait(5)
    if alive():
        raise RuntimeError('ESA processes survived shutdown')


def resource_scope(labels):
    if any(labels.get(key) for key in SESSION_LABELS + (TEST_DATABASE_LABEL,)):
        return 'temporary'
    project = labels.get('com.docker.compose.project', '')
    if DEPLOYMENT.fullmatch(project):
        return 'temporary'
    return 'persistent' if project == 'esa-redmine' else 'foreign'


class Docker:
    def __init__(self, context, dry_run):
        self.prefix = ['docker', '--context', context]
        self.dry_run = dry_run

    def read(self, *args):
        return command(self.prefix + list(args)).stdout.strip()

    def change(self, *args):
        print(('Would run: ' if self.dry_run else 'Running: ') + 'docker ' + ' '.join(args), flush=True)
        if not self.dry_run:
            self.read(*args)

    def resources(self, kind):
        """Inspect labels only; do not read container environment variables."""
        args = ['container', 'ls', '-a', '-q'] if kind == 'container' else [kind, 'ls', '-q']
        for name in self.read(*args).splitlines():
            template = '{{json .Config.Labels}}' if kind == 'container' else '{{json .Labels}}'
            result = command(self.prefix + [kind, 'inspect', '--format', template, name], check=False)
            if result.returncode:
                # A graceful owner may have removed the resource during inspection.
                if 'no such' in result.stderr.lower():
                    continue
                raise RuntimeError('Cannot inspect Docker resource ownership')
            yield name, json.loads(result.stdout) or {}


def clean_docker(docker):
    """Returns whether a disposable test database was removed."""
    projects = set()
    test_database = False
    containers = list(docker.resources('container'))
    # Stop producers and consumers before the persistent database.
    priority = {'esa': 0, 'redmine': 1, 'db': 2}
    containers.sort(key=lambda row: priority.get(row[1].get('com.docker.compose.service'), 3))
    for name, labels in containers:
        scope = resource_scope(labels)
        if scope == 'persistent':
            docker.change('stop', '--time', '30', name)
        elif scope == 'temporary':
            docker.change('rm', '--force', '--volumes', name)
            test_database = test_database or bool(labels.get(TEST_DATABASE_LABEL))
            if DEPLOYMENT.fullmatch(labels.get('com.docker.compose.project', '')):
                projects.add(labels['com.docker.compose.project'])
    for kind in ('volume', 'network'):
        for name, labels in docker.resources(kind):
            if resource_scope(labels) == 'temporary':
                docker.change(kind, 'rm', name)
                if DEPLOYMENT.fullmatch(labels.get('com.docker.compose.project', '')):
                    projects.add(labels['com.docker.compose.project'])
    # Remove disposable deployment tags only, retaining release/base/build caches.
    for project in sorted(projects):
        for service in ('redmine', 'esa-db-init', 'esa', 'fixture'):
            tag = project + '-' + service + ':latest'
            if docker.read('image', 'ls', '-q', tag):
                docker.change('image', 'rm', tag)
    if not docker.dry_run:
        for kind in ('container', 'volume', 'network'):
            for name, labels in docker.resources(kind):
                if resource_scope(labels) == 'temporary':
                    raise RuntimeError('Temporary Docker resource survived cleanup: ' + name)
        if docker.read('container', 'ls', '-q', '--filter', 'label=com.docker.compose.project=esa-redmine'):
            raise RuntimeError('Redmine services survived shutdown')
    return test_database


def clean_session_directories(context, dry_run):
    """Delete only stale, generated session directories in this checkout."""
    for parent in (ROOT / '.esa/replay-sessions', ROOT / 'system_replay/.runtime/sessions'):
        for marker in parent.glob('*/session.json'):
            saved = json.loads(marker.read_text())
            session = saved.get('session', '')
            if saved.get('context') != context:
                continue
            if (not re.fullmatch('[0-9a-f]{32}', session) or
                    not marker.parent.name.startswith(session + '-') or marker.parent.is_symlink()):
                raise RuntimeError('Invalid session ownership marker')
            try:
                os.kill(int(saved['pid']), 0)
            except ProcessLookupError:
                print(('Would remove: ' if dry_run else 'Removing: ') + str(marker.parent), flush=True)
                if not dry_run:
                    shutil.rmtree(marker.parent)
            else:
                if not dry_run:
                    raise RuntimeError('Session owner is still alive; directory retained')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--context', default='colima-redmine', help='Docker context to clean')
    parser.add_argument('--dry-run', action='store_true', help='Inspect without stopping or deleting anything')
    parser.add_argument('--keep-vm', action='store_true', help='Leave Colima running after cleanup')
    parser.add_argument('--timeout', type=int, default=30, help='Graceful host shutdown timeout in seconds')
    args = parser.parse_args()
    if args.timeout < 1:
        parser.error('--timeout must be positive')
    stop_processes(args.dry_run, args.timeout)
    profile = args.context.removeprefix('colima-') if args.context.startswith('colima-') else None
    docker = Docker(args.context, args.dry_run)
    if command(docker.prefix + ['info'], check=False).returncode:
        if profile and shutil.which('colima'):
            rows = command(['colima', 'list', '--json']).stdout.splitlines()
            entries = [json.loads(row) for row in rows if row.strip()]
            if any(row.get('name') == profile and row.get('status', '').lower() == 'stopped' for row in entries):
                print('Colima is already stopped. No Docker runtime is active; orphan cleanup requires a running daemon.')
                return 0
        raise RuntimeError('Docker is unavailable; resource cleanup could not be verified')
    if clean_docker(docker) and TEST_DATABASE_FILES.exists():
        # Its password and URL are useless once the container is gone.
        print(('Would remove: ' if args.dry_run else 'Removing: ') + str(TEST_DATABASE_FILES), flush=True)
        if not args.dry_run:
            shutil.rmtree(TEST_DATABASE_FILES)
    clean_session_directories(args.context, args.dry_run)
    if profile and not args.keep_vm:
        # A dedicated ESA VM normally has no remaining running containers.
        running = set(docker.read('container', 'ls', '-q').splitlines())
        foreign = any(name in running and resource_scope(labels) == 'foreign'
                      for name, labels in docker.resources('container'))
        if foreign:
            print('Other containers are running; Colima was left running.')
        else:
            print(('Would run: ' if args.dry_run else 'Running: ') + 'colima stop --profile ' + profile, flush=True)
            if not args.dry_run:
                command(['colima', 'stop', '--profile', profile], timeout=90)
    print('Shutdown preview complete.' if args.dry_run else 'ESA runtimes stopped; persistent data, reports and caches retained.')
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print('Shutdown failed: ' + str(error), file=sys.stderr)
        raise SystemExit(1)
