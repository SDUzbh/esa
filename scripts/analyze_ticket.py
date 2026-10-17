#!/usr/bin/env python3
"""Forward one ticket through ESA and download the provider's raw runtime events."""
import argparse
from datetime import datetime, timezone
import getpass
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
STAGES = {"route": "routing", "engineer": "engineering", "assign": "assignment"}
ACTIVE = {"queued", "running", "investigating"}


def save_json(path, value):
    path.write_text(json.dumps(value, indent=2, ensure_ascii=False) + "\n")


def read_token(path):
    token = os.environ.get("ESA_API_TOKEN", "")
    if not token and path.is_file():
        for line in path.read_text().splitlines():
            if line.startswith("ESA_API_TOKEN="):
                token = line.partition("=")[2].strip().strip("\"'")
    if len(token) < 24:
        raise ValueError("Set ESA_API_TOKEN or --token-env-file (at least 24 characters)")
    return token


def base_url(value):
    url = urllib.parse.urlsplit(value)
    if url.scheme not in {"http", "https"} or not url.hostname or url.username or url.password or url.query or url.fragment:
        raise ValueError("Expected an HTTP(S) base URL without credentials, query or fragment")
    return value.rstrip("/")


def request(base, path, token="", project="", body=None):
    headers = {"Authorization": "Bearer " + token} if token else {}
    if project:
        headers["X-ESA-Project-IDs"] = project
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(base + path, data=data, headers=headers)
    with urllib.request.urlopen(req, timeout=15) as response:
        return json.load(response)


def collect_stage_logs(provider, token, run_id, stage, label, output, cursors):
    """Append complete API log events and advance byte cursors without inventing absent stages."""
    key = (run_id, stage)
    offset = cursors.get(key, 0)
    while True:
        page = request(provider, "/runs/" + urllib.parse.quote(run_id, safe="") + "/logs?" +
                       urllib.parse.urlencode({"stage": stage, "offset": offset}), token)
        if page["events"]:
            folder = output / "logs" / run_id
            folder.mkdir(parents=True, exist_ok=True)
            with (folder / (label + ".events.jsonl")).open("a") as stream:
                for event in page["events"]:
                    stream.write(json.dumps(event, ensure_ascii=False) + "\n")
                    item = event.get("item") or {}
                    print(f"[{label}] {event.get('type', 'event')} {item.get('type', '')} {item.get('tool', '')}", flush=True)
        next_offset = page["next_offset"]
        if page["has_more"] and next_offset <= offset:
            raise ValueError("Provider returned a non-advancing log cursor")
        cursors[key] = next_offset
        offset = next_offset
        if not page["has_more"]:
            break


def collect_logs(provider, token, run_id, output, cursors, errors):
    for stage, label in STAGES.items():
        key = run_id + "/" + stage
        try:
            collect_stage_logs(provider, token, run_id, stage, label, output, cursors)
            errors.pop(key, None)
        except (OSError, ValueError, urllib.error.URLError) as exc:
            message = type(exc).__name__ + (f": HTTP {exc.code}" if isinstance(exc, urllib.error.HTTPError) else "")
            if errors.get(key) != message:
                print(f"Log download warning [{key}]: {message}; analysis continues", file=sys.stderr)
            errors[key] = message


