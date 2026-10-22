"""Decode the ESA tool server's MCP text back into data, for verification scripts.

read_code, run and install_environment print a header and one line each as
`<line>#<check> | text` (see internal/replaytools/render.go); source_search
prints hits grouped by file. A read_code call with several ranges prints one
such block per range, separated by a blank line. Every other tool returns JSON.
"""
import json
import re

LINE = re.compile(r'^(\d+)#([0-9a-f]+) \| (.*)$')
LABEL = re.compile(r'^Cite a line by its evidence ID (.+):<line>#<check>')
HEADER = re.compile(r'^\S.* (at commit \S+|output \S+), lines \d+-\d+$')


def decode(text):
    if text.startswith('{'):
        return json.loads(text)
    rows = text.split('\n')
    if re.match(r'^\d+ literal hits? at commit ', rows[0]):
        hits, file = [], ''
        for row in rows[1:]:
            if row.startswith('  '):
                number, _, line = row.strip().partition(': ')
                hits.append({'file': file, 'line': int(number), 'text': line})
            else:
                file = row
        return {'hits': hits, 'truncated': 'limit reached' in rows[0]}
    blocks = text.split('\n\n')
    if len(blocks) > 1 and all(HEADER.match(block.split('\n', 1)[0]) for block in blocks):
        return {'documents': [document(block)['evidence'] for block in blocks]}
    return document(text)


def document(text):
    rows = text.split('\n')
    result, label, lines = {'environment_request': None}, '', []
    for row in rows[1:]:
        if row.startswith('environment_request: '):
            result['environment_request'] = json.loads(row[len('environment_request: '):])
        elif LABEL.match(row):
            label = LABEL.match(row).group(1)
        elif LINE.match(row):
            number, check, line = LINE.match(row).groups()
            lines.append({'line': int(number), 'id': '%s:%s#%s' % (label, number, check), 'text': line})
    span = re.search(r'lines (\d+)-(\d+)$', rows[0])
    result['evidence'] = {'header': rows[0], 'lines': lines,
                          'first_line': int(span.group(1)) if span else None, 'last_line': int(span.group(2)) if span else None}
    return result
