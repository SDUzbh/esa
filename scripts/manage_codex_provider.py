#!/usr/bin/env python3
"""Manage the local, explicitly registered Codex adapter without a system service."""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import time
import urllib.request

ROOT=Path(__file__).resolve().parents[1]
STATE=ROOT/'.esa/codex-provider'
ARGS=['bin/esa-codex','-listen','127.0.0.1:8091','-evidence','.esa/replays/KAFKA-2880/trial-1/evidence.json']

def matching_pids():
    # Inspect only exact adapter argv matches; never print other process arguments.
    rows=subprocess.run(['ps','-axo','pid=,command='],capture_output=True,text=True,check=True).stdout.splitlines()
    return [int(parts[0]) for row in rows if len(parts:=row.strip().split(None,1))==2 and parts[1]==' '.join(ARGS)]

def main():
    parser=argparse.ArgumentParser();parser.add_argument('action',choices=['start','restart','stop','status']);args=parser.parse_args()
    pids=matching_pids()
    if args.action in ['restart','stop']:
        for pid in pids:os.kill(pid,signal.SIGTERM)
        for _ in range(450):
            if not matching_pids():break
            time.sleep(.1)
        pids=matching_pids()
        if pids:raise RuntimeError('Adapter is still shutting down')
    if args.action in ['start','restart'] and not pids:
        STATE.mkdir(parents=True,exist_ok=True)
        path=STATE/'provider.log';fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_APPEND,0o600)
        with os.fdopen(fd,'ab') as log:
            process=subprocess.Popen(ARGS,cwd=ROOT,stdin=subprocess.DEVNULL,stdout=log,stderr=log,start_new_session=True)
        (STATE/'pid').write_text(str(process.pid)+'\n');pids=[process.pid]
        for _ in range(150):
            try:
                with urllib.request.urlopen('http://127.0.0.1:8091/healthz',timeout=1) as r:health=json.load(r)
                print(json.dumps({'pids':pids,'health':health}));return
            except OSError:time.sleep(.1)
        raise RuntimeError('Adapter did not become ready; inspect private provider.log')
    print(json.dumps({'pids':pids,'status':'running' if pids else 'stopped'}))

if __name__=='__main__':main()
