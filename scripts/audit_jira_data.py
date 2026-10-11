#!/usr/bin/env python3
"""Audit a frozen local snapshot against read-only public Jira REST responses."""
import argparse
import collections
import hashlib
import json
import re
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlencode

ROOT = Path(__file__).resolve().parents[1]
BASE = "https://issues.apache.org/jira"


def read(path):
    return json.loads(path.read_text(encoding="utf-8"))


def write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def numeric(key):
    return int(key.split("-")[1])


def timestamp(value):
    # Jira uses +0000; Python 3.9 fromisoformat requires +00:00.
    normalized = re.sub(r"([+-]\d{2})(\d{2})$", r"\1:\2", value.replace("Z", "+00:00"))
    result = datetime.fromisoformat(normalized)
    if result.tzinfo is None:
        raise ValueError("Timestamp lacks timezone")
    return result


def request_config(out, requests, name):
    lines = ["fail", "silent", "show-error", "connect-timeout = 15", "max-time = 60", "retry = 2", "retry-delay = 2", 'rate = "1/s"']
    for pos, (filename, endpoint, params) in enumerate(requests):
        if pos:
            lines.append("next")
            lines.extend(["fail", "silent", "show-error", "connect-timeout = 15", "max-time = 60", "retry = 2", "retry-delay = 2"])
        url = BASE + endpoint + "?" + urlencode(params)
        lines.extend(["url = " + json.dumps(url), "output = " + json.dumps(str(out / "remote" / filename))])
    (out / "remote").mkdir(exist_ok=True)
    (out / name).write_text("\n".join(lines) + "\n")


def snapshot(out):
    out.mkdir(parents=True, exist_ok=False)
    entries = []
    for path in sorted((ROOT / "data/jira/raw").glob("KAFKA-*.json"), key=lambda p: numeric(p.stem)):
        content = path.read_bytes()
        try:
            value, error = json.loads(content), None
        except Exception as exc:
            value, error = None, str(exc)
        entries.append({"key": path.stem, "path": str(path), "sha256": hashlib.sha256(content).hexdigest(), "bytes": len(content), "value": value, "parse_error": error})
    frozen = {"captured_at_utc": datetime.now(timezone.utc).isoformat(), "entries": entries, "index": read(ROOT / "data/jira/index/issue_keys.json"), "download_report": read(ROOT / "data/jira/index/download_report.json")}
    write(out / "local_snapshot.json", frozen)
    requests = []
    keys = [e["key"] for e in entries]
    for start in range(0, len(keys), 50):
        chunk = keys[start:start + 50]
        requests.append((f"issues-{start:05d}.json", "/rest/api/2/search", {"jql": "key in (" + ",".join(chunk) + ") ORDER BY key ASC", "maxResults": 100, "fields": "*all", "expand": "changelog"}))
    requests.append(("index-00000.json", "/rest/api/2/search", {"jql": frozen["index"]["jql"], "maxResults": 1000, "startAt": 0, "fields": "summary"}))
    request_config(out, requests, "requests-initial.conf")
    print(json.dumps({"snapshot_files": len(keys), "captured_at_utc": frozen["captured_at_utc"], "requests": len(requests), "config": str(out / "requests-initial.conf")}, ensure_ascii=False))


def complete_plan(out):
    first = read(out / "remote/index-00000.json")
    size, total = first["maxResults"], first["total"]
    if size <= 0:
        raise ValueError("Invalid server page size")
    frozen = read(out / "local_snapshot.json")
    requests = []
    for offset in range(size, total, size):
        requests.append((f"index-{offset:05d}.json", "/rest/api/2/search", {"jql": frozen["index"]["jql"], "maxResults": size, "startAt": offset, "fields": "summary"}))
    for path in sorted((out / "remote").glob("issues-*.json")):
        payload = read(path)
        if len(payload.get("issues", [])) != payload.get("total"):
            raise ValueError(f"Incomplete batch {path}")
        for issue in payload["issues"]:
            key = issue["key"]
            comments = issue.get("fields", {}).get("comment", {})
            count, values = comments.get("total"), comments.get("comments", [])
            if isinstance(count, int) and len(values) < count:
                for offset in range(0, count, 100):
                    requests.append((f"comments-{key}-{offset:05d}.json", f"/rest/api/2/issue/{key}/comment", {"startAt": offset, "maxResults": 100}))
            history = issue.get("changelog", {})
            if len(history.get("histories", [])) < history.get("total", 0):
                requests.append((f"history-{key}.json", f"/rest/api/2/issue/{key}/changelog", {"startAt": 0, "maxResults": 1000}))
    if requests:
        request_config(out, requests, "requests-complete.conf")
    print(json.dumps({"server_page_size": size, "source_total": total, "remaining_requests": len(requests), "config": str(out / "requests-complete.conf")}, ensure_ascii=False))


