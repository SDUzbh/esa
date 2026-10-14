#!/usr/bin/env python3
"""Check official stable releases and rebuild immutable ESA Profile images.

Uses Docker and Python's standard library only. Never installs host software,
updates running containers, or replaces registered historical product packages.
"""
import argparse
import configparser
from datetime import datetime, timezone
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile
from urllib.request import Request, urlopen
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[1]
PROFILES = ['general', 'java-debug', 'performance', 'compatibility', 'configuration']
STATE = ROOT / '.esa/tool-images'
PACKAGE = re.compile(r'^[a-z0-9][a-z0-9+.-]*(?::[a-z0-9]+)?$')
VERSION = re.compile(r'^[A-Za-z0-9.+:~_-]+$')
SEMVER = re.compile(r'^v?(\d+)\.(\d+)(?:\.(\d+))?$')
SOOT_BASE = 'https://repo.maven.apache.org/maven2/org/soot-oss/soot'

def sha(data):
    return hashlib.sha256(data).hexdigest()

def atomic(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.new')
    temporary.write_bytes(data if isinstance(data, bytes) else data.encode())
    temporary.replace(path)

def configurations(root=ROOT):
    result, policy = {}, None
    for profile in PROFILES:
        path = root / 'profiles' / profile / 'toolset.txt'
        config = configparser.ConfigParser(interpolation=None)
        config.read_string(path.read_text())
        if config['profile']['id'] != profile or config['profile']['version'] != '2':
            raise ValueError('Invalid Profile/version: ' + profile)
        release = dict(config['release'])
        if release != {'os': 'noble', 'target_image': 'eclipse-temurin:8-jdk-noble', 'tool_jdk_image': 'eclipse-temurin:21-jdk-noble', 'async_profiler_repository': 'async-profiler/async-profiler'}:
            raise ValueError('Use the approved Noble/Temurin GA and official profiler release policy')
        if policy is not None and policy != release:
            raise ValueError('Shared toolkit Profiles must have the same release policy')
        policy = release
        packages = config['apt']['packages'].split()
        if not packages or any(not PACKAGE.fullmatch(package) for package in packages):
            raise ValueError('Invalid apt package declaration')
        result[profile] = {'config': config, 'packages': packages, 'bytes': path.read_bytes()}
    if result['java-debug']['config'].get('java_tools', 'soot') != 'org.soot-oss:soot':
        raise ValueError('Use the official org.soot-oss:soot artifact')
    return result, policy

class Docker:
    def __init__(self, context):
        self.context = context
        self.command = ['docker'] + (['--context', context] if context else [])

    def run(self, args, timeout=180, input=None):
        result = subprocess.run(self.command + args, input=input, capture_output=True, text=True, timeout=timeout)
        if result.returncode:
            raise RuntimeError('Docker %s failed:\n%s' % (args[0], result.stderr[-6000:]))
        return result.stdout.strip()

    def inspect(self, image):
        return json.loads(self.run(['image', 'inspect', image]))[0]

    def pull(self, image):
        print('Checking official GA image: ' + image, flush=True)
        self.run(['pull', image], timeout=600)
        value = self.inspect(image)
        if not value.get('RepoDigests'):
            raise RuntimeError('Official image has no repository digest')
        return value['RepoDigests'][0]

def resolve_packages(docker, base, names):
    # Resolve signed distro candidates and the full dependency installation plan
    # in a disposable container. No host filesystem is mounted here.
    script = '''set -eu
. /etc/os-release
test "$VERSION_CODENAME" = noble
apt-get update >&2
for package do
  candidate=$(apt-cache policy "$package" | awk '/Candidate:/ {print $2}')
  test -n "$candidate" && test "$candidate" != '(none)'
  printf 'ESA_PACKAGE %s %s\\n' "$package" "$candidate"
done
apt-get -s --no-install-recommends install "$@" | sed -n '/^Inst /p'
apt list --upgradable 2>/dev/null | sed '1d' | while read -r name version rest; do
  printf 'ESA_PACKAGE %s %s\\n' "${name%%/*}" "$version"
done
'''
    output = docker.run(['run', '--rm', '--memory', '512m', '--cpus', '1', '--pids-limit', '128', '--entrypoint', 'sh', base, '-c', script, 'resolver'] + names, timeout=360)
    versions = {}
    for line in output.splitlines():
        if line.startswith('ESA_PACKAGE '):
            _, package, version = line.split()
        elif line.startswith('Inst '):
            match = re.match(r'^Inst (\S+)(?: \[[^]]+\])? \((\S+)', line)
            if not match:
                raise ValueError('Unrecognized official dependency plan')
            package, version = match.groups()
        else:
            continue
        if not PACKAGE.fullmatch(package) or not VERSION.fullmatch(version):
            raise ValueError('Invalid resolved package/version')
        if package in versions and versions[package] != version:
            raise ValueError('Conflicting package candidates: ' + package)
        versions[package] = version
    if any(name not in versions for name in names):
        raise RuntimeError('Incomplete stable package resolution')
    return dict(sorted(versions.items()))

def official_json(url):
    request = Request(url, headers={'User-Agent': 'ESA-tool-image-updater', 'Accept': 'application/vnd.github+json'})
    with urlopen(request, timeout=60) as response:
        return json.load(response)

def choose_profiler(releases, arch):
    stable = [release for release in releases if not release.get('draft') and not release.get('prerelease') and SEMVER.fullmatch(release.get('tag_name', ''))]
    if not stable:
        raise RuntimeError('Official stable async-profiler release unavailable')
    latest = max(stable, key=lambda release: tuple(int(value or 0) for value in SEMVER.fullmatch(release['tag_name']).groups()))
    suffix = '-linux-arm64.tar.gz' if arch == 'arm64' else '-linux-x64.tar.gz'
    assets = [asset for asset in latest.get('assets', []) if asset['name'].endswith(suffix)]
    if len(assets) != 1:
        raise RuntimeError('Stable profiler lacks one unambiguous platform asset')
    asset = assets[0]
    checksum = asset.get('digest', '')
    if not re.fullmatch(r'sha256:[a-f0-9]{64}', checksum):
        raise RuntimeError('Official profiler asset lacks a published SHA256; refusing an unverified download')
    url = asset['browser_download_url']
    if not url.startswith('https://github.com/async-profiler/async-profiler/releases/download/'):
        raise RuntimeError('Profiler asset is outside the official repository')
    return {'version': latest['tag_name'], 'published_at': latest['published_at'], 'url': url, 'sha256': checksum.split(':', 1)[1], 'asset': asset['name']}

def choose_soot(versions):
    stable = [version for version in versions if re.fullmatch(r'\d+\.\d+\.\d+', version)]
    if not stable:
        raise RuntimeError('Official stable Soot release unavailable')
    return max(stable, key=lambda version: tuple(map(int, version.split('.'))))

def resolve_soot():
    with urlopen(SOOT_BASE + '/maven-metadata.xml', timeout=60) as response:
        metadata = response.read(1 << 20)
    version = choose_soot([item.text or '' for item in ET.fromstring(metadata).findall('./versioning/versions/version')])
    asset = 'soot-' + version + '-jar-with-dependencies.jar'
    url = SOOT_BASE + '/' + version + '/' + asset
    with urlopen(url + '.sha256', timeout=60) as response:
        checksum = response.read(256).decode().strip().lower()
    if not re.fullmatch(r'[a-f0-9]{64}', checksum):
        raise RuntimeError('Official Soot artifact lacks a published SHA256')
    return {'version': version, 'url': url, 'asset': asset, 'sha256': checksum}

def download_artifact(release):
    path = STATE / 'artifacts' / release['asset']
    if path.exists() and sha(path.read_bytes()) == release['sha256']:
        return path
    request = Request(release['url'], headers={'User-Agent': 'ESA-tool-image-updater'})
    with urlopen(request, timeout=120) as response:
        data = response.read(32 * 1024 * 1024 + 1)
    if len(data) > 32 * 1024 * 1024 or sha(data) != release['sha256']:
        raise RuntimeError('Tool artifact exceeds size bound or published SHA256 differs')
    atomic(path, data)
    return path

def extract_profiler(archive, destination):
    with tarfile.open(archive) as entries:
        names = entries.getnames()
        roots = {Path(name).parts[0] for name in names if Path(name).parts}
        if len(roots) != 1:
            raise ValueError('Profiler archive must have one root')
        for member in entries:
            parts = Path(member.name).parts
            if Path(member.name).is_absolute() or '..' in parts or not (member.isdir() or member.isfile()):
                raise ValueError('Unsafe profiler archive member')
            relative = Path(*parts[1:])
            if relative == Path('.'):
                continue
            target = destination / relative
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with entries.extractfile(member) as source, target.open('wb') as output:
                    shutil.copyfileobj(source, output)
                target.chmod(0o755 if member.mode & 0o111 else 0o644)

def inspect_image(docker, image):
    output = docker.run(['run', '--rm', '--network', 'none', '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--memory', '512m', '--cpus', '1', '--pids-limit', '128', '--tmpfs', '/tmp:rw,nosuid,size=32m', '--entrypoint', 'python3', image, '/opt/esa/inspect_tools.py'], timeout=90)
    return json.loads(output)

def lock_text(release, inventory, image_id):
    lines = ['# Generated by scripts/update_tool_images.py; immutable release inspection.', '[toolkit]', '[images]', 'target = ' + release['target_image'], 'tool_jdk = ' + release['tool_jdk_image'], 'image = ' + image_id, '[upstream]', 'async_profiler = ' + release['profiler']['version'], 'async_profiler_sha256 = ' + release['profiler']['sha256'], '[packages]']
    lines += [name + ' = ' + version for name, version in sorted(inventory['packages'].items())]
    lines += ['[commands]'] + [name + ' = ' + path for name, path in sorted(inventory['commands'].items())]
    lines += ['jfr = ' + inventory['jfr'], '[java]', 'target = ' + inventory['target_java'].splitlines()[0], 'tools = ' + inventory['tool_java'].splitlines()[0]]
    lines += ['[soot]', 'version = ' + release['soot']['version'], 'sha256 = ' + release['soot']['sha256'], 'source = ' + release['soot']['url']]
    return '\n'.join(lines) + '\n'

def update(context, check=False, locked=False):
    configs, policy = configurations()
    docker = Docker(context)
    architecture = docker.run(['info', '--format', '{{.Architecture}}'])
    arch = {'aarch64': 'arm64', 'arm64': 'arm64', 'x86_64': 'amd64', 'amd64': 'amd64'}.get(architecture)
    if not arch:
        raise RuntimeError('Supported toolkit platforms are Linux ARM64 and AMD64')
    previous = json.loads((STATE / 'current.json').read_text()) if (STATE / 'current.json').exists() else None
    if locked:
        release = json.loads((STATE / 'release.json').read_text())
        if release['architecture'] != arch:
            raise ValueError('Release lock platform differs from the Docker host')
    else:
        target = docker.pull(policy['target_image'])
        tool_jdk = docker.pull(policy['tool_jdk_image'])
        packages = sorted({package for config in configs.values() for package in config['packages']})
        versions = resolve_packages(docker, target, packages)
        profiler = choose_profiler(official_json('https://api.github.com/repos/async-profiler/async-profiler/releases?per_page=100'), arch)
        release = {'architecture': arch, 'target_image': target, 'tool_jdk_image': tool_jdk, 'packages': versions, 'profiler': profiler, 'soot': resolve_soot()}
    if 'soot' not in release:
        # Initialize the newly declared tool without upgrading the existing stack.
        print('Registering stable Soot; retaining existing JDK/package/profiler locks.', flush=True)
        release['soot'] = resolve_soot()
    profile_sources = b''.join((ROOT / 'profiles' / name / filename).read_bytes() for name in PROFILES for filename in ('toolset.txt', 'SKILL.md', 'environment.json', 'tools.json'))
    source_hash = sha(profile_sources + (ROOT / 'deploy/tools/Dockerfile').read_bytes() + (ROOT / 'deploy/tools/soot').read_bytes() + (ROOT / 'scripts/inspect_tools.py').read_bytes() + Path(__file__).read_bytes())
    fingerprint = sha(json.dumps(release, sort_keys=True).encode() + source_hash.encode())
    unchanged = previous and previous['release_hash'] == fingerprint and previous['docker_context'] == context
    if unchanged:
        try:
            entry = previous['toolkit']
            info = docker.inspect(entry['image_id'])
            unchanged = info['Config']['Labels'].get('esa.release.hash') == fingerprint
        except (RuntimeError, KeyError):
            unchanged = False
    print(json.dumps({'status': 'current' if unchanged else 'update_available', 'architecture': arch, 'release_hash': fingerprint, 'async_profiler': release['profiler']['version'], 'soot': release['soot']['version']}), flush=True)
    if check:
        return
    if unchanged:
        entry = previous['toolkit']
        inventory = inspect_image(docker, entry['image_id'])
        atomic(ROOT / 'deploy/tools/toolset.release.txt', lock_text(release, inventory, entry['image_id']))
        print('Shared immutable toolkit passed inspection; no rebuild needed.', flush=True)
        return
    archive = download_artifact(release['profiler'])
    soot = download_artifact(release['soot'])
    STATE.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='build-', dir=str(STATE)) as temporary:
        directory = Path(temporary)
        shutil.copy(ROOT / 'deploy/tools/Dockerfile', directory / 'Dockerfile')
        shutil.copy(ROOT / 'scripts/inspect_tools.py', directory / 'inspect_tools.py')
        shutil.copy(ROOT / 'deploy/tools/soot', directory / 'soot')
        shutil.copy(soot, directory / 'soot.jar')
        atomic(directory / 'packages.txt', '\n'.join(name + '=' + version for name, version in release['packages'].items()) + '\n')
        extract_profiler(archive, directory / 'async-profiler')
        for profile in PROFILES:
            shutil.copytree(ROOT / 'profiles' / profile, directory / 'profiles' / profile, ignore=shutil.ignore_patterns('toolset.release.txt'))
        base = 'esa-toolkit:release-' + fingerprint[:16]
        common = ['build', '--build-arg', 'TARGET_IMAGE=' + release['target_image'], '--build-arg', 'TOOL_JDK_IMAGE=' + release['tool_jdk_image'], '--build-arg', 'TOOLSET_HASH=' + source_hash, '--build-arg', 'RELEASE_HASH=' + fingerprint]
        print('Building shared immutable release toolkit...', flush=True)
        docker.run(common + ['--target', 'toolkit', '-t', base, str(directory)], timeout=1200)
        image_id = docker.inspect(base)['Id']
        inventory = inspect_image(docker, image_id)
        for profile in PROFILES:
            for command in configs[profile]['config']['commands']['primary'].split():
                if not inventory['commands'].get(command):
                    raise RuntimeError('Missing Profile command: ' + command)
        toolkit = {'tag': base, 'image_id': image_id, 'toolset_hash': source_hash, 'inventory': inventory}
        manifest = {'schema_version': 1, 'updated_at': datetime.now(timezone.utc).isoformat(), 'docker_context': context, 'architecture': arch, 'release_hash': fingerprint, 'release': release, 'toolkit': toolkit, 'profiles': PROFILES}
        atomic(STATE / 'release.json', json.dumps(release, indent=2) + '\n')
        atomic(ROOT / 'deploy/tools/toolset.release.txt', lock_text(release, inventory, image_id))
        atomic(STATE / 'current.json', json.dumps(manifest, indent=2) + '\n')
    print(json.dumps({'status': 'updated', 'toolkit': toolkit['tag'], 'profiles': PROFILES}), flush=True)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--context', default=os.environ.get('DOCKER_CONTEXT', 'colima-redmine'))
    parser.add_argument('--check', action='store_true', help='Resolve official releases and report changes without building or switching manifests')
    parser.add_argument('--locked', action='store_true', help='Use the last verified release lock instead of resolving newer releases')
    args = parser.parse_args()
    update(args.context, args.check, args.locked)

if __name__ == '__main__':
    main()
