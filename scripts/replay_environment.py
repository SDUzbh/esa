"""Reusable Docker replay environment; standard library only, no model calls."""
from contextlib import AbstractContextManager
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import tarfile
import tempfile
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_IMAGE = 'eclipse-temurin:8-jdk'


def digest(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(chunk)
    return h.hexdigest()


def write_json(path, value):
    path = Path(path); path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + '.tmp')
    temporary.write_text(json.dumps(value, indent=2) + '\n')
    temporary.replace(path)


def fetch(url, target, algorithm='sha256', checksum_url=None):
    """Validate a cached file every time; download atomically if absent."""
    target = Path(target); target.parent.mkdir(parents=True, exist_ok=True)
    lock = target.with_suffix(target.suffix + '.sha256')
    if target.exists() and lock.exists():
        if digest(target) != lock.read_text().strip():
            raise RuntimeError('Cached artifact checksum mismatch: ' + target.name)
        return target
    temporary = target.with_suffix(target.suffix + '.download')
    try:
        with urllib.request.urlopen(url, timeout=60) as response, temporary.open('wb') as output:
            shutil.copyfileobj(response, output, 1024 * 1024)
        if checksum_url:
            with urllib.request.urlopen(checksum_url, timeout=30) as response:
                checksum = response.read().decode()
            h = hashlib.new(algorithm)
            with temporary.open('rb') as stream:
                for chunk in iter(lambda: stream.read(1024 * 1024), b''): h.update(chunk)
            # Apache's old .sha2 format prints one digest in spaced 8-digit groups.
            published = ''.join(re.findall(r'\b[0-9a-fA-F]{8,128}\b', checksum)).lower()
            if h.hexdigest().lower() not in published:
                raise RuntimeError('Official artifact checksum mismatch: ' + target.name)
        else:
            raise RuntimeError('An official checksum is required for new artifacts')
        temporary.replace(target); lock.write_text(digest(target) + '\n')
        return target
    finally:
        temporary.unlink(missing_ok=True)


def dependencies(build_file, cache):
    text = Path(build_file).read_text(); result = {}
    for group, artifact in [('org.slf4j', 'slf4j-api'), ('org.xerial.snappy', 'snappy-java'), ('net.jpountz.lz4', 'lz4')]:
        match = re.search(re.escape(group + ':' + artifact + ':') + r'([^\'"\s]+)', text)
        if not match: raise RuntimeError('Historical dependency definition missing: ' + artifact)
        version = match.group(1)
        if not re.fullmatch(r'[\w.\-]+', version): raise RuntimeError('Unsupported dependency expression')
        name = artifact + '-' + version + '.jar'
        url = 'https://repo.maven.apache.org/maven2/' + group.replace('.', '/') + '/' + artifact + '/' + version + '/' + name
        path = fetch(url, Path(cache) / 'dependencies' / name, 'sha1', url + '.sha1')
        result[name] = {'path': str(path), 'sha256': digest(path), 'source': url}
    return result


def kafka_binary(version, scala, cache):
    if not re.fullmatch(r'\d+\.\d+\.\d+(?:\.\d+)?(?:-beta\d*)?', version):
        raise RuntimeError('A published numeric Kafka version is required for binary installation')
    if not re.fullmatch(r'\d+\.\d+(?:\.\d+)?', scala): raise RuntimeError('Invalid Scala binary version')
    name = 'kafka_' + scala + '-' + version
    url = 'https://archive.apache.org/dist/kafka/' + version + '/' + name + '.tgz'
    # Old Apache releases publish SHA-512 in a .sha2 file.
    archive = fetch(url, Path(cache) / 'artifacts' / (name + '.tgz'), 'sha512', url + '.sha2')
    out = Path(cache) / 'products' / name
    marker = out / '.esa-artifact-sha256'
    if marker.exists() and marker.read_text().strip() == digest(archive): return out
    temporary = Path(tempfile.mkdtemp(prefix=name + '-', dir=str(archive.parent)))
    try:
        with tarfile.open(archive) as entries:
            for member in entries:
                path = Path(member.name)
                if path.is_absolute() or '..' in path.parts or not (member.isdir() or member.isfile()):
                    raise RuntimeError('Unsupported archive entry: ' + member.name)
                target = temporary / path
                if member.isdir(): target.mkdir(parents=True, exist_ok=True)
                else:
                    target.parent.mkdir(parents=True, exist_ok=True)
                    with entries.extractfile(member) as source, target.open('wb') as destination: shutil.copyfileobj(source, destination)
                    target.chmod(0o755 if member.mode & 0o111 else 0o644)
        product = temporary / name
        if out.exists(): shutil.rmtree(out)
        out.parent.mkdir(parents=True, exist_ok=True); product.replace(out)
        marker.write_text(digest(archive) + '\n')
    finally: shutil.rmtree(temporary, ignore_errors=True)
    return out


