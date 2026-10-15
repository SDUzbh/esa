#!/usr/bin/env python3
"""Organize Jira exports by original creation date in UTC, without rewriting JSON."""
import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import sys

from jira_dataset import created_path, dataset_writer, issue_files


def plan_layout(root):
    """Validate every export and destination before proposing any relocation."""
    if not (root / "raw").is_dir():
        raise ValueError("Dataset must contain a raw directory: " + str(root))
    entries = []
    for source in issue_files(root / "raw"):
        content = source.read_bytes()
        try:
            target = root / created_path(json.loads(content))
        except (ValueError, KeyError, TypeError) as exc:
            raise ValueError(f"Cannot organize {source}: {exc}") from exc
        if source.name != target.name:
            raise ValueError("Issue key does not match filename: " + str(source))
        if target != source and (target.exists() or target.is_symlink()):
            raise ValueError("Destination already exists: " + str(target))
        # Refuse a redirected date directory before performing any migration.
        for directory in target.parents:
            if directory == root:
                break
            if directory.is_symlink():
                raise ValueError("Symlink in destination path: " + str(directory))
        entries.append({"key": source.stem, "source": str(source.relative_to(root)),
                        "target": str(target.relative_to(root)), "bytes": len(content),
                        "sha256": hashlib.sha256(content).hexdigest()})
    return entries


def summary(entries):
    return {"files": len(entries), "bytes": sum(e["bytes"] for e in entries),
            "to_move": sum(e["source"] != e["target"] for e in entries),
            "years": dict(sorted(Counter(Path(e["target"]).parts[1] for e in entries).items())),
            "date_field": "issue.fields.created", "timezone": "UTC"}


def write_manifest(path, value):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(value, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    temporary.replace(path)


def apply_layout(root):
    """Record a manifest, relocate unchanged bytes and verify destination hashes."""
    with dataset_writer(root):
        entries = plan_layout(root)  # Validate the entire batch before moving anything.
        result = summary(entries)
        if result["to_move"] == 0:
            return dict(result, verified=True, status="already_organized")
        now = datetime.now(timezone.utc)
        manifest_path = root / "index" / ("created-layout-" + now.strftime("%Y%m%dT%H%M%S%fZ") + ".json")
        manifest = {"status": "planned", "created_at_utc": now.isoformat(), **result, "entries": entries}
        write_manifest(manifest_path, manifest)
        try:
            for entry in entries:
                source, target = root / entry["source"], root / entry["target"]
                if source == target:
                    continue
                if hashlib.sha256(source.read_bytes()).hexdigest() != entry["sha256"]:
                    raise ValueError("Source changed after planning: " + str(source))
                target.parent.mkdir(parents=True, exist_ok=True)
                if target.exists() or target.is_symlink():
                    raise ValueError("Destination appeared during migration: " + str(target))
                source.rename(target)
            # Verify bytes, count and paths independently after migration.
            if {p.relative_to(root).as_posix() for p in issue_files(root / "raw")} != {e["target"] for e in entries}:
                raise ValueError("File set changed during migration")
            for entry in entries:
                if hashlib.sha256((root / entry["target"]).read_bytes()).hexdigest() != entry["sha256"]:
                    raise ValueError("Checksum verification failed: " + entry["target"])
            manifest["status"] = "completed"
        except BaseException:
            manifest["status"] = "interrupted"
            write_manifest(manifest_path, manifest)
            raise
        write_manifest(manifest_path, manifest)
        return dict(result, verified=True, status="completed", manifest=str(manifest_path))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("dataset", nargs="?", type=Path, default=Path(__file__).resolve().parents[1] / "data/jira")
    parser.add_argument("--apply", action="store_true", help="Move files and verify SHA-256; default only previews")
    args = parser.parse_args()
    root = args.dataset.resolve()
    result = apply_layout(root) if args.apply else dict(summary(plan_layout(root)), status="preview")
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, RuntimeError) as exc:
        sys.exit("Jira organization failed: " + str(exc))
