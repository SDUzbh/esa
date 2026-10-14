#!/usr/bin/env python3
"""Read-only downloader for public Jira engineering tickets.

This script is intentionally conservative: sequential API calls, retry/backoff,
resumable JSON exports and explicit flags when changelog completeness is unknown.
Run with --incremental to refresh changed exports, preserving old bytes in
history/. Python 3.9+ on macOS/Linux; no third-party dependencies.
"""

import argparse
import hashlib
import json
import random
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, quote
from urllib.request import Request, urlopen

from jira_dataset import created_path, dataset_writer, issue_files, timestamp

DEFAULT_JQL = "project = KAFKA AND issuetype = Bug ORDER BY created ASC"
TRANSIENT = {429, 500, 502, 503, 504}


class APIError(RuntimeError):
    def __init__(self, url, status, detail=""):
        super().__init__(f"HTTP {status} from {url}: {detail[:500]}")
        self.status = status


class Client:
    def __init__(self, base, delay, retries):
        self.base = base.rstrip("/")
        self.delay = delay
        self.retries = retries
        self.last_call = 0.0

    def get(self, endpoint, params=None):
        url = self.base + endpoint
        if params:
            url += "?" + urlencode(params)
        for attempt in range(self.retries + 1):
            gap = self.delay - (time.monotonic() - self.last_call)
            if gap > 0:
                time.sleep(gap)
            self.last_call = time.monotonic()
            req = Request(url, headers={
                "User-Agent": "KafkaIssueResearchDownloader/0.1 (read-only; public data)",
                "Accept": "application/json",
            })
            try:
                with urlopen(req, timeout=30) as rsp:
                    body = rsp.read().decode("utf-8")
                    data = json.loads(body)
                    if not isinstance(data, dict):
                        raise RuntimeError(f"Expected JSON object from {url}")
                    return data
            except HTTPError as ex:
                detail = ex.read(1000).decode("utf-8", errors="replace")
                if ex.code in TRANSIENT and attempt < self.retries:
                    after = ex.headers.get("Retry-After", "")
                    pause = float(after) if after.isdecimal() else min(60, 2 ** attempt + random.random())
                    print(f"Retryable HTTP {ex.code}: sleeping {pause:.1f}s", file=sys.stderr)
                    time.sleep(pause)
                    continue
                raise APIError(url, ex.code, detail) from ex
            except (URLError, TimeoutError) as ex:
                if attempt < self.retries:
                    time.sleep(min(60, 2 ** attempt + random.random()))
                    continue
                raise RuntimeError(f"Network error for {url}: {ex}") from ex
            except json.JSONDecodeError as ex:
                raise RuntimeError(f"Non-JSON response at {url}: {ex}") from ex
        raise RuntimeError("Unreachable retry loop")


def save_json(path, obj):
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(path.name + ".tmp")
    with temp.open("w", encoding="utf-8") as f:
        json.dump(obj, f, indent=2, ensure_ascii=False)
        f.write("\n")
    temp.replace(path)


def pages(client, path, item_key, page_size=100):
    all_items = []
    start = 0
    expected_total = None
    seen_ids = set()
    while True:
        payload = client.get(path, {"startAt": start, "maxResults": page_size})
        items = payload.get(item_key)
        if not isinstance(items, list):
            raise RuntimeError(f"Missing {item_key} array from {path}")
        for item in items:
            item_id = item.get("id")
            if item_id is not None:
                if item_id in seen_ids:
                    raise RuntimeError(f"Duplicate {item_key} ID from {path}")
                seen_ids.add(item_id)
        all_items.extend(items)
        start += len(items)
        total = payload.get("total")
        if isinstance(total, int):
            if expected_total is not None and total != expected_total:
                raise RuntimeError(f"{item_key} changed during pagination; retry the issue")
            expected_total = total
            if start > total or not items and start < total:
                raise RuntimeError(f"Incomplete {item_key} response from {path}")
        if not items or (isinstance(total, int) and start == total):
            return all_items


