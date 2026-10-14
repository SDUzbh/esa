"""Incremental download tests use local fixtures and never contact Jira."""
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

import kafka_jira_downloader as downloader
from jira_dataset import created_path

BASE = "https://issues.apache.org/jira"
OLD = "2024-01-01T12:00:00.123+0000"
NEW = "2024-01-02T12:00:00.456+0000"
QUERY = "project = KAFKA ORDER BY created ASC"


def export(key, updated=OLD, complete=True):
    return {"metadata": {"source": BASE, "history_complete": complete},
            "issue": {"key": key, "fields": {"created": "2015-11-24T00:13:27.387+0000", "updated": updated}},
            "comments": [], "changelog": []}


def store(root, value):
    path = root / created_path(value)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(json.dumps(value, indent=3).encode() + b"\n\n")
    return path


def search(rows):
    return {"total": len(rows), "issues": [{"key": key, "fields": {"updated": updated}} for key, updated in rows]}


def run(root, client, fetch, *flags):
    with patch.object(sys, "argv", ["downloader", "--output", str(root), "--delay", "0", *flags]), \
         patch.object(downloader, "Client", return_value=client), \
         patch.object(downloader, "download_one", side_effect=fetch) as full:
        result = downloader.main()
    return result, full


class IncrementalDownloaderTests(unittest.TestCase):
    def test_incremental_updates_missing_and_changed_preserves_old_bytes_and_query(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            originals = {key: store(root, export(key)) for key in ("KAFKA-1", "KAFKA-2", "KAFKA-4")}
            old_bytes = {key: path.read_bytes() for key, path in originals.items()}
            (root / "index").mkdir()
            downloader.save_json(root / "index/issue_keys.json", {"jql": QUERY, "keys": ["KAFKA-1", "KAFKA-2", "KAFKA-4", "KAFKA-99"]})
            client = Mock()
            client.get.return_value = search([("KAFKA-1", OLD), ("KAFKA-2", NEW), ("KAFKA-3", NEW), ("KAFKA-4", NEW)])
            def fetch(_, key):
                if key == "KAFKA-4":
                    raise OSError("fixture network failure")
                return export(key, NEW)
            result, full = run(root, client, fetch, "--incremental")
            self.assertEqual(result, 1)
            self.assertEqual([call.args[1] for call in full.call_args_list], ["KAFKA-2", "KAFKA-3", "KAFKA-4"])
            self.assertEqual(client.get.call_args.args[1]["jql"], QUERY)
            self.assertEqual(client.get.call_args.args[1]["fields"], "updated")
            self.assertEqual(originals["KAFKA-1"].read_bytes(), old_bytes["KAFKA-1"])
            self.assertEqual(originals["KAFKA-4"].read_bytes(), old_bytes["KAFKA-4"])
            self.assertEqual(json.loads(originals["KAFKA-2"].read_text())["issue"]["fields"]["updated"], NEW)
            archived = list((root / "history/KAFKA-2").glob("*.json"))
            self.assertEqual(len(archived), 1)
            self.assertEqual(archived[0].read_bytes(), old_bytes["KAFKA-2"])
            self.assertEqual(archived[0].stem, hashlib.sha256(old_bytes["KAFKA-2"]).hexdigest())
            index = json.loads((root / "index/issue_keys.json").read_text())
            self.assertEqual(set(index["keys"]), {"KAFKA-1", "KAFKA-2", "KAFKA-3", "KAFKA-4", "KAFKA-99"})
            self.assertEqual(index["jql"], QUERY)
            report = json.loads((root / "index/download_report.json").read_text())
            self.assertEqual((report["downloaded"], report["updated"], report["skipped"], len(report["failed"])), (1, 1, 1, 1))
            # An explicit failed-key retry does not erase the full corpus index.
            client.get.return_value = {"fields": {"updated": NEW}}
            result, full = run(root, client, lambda _, key: export(key, NEW), "--incremental", "--keys", "KAFKA-4")
            self.assertEqual(result, 0)
            full.assert_called_once()
            self.assertEqual(len(json.loads((root / "index/issue_keys.json").read_text())["keys"]), 5)
            client.get.return_value = search([(f"KAFKA-{i}", OLD if i == 1 else NEW) for i in range(1, 5)])
            result, full = run(root, client, lambda *_: self.fail("unchanged export downloaded"), "--incremental")
            self.assertEqual(result, 0)
            full.assert_not_called()
            self.assertEqual(len(list((root / "history").rglob("*.json"))), 2)

    def test_stale_or_incomplete_response_does_not_replace_complete_export(self):
        for response in (export("KAFKA-1", OLD), export("KAFKA-1", NEW, complete=False)):
            with self.subTest(response=response), tempfile.TemporaryDirectory() as folder:
                root = Path(folder)
                path = store(root, export("KAFKA-1"))
                original = path.read_bytes()
                client = Mock()
                client.get.return_value = {"fields": {"updated": NEW}}
                result, _ = run(root, client, lambda *_: response, "--incremental", "--keys", "KAFKA-1")
                self.assertEqual(result, 1)
                self.assertEqual(path.read_bytes(), original)
                self.assertFalse((root / "history").exists())

    def test_unknown_remote_time_and_incomplete_local_history_are_refreshed(self):
        for remote_time, complete in ((None, True), (OLD, False)):
            with self.subTest(remote_time=remote_time), tempfile.TemporaryDirectory() as folder:
                root = Path(folder)
                store(root, export("KAFKA-1", complete=complete))
                client = Mock()
                client.get.return_value = {"fields": {"updated": remote_time}}
                result, full = run(root, client, lambda *_: export("KAFKA-1"), "--incremental", "--keys", "KAFKA-1")
                self.assertEqual(result, 0)
                full.assert_called_once()

    def test_atomic_write_failure_keeps_current_and_archived_exports_retryable(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            path = store(root, export("KAFKA-1"))
            original = path.read_bytes()
            client = Mock()
            client.get.return_value = {"fields": {"updated": NEW}}
            save = downloader.save_json
            def fail_current(destination, value):
                if destination == path:
                    raise OSError("fixture disk failure")
                save(destination, value)
            with patch.object(downloader, "save_json", side_effect=fail_current):
                result, _ = run(root, client, lambda *_: export("KAFKA-1", NEW), "--incremental", "--keys", "KAFKA-1")
            self.assertEqual(result, 1)
            self.assertEqual(path.read_bytes(), original)
            result, _ = run(root, client, lambda *_: export("KAFKA-1", NEW), "--incremental", "--keys", "KAFKA-1")
            self.assertEqual(result, 0)
            self.assertEqual(len(list((root / "history/KAFKA-1").glob("*.json"))), 1)

    def test_search_uses_server_page_lengths_and_rejects_incomplete_pages(self):
        client = Mock()
        client.get.side_effect = [dict(search([("KAFKA-1", OLD)]), total=2), dict(search([("KAFKA-2", NEW)]), total=2)]
        self.assertEqual(downloader.collect_issues(client, QUERY, 0, 50), {"KAFKA-1": OLD, "KAFKA-2": NEW})
        self.assertEqual([c.args[1]["startAt"] for c in client.get.call_args_list], [0, 1])
        for final in ({"total": 2, "issues": []}, dict(search([("KAFKA-1", OLD)]), total=2)):
            client.get.side_effect = [dict(search([("KAFKA-1", OLD)]), total=2), final]
            with self.assertRaises(RuntimeError):
                downloader.collect_issues(client, QUERY, 0, 50)

    def test_comments_and_history_pagination_reject_missing_and_duplicate_rows(self):
        client = Mock()
        for final in ({"total": 2, "values": []}, {"total": 2, "values": [{"id": "1"}]},
                      {"total": 3, "values": [{"id": "2"}]}):
            client.get.side_effect = [{"total": 2, "values": [{"id": "1"}]}, final]
            with self.assertRaises(RuntimeError):
                downloader.pages(client, "/fixture/changelog", "values")
        client.get.side_effect = [{"total": 2, "values": [{"id": "1"}]}, {"total": 2, "values": [{"id": "2"}]}]
        self.assertEqual(len(downloader.pages(client, "/fixture/changelog", "values")), 2)

    def test_download_one_records_full_comments_and_history(self):
        client = Mock(base=BASE)
        issue = export("KAFKA-1")["issue"]
        issue["changelog"] = {"total": 2, "histories": [{"id": "h1"}]}
        client.get.side_effect = [issue,
                                 {"total": 1, "comments": [{"id": "c1"}]},
                                 {"total": 2, "values": [{"id": "h1"}]},
                                 {"total": 2, "values": [{"id": "h2"}]}]
        value = downloader.download_one(client, "KAFKA-1")
        self.assertEqual(len(value["comments"]), 1)
        self.assertEqual(len(value["changelog"]), 2)
        self.assertTrue(value["metadata"]["history_complete"])
        # A count changed between the initial issue and paginated history.
        client.get.side_effect = [issue, {"total": 0, "comments": []}, {"total": 1, "values": [{"id": "h1"}]}]
        with self.assertRaises(RuntimeError):
            downloader.download_one(client, "KAFKA-1")


if __name__ == "__main__":
    unittest.main()