class Environment(AbstractContextManager):
    """One owned container per pipeline; source and dependency updates are deltas."""
    def __init__(self, cache=None, context='colima-redmine', image=DEFAULT_IMAGE, profile=None):
        self.cache = Path(cache or ROOT / '.esa/replay-cache').resolve()
        self.docker = ['docker'] + (['--context', context] if context else [])
        self.context = context; self.image = image; self.session = uuid.uuid4().hex
        self.name = 'esa-replay-' + self.session
        self.writer = self.name + '-writer'; self.volume = self.name + '-data'
        self.directory = None; self.started = False; self.source_hashes = {}; self.input_hashes = {}
        self.jdk_digest = None; self.old_handlers = {}; self.base_processes = ['1']; self.transitions = 0
        self.preset = None
        if profile:
            if image != DEFAULT_IMAGE:
                raise ValueError('An explicit historical image cannot be replaced by a preset Profile')
            from tool_images import selection
            entry, manifest = selection(profile, context)
            self.image = entry['image_id']
            self.preset = {'profile': profile, 'image_id': entry['image_id'], 'tag': entry['tag'], 'release_hash': manifest['release_hash'], 'toolset_hash': entry['toolset_hash'], 'inventory': entry['inventory']}

    def command(self, args, **kwargs):
        kwargs.setdefault('timeout', 120)
        result = subprocess.run(self.docker + args, text=True, capture_output=True, **kwargs)
        if result.returncode: raise RuntimeError('Docker ' + args[0] + ' failed: ' + result.stderr.strip())
        return result.stdout.strip()

    def __enter__(self):
        sessions = ROOT / '.esa/replay-sessions'; sessions.mkdir(parents=True, exist_ok=True)
        self.directory = Path(tempfile.mkdtemp(prefix=self.session + '-', dir=str(sessions)))
        self.directory.chmod(0o755)
        for name in ('source', 'input'): (self.directory / name).mkdir(mode=0o755)
        (self.directory / 'input/source').mkdir(mode=0o755)
        self.cache.mkdir(parents=True, exist_ok=True)
        write_json(self.directory / 'session.json', {'container':self.name, 'session':self.session, 'pid':os.getpid(), 'context':self.context, 'writer':self.writer, 'volume':self.volume})
        for sig in (signal.SIGINT, signal.SIGTERM):
            self.old_handlers[sig] = signal.getsignal(sig)
            signal.signal(sig, self._interrupt)
        try:
            if self.preset:
                image = json.loads(self.command(['image', 'inspect', self.image]))[0]
                labels = image['Config'].get('Labels', {})
                if image['Id'] != self.preset['image_id'] or labels.get('esa.release.hash') != self.preset['release_hash'] or labels.get('esa.toolkit') != 'release':
                    raise RuntimeError('Preset image identity or release labels differ')
                self.jdk_digest = image['Id']
            else:
                inspected = subprocess.run(self.docker + ['image', 'inspect', self.image, '--format', '{{index .RepoDigests 0}}'], text=True, capture_output=True)
                if inspected.returncode: self.command(['pull', self.image]); inspected = subprocess.run(self.docker + ['image', 'inspect', self.image, '--format', '{{index .RepoDigests 0}}'], check=True, text=True, capture_output=True)
                self.jdk_digest = inspected.stdout.strip()
                if '@sha256:' not in self.jdk_digest: raise RuntimeError('Runtime image needs a pinned repository digest')
            # Mark ownership before starting: cleanup also handles a cancelled run request.
            self.started = True
            self.command(['volume', 'create', '--label', 'esa.replay.session=' + self.session, self.volume])
            self.command(['run', '-d', '--rm', '--name', self.writer, '--label', 'esa.replay.session=' + self.session,
                          '--user', '0:0',
                          '--network', 'none', '--read-only', '--tmpfs', '/tmp:rw,nosuid,size=64m',
                          '--memory', '128m', '--cpus', '0.25', '--pids-limit', '32', '--cap-drop', 'ALL',
                          '--security-opt', 'no-new-privileges',
                          '--mount', 'type=volume,src=' + self.volume + ',dst=/state',
                          '--entrypoint', 'sleep', self.jdk_digest, 'infinity'])
            self.command(['run', '-d', '--rm', '--name', self.name, '--label', 'esa.replay.session=' + self.session,
                          '--init', '--network', 'none', '--read-only', '--user', '1000:1000',
                          '--tmpfs', '/tmp:rw,nosuid,size=512m,mode=1777',
                          '--tmpfs', '/workspace:rw,nosuid,size=1g,mode=1777',
                          '--memory', '2g', '--cpus', '2', '--pids-limit', '256', '--cap-drop', 'ALL',
                          '--security-opt', 'no-new-privileges',
                          '--mount', 'type=volume,src=' + self.volume + ',dst=/input,readonly',
                          '--entrypoint', 'sleep', self.jdk_digest, 'infinity'])
            self.base_processes += self.command(['exec', self.name, 'cat', '/proc/1/task/1/children']).split()
            return self
        except BaseException:
            self.close(); raise

    def _interrupt(self, signum, frame):
        raise KeyboardInterrupt('Replay interrupted by signal ' + str(signum))

    def __exit__(self, kind, value, traceback): self.close(); return False

    def close(self):
        # Disable repeat signals during cleanup, then restore the caller's handlers.
        for sig in self.old_handlers: signal.signal(sig, signal.SIG_IGN)
        try:
            if self.started:
                for name in (self.name, self.writer): remove_owned(self.docker, 'container', name, self.session)
                remove_owned(self.docker, 'volume', self.volume, self.session)
                self.started = False
            if self.directory and self.directory.exists(): shutil.rmtree(self.directory)
        finally:
            for sig, handler in self.old_handlers.items(): signal.signal(sig, handler)
            self.old_handlers = {}

    def sync(self, source, hashes, inputs):
        source = Path(source)
        changed = deleted = unchanged = 0
        uploads = []; removals = []
        for name in self.source_hashes.keys() - hashes.keys():
            (self.directory / 'source' / name).unlink(); removals.append('source/' + name); deleted += 1
        for name, expected in hashes.items():
            path = Path(name)
            if path.is_absolute() or '..' in path.parts: raise RuntimeError('Unsafe source path')
            if self.source_hashes.get(name) == expected: unchanged += 1; continue
            original = source / path
            if original.is_symlink() or digest(original) != expected: raise RuntimeError('Source checksum mismatch: ' + name)
            target = self.directory / 'source' / path; target.parent.mkdir(parents=True, exist_ok=True)
            for parent in target.parents:
                if parent == self.directory: break
                parent.chmod(0o755)
            if target.exists(): target.chmod(0o644)
            shutil.copyfile(original, target); target.chmod(0o644); uploads.append((target, 'source/' + name)); changed += 1
        self.source_hashes = dict(hashes)
        input_changed = input_deleted = 0
        wanted = {}
        for name, original in inputs.items():
            path = Path(name)
            if path.is_absolute() or '..' in path.parts: raise RuntimeError('Unsafe runtime input path')
            expected = digest(original); wanted[name] = expected
            if self.input_hashes.get(name) == expected: continue
            target = self.directory / 'input' / name; target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(original, target); target.chmod(0o755 if Path(original).stat().st_mode & 0o111 else 0o644); uploads.append((target, name)); input_changed += 1
        for name in self.input_hashes.keys() - wanted.keys():
            (self.directory / 'input' / name).unlink(); removals.append(name); input_deleted += 1
        self.input_hashes = wanted
        if removals:
            for first in range(0, len(removals), 100):
                self.command(['exec', self.writer, 'rm', '-f', '--'] + ['/state/' + name for name in removals[first:first+100]])
        if uploads: self.upload(uploads)
        if changed or deleted or input_changed or input_deleted:
            # Compiled classes belong to a precise source/dependency revision.
            self.command(['exec', self.writer, 'rm', '-rf', '/state/classes'])
            self.command(['exec', self.name, 'sh', '-lc', 'rm -rf /workspace/classes /workspace/lib; mkdir -p /workspace/lib'])
        reused = self.transitions > 0; self.transitions += 1
        return {'source_changed': changed, 'source_deleted': deleted, 'source_unchanged': unchanged,
                'input_changed': input_changed, 'input_deleted': input_deleted, 'container_reused': reused}

    def upload(self, files):
        archive = self.directory / 'delta.tar'
        try:
            with tarfile.open(archive, 'w') as tar:
                for target, name in files:
                    info = tar.gettarinfo(str(target), arcname=name)
                    info.uid = info.gid = 1000; info.uname = info.gname = ''
                    with target.open('rb') as stream: tar.addfile(info, stream)
            with archive.open('rb') as stream: self.command(['cp', '-a', '-', self.writer + ':/state'], stdin=stream)
        finally: archive.unlink(missing_ok=True)

    def freeze_build(self):
        """Publish successful baseline bytecode read-only for subsequent agent runs."""
        if self.run('test -f /workspace/classes/.complete').returncode: return
        # Docker cp cannot read this daemon's tmpfs reliably. Stream through exec
        # instead; the trusted writer publishes bytecode into the read-only volume.
        # Publish as the operator writer's user, so later version transitions
        # can invalidate directories without granting the runtime any capabilities.
        producer = subprocess.Popen(self.docker + ['exec', self.name, 'tar', '--owner=0', '--group=0', '-C', '/workspace', '-cf', '-', 'classes'], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            self.command(['cp', '-a', '-', self.writer + ':/state'], stdin=producer.stdout)
            producer.stdout.close()
            _, error = producer.communicate(timeout=30)
            if producer.returncode: raise RuntimeError('Cannot freeze baseline build: ' + error.decode())
        finally:
            if producer.poll() is None: producer.kill(); producer.wait()
        self.command(['exec', self.name, 'rm', '-rf', '/workspace/classes'])

    def run(self, script, timeout=180):
        return subprocess.run(self.docker + ['exec', '-w', '/workspace', self.name, 'sh', '-lc', script], text=True, capture_output=True, timeout=timeout)

    def reset_job(self):
        """Stop ticket-specific processes and remove helper state, retaining builds."""
        script = 'for p in /proc/[0-9]*; do pid=${p##*/}; case "$pid" in ' + '|'.join(self.base_processes) + '|$$) continue;; esac; kill -KILL "$pid" 2>/dev/null || true; done; '
        script += 'rm -rf /workspace/* /workspace/.[!.]* /workspace/..?*; '
        script += 'rm -rf /tmp/* /tmp/.[!.]* /tmp/..?*'
        self.command(['exec', self.name, 'sh', '-lc', script])

    def tool_config(self, commit, audit):
        return {'docker_context': self.context, 'container': self.name, 'session': self.session,
                'source_dir': str(self.directory / 'source'), 'source_commit': commit,
                'files': self.source_hashes, 'audit_path': str(Path(audit).resolve())}


def cleanup_sessions():
    """Recover resources after an untrappable kill; never touch live or foreign sessions."""
    outcomes = []
    for marker in (ROOT / '.esa/replay-sessions').glob('*/session.json'):
        saved = json.loads(marker.read_text())
        try:
            os.kill(saved['pid'], 0)
            outcomes.append({'session':saved['session'], 'state':'live_skipped'}); continue
        except ProcessLookupError: pass
        context = saved.get('context', '')
        docker = ['docker'] + (['--context', context] if context else [])
        for name in (saved['container'], saved.get('writer')):
            if name: remove_owned(docker, 'container', name, saved['session'])
        if saved.get('volume'): remove_owned(docker, 'volume', saved['volume'], saved['session'])
        shutil.rmtree(marker.parent)
        outcomes.append({'session':saved['session'], 'state':'cleaned'})
    return outcomes


def remove_owned(docker, kind, name, session):
    label = '{{index .Labels "esa.replay.session"}}' if kind == 'volume' else '{{index .Config.Labels "esa.replay.session"}}'
    inspect_args = ['volume', 'inspect'] if kind == 'volume' else ['inspect']
    result = subprocess.run(docker + inspect_args + ['--format', label, name], text=True, capture_output=True, timeout=30)
    if result.returncode:
        if 'no such' in result.stderr.lower(): return
        raise RuntimeError('Cannot verify cleanup: ' + result.stderr.strip())
    if result.stdout.strip() != session: raise RuntimeError('Foreign resource ownership; cleanup refused')
    args = ['volume', 'rm', name] if kind == 'volume' else ['rm', '-f', '-v', name]
    subprocess.run(docker + args, check=True, text=True, capture_output=True, timeout=30)
