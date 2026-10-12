#!/usr/bin/env python3
"""Compatibility entry point for the standalone external system replay tester."""
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
MODULE = ROOT / 'system_replay'


def main():
    args = sys.argv[1:]
    if '--manifest' not in args:
        raise SystemExit('System replay requires --manifest with public target requests and test assertions. Direct engineering-provider calls and imported predictions are no longer supported.')
    binary = ROOT / 'bin/system-replay'
    if '--build-tools' in args or not binary.exists():
        args = [a for a in args if a != '--build-tools']
        go = ROOT / '.tools/go/bin/go'
        if not go.exists(): go = Path('go')
        env = dict(os.environ, GOCACHE=str(ROOT / '.tools/go-cache'), GOTOOLCHAIN='local')
        binary.parent.mkdir(exist_ok=True)
        subprocess.run([str(go), 'build', '-trimpath', '-o', str(binary), './cmd/system-replay'], cwd=MODULE, env=env, check=True)
    # Replace this process so cancellation and exit status belong to the tester.
    os.execv(str(binary), [str(binary)] + args)


if __name__ == '__main__': main()
