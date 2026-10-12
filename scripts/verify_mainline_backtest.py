#!/usr/bin/env python3
"""Verify system replay through public APIs; use --runtime for Kafka observations."""
import argparse
from pathlib import Path
import subprocess
import uuid

ROOT = Path(__file__).resolve().parents[1]

def main():
    parser = argparse.ArgumentParser()
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument('--runtime', action='store_true')
    mode.add_argument('--forwarding', action='store_true')
    parser.add_argument('--output', default=str(ROOT / 'reports' / ('system-replay-' + uuid.uuid4().hex)))
    args = parser.parse_args()
    example = 'esa-api-forwarding.json' if args.forwarding else ('esa-api-kafka.json' if args.runtime else 'esa-api-lifecycle.json')
    subprocess.run(['python3', str(ROOT / 'scripts/replay_pipeline.py'), '--build-tools', '--pipeline', '--manifest',
                    str(ROOT / 'system_replay/examples' / example), '--output', args.output], check=True)

if __name__ == '__main__': main()
