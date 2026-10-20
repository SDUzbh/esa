#!/usr/bin/env python3
"""Check that the routing corpus shows tickets as filed and finds usable duplicates."""
import contextlib
import io
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import build_routing_corpus as corpus


def export(root, key, created, summary, description, resolution, links=(), changelog=(), versions=('0.10.1.0',), resolved=None):
    fields = {'created': created, 'summary': summary, 'description': description, 'environment': None,
              'resolution': {'name': resolution} if resolution else None, 'resolutiondate': resolved,
              'components': [{'name': 'core'}], 'versions': [{'name': v} for v in versions],
              'issuelinks': [{'type': {'name': 'Duplicate'}, 'outwardIssue': {'key': k}} for k in links]}
    path = Path(root) / '2016' / '01' / '01' / (key + '.json')
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({'metadata': {}, 'issue': {'key': key, 'fields': fields}, 'changelog': list(changelog)}))


class CorpusTests(unittest.TestCase):
    def test_tickets_as_filed_and_usable_duplicates(self):
        with tempfile.TemporaryDirectory() as root:
            raw = Path(root) / 'raw'
            edit = {'created': '2016-01-03T00:00:00.000+0000', 'items': [
                {'field': 'summary', 'fromString': 'Broker hangs', 'toString': 'GroupMetadata deadlock'},
                {'field': 'Version', 'fromString': None, 'toString': '0.10.1.0'}]}
            export(raw, 'KAFKA-1', '2016-01-01T00:00:00.000+0000', 'GroupMetadata deadlock',
                   'at kafka.coordinator.GroupMetadataManager.store(GroupMetadataManager.scala:1)\nIllegalStateException',
                   'Fixed', changelog=[edit], versions=('0.10.0.0', '0.10.1.0'), resolved='2016-01-05T00:00:00.000+0000')
            export(raw, 'KAFKA-2', '2016-01-02T00:00:00.000+0000', 'Consumer stuck', 'no trace', 'Fixed')
            export(raw, 'KAFKA-3', '2016-01-04T00:00:00.000+0000', 'Deadlock again', 'same as before', 'Duplicate', links=['KAFKA-1'])
            # A duplicate of a later ticket is not a usable case.
            export(raw, 'KAFKA-4', '2016-01-01T12:00:00.000+0000', 'Earlier report', 'text', 'Duplicate', links=['KAFKA-2'])
            out = Path(root) / 'out'
            argv = ['build_routing_corpus.py', '--raw', str(raw), '--output', str(out)]
            with mock.patch.object(sys, 'argv', argv), contextlib.redirect_stdout(io.StringIO()):
                corpus.main()
            rows = {r['key']: r for r in map(json.loads, (out / 'corpus.jsonl').read_text().splitlines())}
            self.assertEqual(sorted(rows), ['KAFKA-1', 'KAFKA-2'])
            self.assertEqual(rows['KAFKA-1']['summary'], 'Broker hangs')
            self.assertEqual(rows['KAFKA-1']['versions'], ['0.10.0.0'])
            cases = json.loads((out / 'cases.json').read_text())
            duplicates = [c for c in cases if c['kind'] == 'duplicate']
            self.assertEqual([(c['key'], c['original']) for c in duplicates], [('KAFKA-3', 'KAFKA-1')])
            self.assertTrue(duplicates[0]['original_fixed_before'] is False)
            self.assertEqual(duplicates[0]['summary'], 'Deadlock again')
            self.assertEqual([c['kind'] for c in cases].count('control'), 1)

    def test_queries(self):
        q = corpus.queries({'summary': 'Deadlock in GroupMetadata', 'components': ['core'], 'versions': ['0.10.1.0'], 'environment': '',
                            'description': 'java.lang.IllegalStateException\n\tat kafka.coordinator.GroupMetadataManager.store(X.scala:1)'})
        self.assertEqual(q['browse'], '')
        self.assertEqual(q['identifier'], 'GroupMetadataManager')
        self.assertEqual(q['component_version'], 'core 0.10.1.0')
        self.assertEqual(q['frames'], 'GroupMetadataManager')
        self.assertEqual(q['exceptions'], 'IllegalStateException')


if __name__ == '__main__':
    unittest.main()
