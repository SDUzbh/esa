"""Shared layout rules for preserved Jira JSON exports; standard library only."""
from contextlib import contextmanager
from datetime import datetime, timezone
import fcntl
from pathlib import Path
import re


def timestamp(value):
    """Parse Jira's offset format, including +0000, on Python 3.9+."""
    if not isinstance(value, str):
        raise ValueError("Missing Jira timestamp")
    value = re.sub(r"([+-]\d{2})(\d{2})$", r"\1:\2", value.replace("Z", "+00:00"))
    result = datetime.fromisoformat(value)
    if result.tzinfo is None:
        raise ValueError("Jira timestamp requires a timezone")
    return result.astimezone(timezone.utc)


def created_path(export):
    """Return raw/YYYY/MM/DD/KEY.json using the original creation time in UTC."""
    issue = export["issue"]
    key = issue["key"]
    if not isinstance(key, str) or not re.fullmatch(r"[A-Z][A-Z0-9_]*-[0-9]+", key):
        raise ValueError("Invalid Jira issue key")
    created = timestamp(issue["fields"]["created"])
    return Path("raw") / f"{created.year:04d}" / f"{created.month:02d}" / f"{created.day:02d}" / (key + ".json")


def issue_files(raw):
    """Read flat or dated layouts and reject ambiguous duplicate issue keys."""
    found = {}
    for path in sorted(Path(raw).rglob("*.json")):
        if path.is_symlink():
            raise ValueError("Symlink is not an original Jira export: " + str(path))
        if not path.is_file():
            continue
        if path.stem in found:
            raise ValueError(f"Duplicate issue key {path.stem}: {found[path.stem]} and {path}")
        found[path.stem] = path
    return list(found.values())


@contextmanager
def dataset_writer(root):
    """Prevent the downloader and organizer from changing paths concurrently."""
    index = Path(root) / "index"
    index.mkdir(parents=True, exist_ok=True)
    with (index / ".layout.lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise RuntimeError("Another downloader or organizer is using this dataset") from exc
        try:
            yield
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)
