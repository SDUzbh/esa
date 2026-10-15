#!/usr/bin/env python3
"""Inspect preinstalled tools without changing a running diagnostic image."""
import argparse
import json
from pathlib import Path
import shutil
import subprocess

COMMANDS = ['java', 'javac', 'jdb', 'jcmd', 'jstack', 'jmap', 'javap', 'jdeps', 'python3', 'rg', 'jq', 'git', 'curl', 'gdb', 'strace', 'lsof', 'ip', 'ss', 'dig', 'tcpdump', 'file', 'readelf', 'objdump', 'unzip', 'sha256sum', 'asprof', 'vmstat', 'pidstat', 'iostat', 'time', 'nc', 'diff', 'soot', 'sootup']

def inspect():
    packages = subprocess.run(['dpkg-query', '-W', '-f=${Package}\t${Version}\n'], capture_output=True, text=True, check=True)
    inventory = {'schema_version': 1, 'packages': dict(line.split('\t', 1) for line in packages.stdout.splitlines()), 'commands': {name: shutil.which(name) for name in COMMANDS}}
    # A separate modern tool JVM must not replace the historical target JVM.
    inventory['target_java'] = subprocess.run(['/opt/java/openjdk/bin/java', '-version'], capture_output=True, text=True, check=True).stderr.strip()
    inventory['tool_java'] = subprocess.run(['/opt/esa/tool-jdk/bin/java', '-version'], capture_output=True, text=True, check=True).stderr.strip()
    inventory['jfr'] = '/opt/esa/tool-jdk/bin/jfr'
    if not Path(inventory['jfr']).is_file() or any(not path for path in inventory['commands'].values()):
        raise RuntimeError('Required preset tool is missing')
    profiler = subprocess.run(['/opt/esa/async-profiler/bin/asprof', '--version'], capture_output=True, text=True, check=True)
    inventory['async_profiler'] = (profiler.stdout + profiler.stderr).strip()
    soot = subprocess.run(['soot', '-version'], capture_output=True, text=True, check=True, timeout=30)
    inventory['soot'] = (soot.stdout + soot.stderr).strip()
    sootup = subprocess.run(['sootup', '--version'], capture_output=True, text=True, check=True, timeout=30)
    inventory['sootup'] = sootup.stdout.strip()
    return inventory

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--write')
    args = parser.parse_args()
    result = inspect()
    output = json.dumps(result, indent=2) + '\n'
    if args.write:
        Path(args.write).write_text(output)
    else:
        print(output, end='')

if __name__ == '__main__':
    main()