def download_one(client, key):
    safe = quote(key, safe="")
    issue = client.get(f"/rest/api/2/issue/{safe}", {"expand": "changelog"})
    comments = pages(client, f"/rest/api/2/issue/{safe}/comment", "comments")

    expanded = issue.get("changelog", {})
    histories = expanded.get("histories", []) if isinstance(expanded, dict) else []
    reported = expanded.get("total") if isinstance(expanded, dict) else None
    history_source = "issue.expand=changelog"
    history_complete = isinstance(reported, int) and len(histories) == reported

    # This paged changelog endpoint exists in some Jira deployments, but not all.
    try:
        fetched = pages(client, f"/rest/api/2/issue/{safe}/changelog", "values")
        if isinstance(reported, int) and len(fetched) != reported:
            raise RuntimeError("Changelog changed during export; retry the issue")
        histories = fetched
        history_source = "issue/changelog paginated endpoint"
        history_complete = True
    except APIError as ex:
        if ex.status not in (403, 404, 405):
            raise
        # Fallback: the expanded result may truncate the changelog.
        history_source = "issue.expand=changelog (dedicated endpoint unavailable)"
    return {
        "metadata": {
            "source": client.base,
            "issue_url": f"{client.base}/browse/{key}",
            "exported_at_utc": datetime.now(timezone.utc).isoformat(),
            "history_source": history_source,
            "history_complete": history_complete,
            "history_reported_total": reported,
            "attachments_downloaded": False,
        },
        "issue": issue,
        "comments": comments,
        "changelog": histories,
    }


def collect_issues(client, jql, limit, page_size):
    issues_by_key = {}
    offset = 0
    total = None
    while limit == 0 or len(issues_by_key) < limit:
        n = page_size if limit == 0 else min(page_size, limit - len(issues_by_key))
        data = client.get("/rest/api/2/search", {
            "jql": jql,
            "startAt": offset,
            "maxResults": n,
            "fields": "updated",
        })
        issues = data.get("issues")
        if not isinstance(issues, list):
            raise RuntimeError("Jira search did not return issues[]")
        if total is None:
            total = data.get("total")
            print(f"Search reports total={total} matching tickets")
        if not issues and isinstance(total, int) and offset < total:
            raise RuntimeError("Jira search ended before the reported total; rerun the query")
        for issue in issues:
            key = issue["key"]
            if key in issues_by_key:
                raise RuntimeError("Jira search returned a duplicate key; rerun the changing query: " + key)
            issues_by_key[key] = issue.get("fields", {}).get("updated")
        offset += len(issues)
        if not issues or (isinstance(total, int) and offset >= total):
            break
    return issues_by_key


def archive_export(root, key, content):
    """Keep the exact old bytes before replacing a current export."""
    digest = hashlib.sha256(content).hexdigest()
    path = root / "history" / key / (digest + ".json")
    if path.exists():
        if hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise ValueError("Historical export checksum mismatch: " + str(path))
    else:
        path.parent.mkdir(parents=True, exist_ok=True)
        temporary = path.with_suffix(".tmp")
        temporary.write_bytes(content)
        temporary.replace(path)
    return str(path.relative_to(root))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="https://issues.apache.org/jira")
    parser.add_argument("--jql", help="JQL scope; defaults to the saved dataset query, then Kafka bugs")
    parser.add_argument("--keys", nargs="+", help="download these issue keys instead of searching")
    parser.add_argument("--limit", type=int, default=None,
                        help="search results to inspect; 0 means all (default: 20, or all with --incremental)")
    parser.add_argument("--incremental", action="store_true", help="Download missing issues and refresh changed/incomplete exports")
    parser.add_argument("--page-size", type=int, default=100)
    parser.add_argument("--delay", type=float, default=1.0, help="minimum pause between HTTP requests")
    parser.add_argument("--retries", type=int, default=4)
    parser.add_argument("--output", type=Path, default=Path(__file__).resolve().parents[1] / "data/jira")
    args = parser.parse_args()
    if args.limit is None:
        args.limit = 0 if args.incremental else 20
    if args.limit < 0 or not 1 <= args.page_size <= 100 or args.delay < 0 or args.retries < 0:
        parser.error("limit >= 0, page-size 1..100, delay >= 0 and retries >= 0 required")
    with dataset_writer(args.output):
        return download(args)