def canonical(value):
    if isinstance(value, dict):
        return {k: canonical(v) for k, v in sorted(value.items())}
    if isinstance(value, list):
        converted = [canonical(v) for v in value]
        if converted and all(isinstance(v, dict) and "id" in v for v in converted):
            return sorted(converted, key=lambda v: str(v["id"]))
        return converted
    return value


def classify_field_difference(field, left, right):
    if field in {"created", "updated", "resolutiondate"} and isinstance(left, str) and isinstance(right, str):
        a, b = timestamp(left), timestamp(right)
        if a == b:
            return "timezone_representation"
        if b.microsecond == 0 and a.replace(microsecond=0) == b:
            return "search_endpoint_second_precision"
    if field == "customfield_12312521" and isinstance(left, str) and isinstance(right, str):
        a, b = (datetime.strptime(value, "%Y-%m-%d %H:%M:%S.%f") for value in (left, right))
        if b.microsecond == 0 and a.replace(microsecond=0) == b:
            return "search_endpoint_second_precision"
    if field == "customfield_12314020" and isinstance(left, str) and isinstance(right, str):
        strip_identity = lambda value: re.sub(r"@[0-9a-fA-F]+", "@RUNTIME_ID", value)
        if strip_identity(left) == strip_identity(right):
            return "runtime_object_identity_only"
    return "content_difference"


