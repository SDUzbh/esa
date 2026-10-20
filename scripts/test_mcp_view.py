#!/usr/bin/env python3
"""Check that verification scripts can decode the tool server's compact text."""
import unittest

import mcp_view


class DecodeTests(unittest.TestCase):
    def test_numbered_document(self):
        text = '\n'.join([
            'streams/src/StreamThread.java at commit 3402a8361b73, lines 511-512',
            'Cite a line by its evidence ID StreamThread.java:<line>#<check>, for example StreamThread.java:511#0fe2',
            '511#0fe2 |     static class Impl {',
            '512#bf93 | ',
            'environment_request: {"reason": "r"}',
        ])
        result = mcp_view.decode(text)
        evidence = result['evidence']
        self.assertEqual((evidence['first_line'], evidence['last_line']), (511, 512))
        self.assertEqual(evidence['lines'][0], {'line': 511, 'id': 'StreamThread.java:511#0fe2', 'text': '    static class Impl {'})
        self.assertEqual(evidence['lines'][1]['text'], '')
        self.assertEqual(result['environment_request'], {'reason': 'r'})

    def test_search_and_json(self):
        text = '\n'.join([
            '2 literal hits at commit f10ef2720b03 (limit reached; more exist, narrow the query). Read a hit with read_code to cite it.',
            'clients/A.java', '  10: a()', 'clients/B.java', '  20: b(x: 1)',
        ])
        self.assertEqual(mcp_view.decode(text), {'hits': [{'file': 'clients/A.java', 'line': 10, 'text': 'a()'},
                                                          {'file': 'clients/B.java', 'line': 20, 'text': 'b(x: 1)'}],
                                                 'truncated': True})
        self.assertEqual(mcp_view.decode('{"status": "granted"}'), {'status': 'granted'})


if __name__ == '__main__':
    unittest.main()