def main(argv=None):
    """Forward one explicit ticket through the real pipeline and retain private API results.

    A locally started target is owned and stopped on exit. An attached service is
    neither reconfigured nor stopped; log failures do not retry Agent analysis."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("ticket", type=Path, help="ESA Ticket JSON, or an Event JSON containing ticket")
    parser.add_argument("--routing-provider", default="http://127.0.0.1:8091", help="Provider base URL; Routing Agent decides whether engineering is needed")
    parser.add_argument("--esa-url", help="Use an existing ESA service configured for the same provider; otherwise start an isolated local service")
    parser.add_argument("--esa-bin", type=Path, default=ROOT / "bin/esa-host")
    parser.add_argument("--database-url", default=os.environ.get("ESA_DATABASE_URL", ""),
                        help="Postgres for the isolated local service (default: $ESA_DATABASE_URL); it runs in a throwaway schema")
    parser.add_argument("--rules", type=Path, default=ROOT / "config/rules.json")
    parser.add_argument("--issues-directory", type=Path, default=ROOT / "runtime/esa/issues", help="Must match the provider's shared Issue projection root")
    parser.add_argument("--token-env-file", type=Path, default=ROOT / "deploy/redmine/.env")
    parser.add_argument("--forwarded-by", default="cli:" + getpass.getuser())
    parser.add_argument("--output", type=Path, help="New private output directory")
    parser.add_argument("--wait-seconds", type=int, default=1200, help="Maximum client wait; independent of advisory token thresholds")
    args = parser.parse_args(argv)
    os.umask(0o077)
    if args.wait_seconds < 1:
        parser.error("--wait-seconds must be positive")
    provider = base_url(args.routing_provider)
    esa_url = base_url(args.esa_url) if args.esa_url else None
    token = read_token(args.token_env_file)
    raw = json.loads(args.ticket.read_text())
    if not isinstance(raw, dict):
        raise ValueError("Ticket file must contain a JSON object")
    ticket = raw.get("ticket", raw)
    if not isinstance(ticket, dict) or any(not isinstance(ticket.get(k), str) or not ticket[k].strip() for k in ("id", "project_id", "subject")):
        raise ValueError("Ticket requires nonempty string id, project_id and subject fields")
    if ticket.get("closed") or ticket.get("private"):
        raise ValueError("This diagnostic runner requires an open, non-private ticket")
    request(provider, "/healthz")
    # Fail before forwarding/spending if an old provider has no log API.
    request(provider, "/runs/run_probe/logs?stage=route&offset=0", token)
    if not esa_url and not args.esa_bin.is_file():
        raise ValueError("Build bin/esa-host first: .tools/go/bin/go build -o bin/esa-host ./cmd/esa")
    if not esa_url and not args.database_url:
        raise ValueError("The isolated local service keeps its state in Postgres: set ESA_DATABASE_URL or --database-url")
    output = (args.output or ROOT / "runtime/ticket-runs" / ("ticket_" + secrets.token_hex(8))).resolve()
    output.mkdir(parents=True, exist_ok=False, mode=0o700)
    process = None
    server_log = None
    cursors = {}
    log_errors = {}
    run_ids = set()
    try:
        if not esa_url:
            # Use the real service/worker/claim pipeline, never a second router.
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
            esa_url = f"http://127.0.0.1:{port}"
            service_token = secrets.token_urlsafe(32)
            env = {k: v for k, v in os.environ.items() if not k.startswith("ESA_")}
            env.update(ESA_API_TOKEN=service_token, ESA_ANALYZER_TOKEN=token, ESA_ANALYZER_URL=provider + "/analyze",
                       ESA_DATABASE_URL=args.database_url)
            server_log = (output / "service.log").open("wb")
            process = subprocess.Popen([str(args.esa_bin.resolve()), "-listen", f"127.0.0.1:{port}",
                                        "-ephemeral-schema", "-rules", str(args.rules.resolve()),
                                        "-issues-directory", str(args.issues_directory.resolve())],
                                       cwd=ROOT, env=env, stdout=server_log, stderr=server_log)
            for _ in range(100):
                if process.poll() is not None:
                    raise RuntimeError("ESA exited during startup; inspect service.log")
                try:
                    request(esa_url, "/healthz?role=active")
                    break
                except (OSError, urllib.error.URLError):
                    time.sleep(0.1)
            else:
                raise RuntimeError("ESA startup timed out")
        else:
            service_token = token
        now = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
        # Preserve supplied snapshot dates. Missing dates use this explicit intake time.
        ticket.setdefault("updated_at", now)
        ticket.setdefault("created_at", ticket["updated_at"])
        event = {"id": "cli_" + secrets.token_hex(16), "origin": "operator_cli", "occurred_at": ticket["updated_at"],
                 "forwarded_by": args.forwarded_by, "forwarded_at": now, "ticket": ticket}
        save_json(output / "event.json", event)
        receipt = request(esa_url, "/v1/events", service_token, ticket["project_id"], event)
        save_json(output / "receipt.json", receipt)
        if receipt.get("run_id"):
            run_ids.add(receipt["run_id"])
        deadline = time.monotonic() + args.wait_seconds
        print("Output: " + str(output), flush=True)
        while True:
            detail = request(esa_url, "/v1/tickets/" + urllib.parse.quote(ticket["id"], safe=""), service_token, ticket["project_id"])
            run = detail["ticket"].get("latest_run")
            if run:
                run_ids.add(run["id"])  # Joined tasks can expose a different leader run.
            save_json(output / "result.json", detail)
            for run_id in sorted(run_ids):
                collect_logs(provider, token, run_id, output, cursors, log_errors)
            if not run or run["state"] not in ACTIVE:
                print("State: " + (run["state"] if run else receipt["action"]), flush=True)
                return 0 if not log_errors and run and run["state"] in {"awaiting_review", "returned_to_support", "awaiting_information", "completed"} else 1
            if time.monotonic() >= deadline:
                raise TimeoutError("Client wait expired; inspect result.json and downloaded logs. No automatic retry was submitted.")
            time.sleep(1)
    finally:
        if process and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=45)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        if server_log:
            server_log.close()
        # Flush final model events after local worker cancellation/cleanup too.
        for run_id in sorted(run_ids):
            collect_logs(provider, token, run_id, output, cursors, log_errors)
        save_json(output / "logs.json", {"runs": sorted(run_ids), "download_errors": log_errors, "streams": [
            {"run_id": k[0], "stage": k[1], "bytes_read": v} for k, v in sorted(cursors.items())]})


if __name__ == "__main__":
    def interrupt(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupt)
    try:
        sys.exit(main())
    except (OSError, ValueError, RuntimeError, urllib.error.URLError) as exc:
        print(f"Ticket analysis failed: {exc}", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        sys.exit(130)