def compare(out):
    frozen = read(out / "local_snapshot.json")
    checks = collections.Counter()
    local_problems, differences, remote_incomplete = [], [], []
    remote = {}
    for path in sorted((out / "remote").glob("issues-*.json")):
        payload = read(path)
        for issue in payload["issues"]:
            remote[issue["key"]] = issue
    required = ["summary", "description", "issuetype", "created", "updated", "status", "resolution", "priority", "components", "versions", "fixVersions", "issuelinks", "attachment"]
    for entry in frozen["entries"]:
        key, value = entry["key"], entry["value"]
        if entry["parse_error"]:
            local_problems.append({"key": key, "type": "invalid_json", "detail": entry["parse_error"]})
            continue
        issue, metadata = value.get("issue", {}), value.get("metadata", {})
        fields = issue.get("fields", {})
        comments, history = value.get("comments"), value.get("changelog")
        def problem(kind, detail):
            local_problems.append({"key": key, "type": kind, "detail": detail})
        if issue.get("key") != key:
            problem("key_mismatch", issue.get("key"))
        for field in required:
            if field not in fields:
                problem("missing_field", field)
        for name, rows, reported in [("comments", comments, fields.get("comment", {}).get("total")), ("changelog", history, metadata.get("history_reported_total"))]:
            if not isinstance(rows, list):
                problem("invalid_array", name)
                continue
            if reported is not None and len(rows) != reported:
                problem("count_mismatch", {"array": name, "actual": len(rows), "reported": reported})
            ids = [r.get("id") for r in rows]
            if len(set(ids)) != len(ids):
                problem("duplicate_ids", name)
            for row in rows:
                try:
                    timestamp(row["created"])
                except (ValueError, KeyError, TypeError):
                    problem("invalid_timestamp", {"array": name, "id": row.get("id")})
        expanded = issue.get("changelog", {}).get("histories", [])
        if canonical(history) != canonical(expanded):
            problem("embedded_changelog_differs", "top-level versus issue.changelog.histories")
        if metadata.get("history_complete") is not True:
            problem("history_not_marked_complete", metadata.get("history_complete"))
        checks["parsed_local"] += 1
        if key not in remote:
            differences.append({"key": key, "type": "missing_from_live_batch"})
            continue
        live = remote[key]
        live_comments = live.get("fields", {}).get("comment", {})
        live_history = live.get("changelog", {})
        comment_pages = sorted((out / "remote").glob(f"comments-{key}-*.json"))
        remote_comments = [r for p in comment_pages for r in read(p)["comments"]] if comment_pages else live_comments.get("comments", [])
        history_file = out / "remote" / f"history-{key}.json"
        remote_history = read(history_file).get("values", []) if history_file.exists() else live_history.get("histories", [])
        if len(remote_comments) != live_comments.get("total") or len(remote_history) != live_history.get("total"):
            remote_incomplete.append(key)
        changed = []
        raw_changed = []
        for field in sorted(set(fields) | set(live.get("fields", {}))):
            if field == "comment":
                continue
            left, right = fields.get(field), live.get("fields", {}).get(field)
            if canonical(left) != canonical(right):
                raw_changed.append(field)
                kind = classify_field_difference(field, left, right)
                if kind == "content_difference":
                    changed.append(field)
                differences.append({"key": key, "type": "field_difference", "classification": kind, "field": field, "local": left, "live": right, "local_updated": fields.get("updated"), "live_updated": live.get("fields", {}).get("updated")})
        for name, left, right in [("comments", comments, remote_comments), ("changelog", history, remote_history)]:
            if canonical(left) != canonical(right):
                changed.append(name)
                local_by_id = {r["id"]: r for r in left or []}
                live_by_id = {r["id"]: r for r in right}
                differences.append({"key": key, "type": name + "_difference", "local_count": len(left or []), "live_count": len(right), "missing_ids": sorted(set(live_by_id) - set(local_by_id)), "extra_ids": sorted(set(local_by_id) - set(live_by_id)), "changed_entries": [{"id": rid, "local": local_by_id[rid], "live": live_by_id[rid]} for rid in sorted(set(local_by_id) & set(live_by_id)) if canonical(local_by_id[rid]) != canonical(live_by_id[rid])]})
        checks["live_compared"] += 1
        checks["identical_business_content" if not changed else "different_content"] += 1
        if raw_changed and not changed:
            checks["representation_differences_only"] += 1
        checks["comments_compared"] += len(comments or [])
        checks["history_entries_compared"] += len(history or [])
    index_pages = [read(p) for p in sorted((out / "remote").glob("index-*.json"))]
    source_keys = [i["key"] for page in index_pages for i in page["issues"]]
    expected = set(frozen["index"]["keys"])
    actual = {e["key"] for e in frozen["entries"]}
    source_set = set(source_keys)
    summary = {"captured_at_utc": frozen["captured_at_utc"], "comparison_finished_at_utc": datetime.now(timezone.utc).isoformat(), "local_files": len(actual), "local_index_count": len(frozen["index"]["keys"]), "duplicate_local_index_keys": len(frozen["index"]["keys"]) - len(expected), "source_reported_totals": sorted({p["total"] for p in index_pages}), "source_keys_fetched": len(source_keys), "source_unique_keys": len(source_set), "duplicate_source_keys": len(source_keys) - len(source_set), "source_index_complete": len(source_keys) == len(source_set) == index_pages[0]["total"] and len({p["total"] for p in index_pages}) == 1, "index_missing_from_local": sorted(source_set - expected, key=numeric), "index_local_not_in_source": sorted(expected - source_set, key=numeric), "undownloaded_keys_count": len(expected - actual), "undownloaded_keys": sorted(expected - actual, key=numeric), "local_files_not_in_index": sorted(actual - expected, key=numeric), "checks": dict(checks), "local_problems": local_problems, "difference_count": len(differences), "difference_types": dict(collections.Counter(d["type"] for d in differences)), "difference_fields": dict(collections.Counter(d.get("field") for d in differences if d.get("field"))), "remote_incomplete_keys": remote_incomplete, "download_report": frozen["download_report"], "current_raw_file_count": len(list((ROOT / "data/jira/raw").glob("KAFKA-*.json")))}
    summary["difference_classifications"] = dict(collections.Counter(d.get("classification", "content_difference") for d in differences))
    write(out / "audit_results.json", summary)
    write(out / "differences.json", differences)
    print(json.dumps({k: v for k, v in summary.items() if k != "undownloaded_keys"}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["snapshot", "complete-plan", "compare"])
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    {"snapshot": snapshot, "complete-plan": complete_plan, "compare": compare}[args.mode](args.output.resolve())
