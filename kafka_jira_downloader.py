#!/usr/bin/env python3
"""Read-only downloader for public Jira engineering tickets.

This script is intentionally conservative: sequential API calls, retry/backoff,
resumable JSON exports and explicit flags when changelog completeness is unknown.
Python 3.9+; no third-party dependencies.
"""

import argparse
import json
import random
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, quote
from urllib.request import Request, urlopen

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
    while True:
        payload = client.get(path, {"startAt": start, "maxResults": page_size})
        items = payload.get(item_key)
        if not isinstance(items, list):
            raise RuntimeError(f"Missing {item_key} array from {path}")
        all_items.extend(items)
        start += len(items)
        total = payload.get("total")
        if not items or (isinstance(total, int) and start >= total):
            return all_items
        if len(items) == 0:
            return all_items


def download_one(client, key):
    safe = quote(key, safe="")
    issue = client.get(f"/rest/api/2/issue/{safe}", {"expand": "changelog"})
    comments = pages(client, f"/rest/api/2/issue/{safe}/comment", "comments")

    expanded = issue.get("changelog", {})
    histories = expanded.get("histories", []) if isinstance(expanded, dict) else []
    reported = expanded.get("total") if isinstance(expanded, dict) else None
    history_source = "issue.expand=changelog"
    history_complete = isinstance(reported, int) and len(histories) >= reported

    # This paged changelog endpoint exists in some Jira deployments, but not all.
    try:
        fetched = pages(client, f"/rest/api/2/issue/{safe}/changelog", "values")
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


def collect_keys(client, jql, limit, page_size):
    keys = []
    offset = 0
    total = None
    while limit == 0 or len(keys) < limit:
        n = page_size if limit == 0 else min(page_size, limit - len(keys))
        data = client.get("/rest/api/2/search", {
            "jql": jql,
            "startAt": offset,
            "maxResults": n,
            "fields": "summary",
        })
        issues = data.get("issues")
        if not isinstance(issues, list):
            raise RuntimeError("Jira search did not return issues[]")
        if total is None:
            total = data.get("total")
            print(f"Search reports total={total} matching tickets")
        got = [x["key"] for x in issues]
        keys.extend(got)
        offset += len(issues)
        if not issues or (isinstance(total, int) and offset >= total):
            break
    return keys


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="https://issues.apache.org/jira")
    parser.add_argument("--jql", default=DEFAULT_JQL)
    parser.add_argument("--keys", nargs="+", help="download these issue keys instead of searching")
    parser.add_argument("--limit", type=int, default=20,
                        help="number to download; 0 means all matched issues (default: 20)")
    parser.add_argument("--page-size", type=int, default=50)
    parser.add_argument("--delay", type=float, default=1.0, help="minimum pause between HTTP requests")
    parser.add_argument("--retries", type=int, default=4)
    parser.add_argument("--output", type=Path, default=Path("data/jira"))
    args = parser.parse_args()
    if args.limit < 0 or not 1 <= args.page_size <= 100 or args.delay < 0:
        parser.error("limit >= 0, page-size 1..100, delay >= 0 required")
    client = Client(args.base_url, args.delay, args.retries)
    raw = args.output / "raw"
    index = args.output / "index"
    if args.keys:
        keys = list(dict.fromkeys(args.keys))
    else:
        try:
            keys = collect_keys(client, args.jql, args.limit, args.page_size)
        except Exception as ex:
            sys.exit(f"Cannot query Jira search API: {ex}\nCheck public API access with curl first, or use --keys for individual tickets.")
    save_json(index / "issue_keys.json", {"jql": args.jql, "keys": keys})
    print(f"Downloading {len(keys)} tickets")
    failed = []
    incomplete = []
    for i, key in enumerate(keys, start=1):
        dest = raw / f"{key}.json"
        if dest.exists():
            print(f"[{i}/{len(keys)}] {key} already saved; skip")
            continue
        try:
            result = download_one(client, key)
            save_json(dest, result)
            if not result["metadata"]["history_complete"]:
                incomplete.append(key)
            print(f"[{i}/{len(keys)}] {key}: {len(result['comments'])} comments, "
                  f"{len(result['changelog'])} changelog entries "
                  f"(history complete={result['metadata']['history_complete']})")
        except Exception as ex:
            failed.append({"key": key, "error": str(ex)})
            print(f"[{i}/{len(keys)}] {key}: ERROR: {ex}", file=sys.stderr)
    save_json(index / "download_report.json", {
        "requested": len(keys),
        "failed": failed,
        "incomplete_changelogs_newly_downloaded": incomplete,
        "note": "Files already present were skipped without rechecking completeness.",
    })
    print(f"Finished; failures={len(failed)}; new incomplete histories={len(incomplete)}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
