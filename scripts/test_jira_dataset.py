import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import kafka_jira_downloader as downloader
from jira_dataset import created_path, dataset_writer, issue_files
from organize_jira_data import apply_layout, plan_layout


def export(key="KAFKA-1", created="2015-11-24T00:13:27.387+0000"):
    return {"metadata": {"history_complete": True}, "issue": {"key": key, "fields": {"created": created}},
            "comments": [], "changelog": []}


class JiraDatasetTests(unittest.TestCase):
    def test_creation_date_uses_utc_and_requires_valid_provenance(self):
        self.assertEqual(created_path(export(created="2020-01-01T00:30:00+0800")), Path("raw/2019/12/31/KAFKA-1.json"))
        self.assertEqual(created_path(export(created="2020-01-01T00:00:00Z")), Path("raw/2020/01/01/KAFKA-1.json"))
        for value in ("2020-01-01T00:00:00", "invalid", None):
            with self.assertRaises(ValueError):
                created_path(export(created=value))
        with self.assertRaises(ValueError):
            created_path(export(key="../../escape"))

    def test_migration_preserves_bytes_mtime_and_is_idempotent(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / "raw").mkdir()
            original = json.dumps(export(), indent=3).encode() + b"\n\n"
            source = root / "raw/KAFKA-1.json"
            source.write_bytes(original)
            mtime = source.stat().st_mtime_ns
            self.assertEqual(len(plan_layout(root)), 1)
            self.assertFalse((root / "index").exists())  # Preview is read-only.
            result = apply_layout(root)
            target = root / created_path(export())
            self.assertFalse(source.exists())
            self.assertEqual(target.read_bytes(), original)
            self.assertEqual(target.stat().st_mtime_ns, mtime)
            manifest = json.loads(Path(result["manifest"]).read_text())
            self.assertEqual(manifest["status"], "completed")
            self.assertEqual(manifest["entries"][0]["sha256"], hashlib.sha256(original).hexdigest())
            self.assertEqual(issue_files(root / "raw"), [target])
            self.assertEqual(apply_layout(root)["status"], "already_organized")
            self.assertEqual(len(list((root / "index").glob("created-layout-*.json"))), 1)

    def test_bad_dates_and_duplicate_keys_fail_before_moving(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / "raw").mkdir()
            source = root / "raw/KAFKA-1.json"
            source.write_text(json.dumps(export()))
            bad = root / "raw/KAFKA-2.json"
            bad.write_text(json.dumps(export(key="KAFKA-2", created="invalid")))
            with self.assertRaises(ValueError):
                apply_layout(root)
            self.assertTrue(source.exists())
            bad.unlink()
            target = root / created_path(export())
            target.parent.mkdir(parents=True)
            target.write_text("different content")
            with self.assertRaisesRegex(ValueError, "Duplicate issue key"):
                apply_layout(root)
            self.assertEqual(target.read_text(), "different content")
            self.assertTrue(source.exists())

    def test_downloader_skips_dated_files_and_writes_new_files_by_date(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            target = root / created_path(export())
            target.parent.mkdir(parents=True)
            target.write_text(json.dumps(export()))
            with patch.object(sys, "argv", ["downloader", "--output", folder, "--keys", "KAFKA-1", "KAFKA-2"]), \
                 patch.object(downloader, "download_one", return_value=export(key="KAFKA-2")) as download:
                self.assertEqual(downloader.main(), 0)
                download.assert_called_once()
                self.assertEqual(download.call_args.args[1], "KAFKA-2")
            self.assertTrue((root / created_path(export(key="KAFKA-2"))).is_file())
            self.assertFalse((root / "raw/KAFKA-2.json").exists())

    def test_writer_lock_refuses_concurrent_migration(self):
        with tempfile.TemporaryDirectory() as folder:
            with dataset_writer(Path(folder)):
                with self.assertRaisesRegex(RuntimeError, "Another downloader"):
                    with dataset_writer(Path(folder)):
                        self.fail("concurrent writer acquired lock")


if __name__ == "__main__":
    unittest.main()