def download(args):
    client = Client(args.base_url, args.delay, args.retries)
    raw, index = args.output / "raw", args.output / "index"
    existing = {path.stem: path for path in issue_files(raw)}
    index_file = index / "issue_keys.json"
    previous_index = json.loads(index_file.read_text()) if index_file.exists() else {}
    source = args.base_url.rstrip("/")
    if previous_index.get("source", source).rstrip("/") != source:
        raise ValueError("Dataset belongs to a different Jira server")
    if existing and not previous_index.get("source"):
        first = json.loads(next(iter(existing.values())).read_bytes())
        if first.get("metadata", {}).get("source", source).rstrip("/") != source:
            raise ValueError("Existing dataset belongs to a different Jira server")
    query = args.jql or previous_index.get("jql") or DEFAULT_JQL
    candidates = dict.fromkeys(args.keys) if args.keys else collect_issues(client, query, args.limit, args.page_size)
    # A limited run must not erase the rest of the desired corpus or failed keys.
    all_keys = list(dict.fromkeys(previous_index.get("keys", []) + list(existing) + list(candidates)))
    save_json(index_file, {**previous_index, "source": source,
                          "jql": previous_index.get("jql") or query, "keys": all_keys})
    print(f"Inspecting {len(candidates)} tickets (incremental={args.incremental})")
    failed, incomplete, archives = [], [], []
    counts = {"downloaded": 0, "updated": 0, "skipped": 0}
    for i, (key, remote_updated) in enumerate(candidates.items(), start=1):
        old_path = existing.get(key)
        try:
            old_content = old = None
            if old_path:
                if not args.incremental:
                    counts["skipped"] += 1
                    print(f"[{i}/{len(candidates)}] {key} already saved; skip")
                    continue
                old_content = old_path.read_bytes()
                old = json.loads(old_content)
                if old["issue"]["key"] != key:
                    raise ValueError("Stored Jira issue key differs from filename")
                if old.get("metadata", {}).get("source", source).rstrip("/") != source:
                    raise ValueError("Stored export belongs to a different Jira server")
                if args.keys:
                    header = client.get(f"/rest/api/2/issue/{quote(key, safe='')}", {"fields": "updated"})
                    remote_updated = header.get("fields", {}).get("updated")
                local_updated = old["issue"]["fields"].get("updated")
                complete = old.get("metadata", {}).get("history_complete", False)
                if remote_updated and local_updated and timestamp(remote_updated) <= timestamp(local_updated) and complete:
                    counts["skipped"] += 1
                    print(f"[{i}/{len(candidates)}] {key} unchanged; skip")
                    continue
            result = download_one(client, key)
            dest = args.output / created_path(result)
            if dest.stem != key:
                raise ValueError("Jira returned a different issue key")
            if args.incremental:
                # Missing/invalid timestamps must never be accepted as a new sync baseline.
                new_updated = timestamp(result["issue"]["fields"].get("updated"))
                if remote_updated and new_updated < timestamp(remote_updated):
                    raise ValueError("Downloaded issue is older than its search/header timestamp; retry later")
                if old:
                    old_updated = old["issue"]["fields"].get("updated")
                    if old_updated and new_updated < timestamp(old_updated):
                        raise ValueError("Refusing to replace a newer local export with an older response")
                    if created_path(old) != created_path(result):
                        raise ValueError("Original creation date changed; inspect before relocating the export")
                    if old.get("metadata", {}).get("history_complete") and not result["metadata"]["history_complete"]:
                        raise ValueError("Refusing to replace complete history with an incomplete export")
                    archives.append({"key": key, "path": archive_export(args.output, key, old_content)})
                    dest = old_path  # Keep legacy flat exports usable until explicit organization.
            save_json(dest, result)
            existing[key] = dest
            counts["updated" if old_path else "downloaded"] += 1
            if not result["metadata"]["history_complete"]:
                incomplete.append(key)
            print(f"[{i}/{len(candidates)}] {key}: {'updated' if old_path else 'downloaded'}, "
                  f"{len(result['comments'])} comments, {len(result['changelog'])} changelog entries "
                  f"(history complete={result['metadata']['history_complete']})")
        except Exception as ex:
            failed.append({"key": key, "error": str(ex)})
            print(f"[{i}/{len(candidates)}] {key}: ERROR: {ex}", file=sys.stderr)
    report = {"requested": len(candidates), "incremental": args.incremental, "jql": query,
              "finished_at_utc": datetime.now(timezone.utc).isoformat(), **counts,
              "failed": failed, "archived_exports": archives,
              "incomplete_changelogs_newly_downloaded": incomplete,
              "note": "Incremental mode compares updated timestamps; older exports are preserved in history/. No remote deletion is mirrored."}
    save_json(index / "download_report.json", report)
    print(f"Finished; downloaded={counts['downloaded']}; updated={counts['updated']}; "
          f"skipped={counts['skipped']}; failures={len(failed)}")
    return 1 if failed else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, RuntimeError) as exc:
        sys.exit("Jira download failed: " + str(exc))
