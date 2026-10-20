#!/usr/bin/env python3
"""Build an offline routing-search corpus from the preserved Kafka Jira export.

Writes, for tools/routing-eval (review Issue 15):

* corpus.jsonl: every ticket whose final resolution is Fixed, as the Routing
  Agent's directory would describe it: key, creation and resolution times,
  summary, description and environment as they were at creation (the
  changelog's first earlier value, so later edits do not leak in), components
  and the affected versions at creation;
* cases.json: duplicates whose single Duplicate link (either direction) is a
  Fixed ticket created earlier, plus as many negative controls (Fixed tickets
  with no Duplicate link at all) drawn from the same years, each with the
  queries a Routing Agent sends or could send, and its text as filed.

Nothing here calls a model or a service.
"""
import argparse
import json
import random
import re
import sys
from collections import Counter
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from jira_dataset import issue_files, timestamp  # noqa: E402

FRAME = re.compile(r'\bat (?:org\.apache\.kafka|kafka)\.(?:[\w$]+\.)*?([A-Z][\w$]*)\.[\w$<>]+\(')
EXCEPTION = re.compile(r'\b([A-Z]\w*(?:Exception|Error))\b')
IDENTIFIER = re.compile(r'\b([A-Z][a-z0-9]+(?:[A-Z][a-z0-9]+)+)\b')


def initial(fields, changelog, name, jira_field):
    """The field's value at creation: the oldest change's previous value, if any."""
    for entry in sorted(changelog, key=lambda e: e.get('created', '')):
        for item in entry.get('items', []):
            if item.get('field') == jira_field:
                return item.get('fromString') or ''
    return fields.get(name) or ''


def initial_versions(fields, changelog):
    """Affected versions at creation; the current ones when history edited them."""
    names = [v.get('name', '') for v in fields.get('versions') or []]
    for entry in sorted(changelog, key=lambda e: e.get('created', '')):
        for item in entry.get('items', []):
            if item.get('field') == 'Version':
                # An added version was not there at creation; a removed one was.
                if item.get('toString') in names:
                    names.remove(item['toString'])
                if item.get('fromString') and item['fromString'] not in names:
                    names.append(item['fromString'])
    return names


def load(raw):
    tickets = {}
    for path in issue_files(raw):
        data = json.loads(path.read_text())
        issue, changelog = data['issue'], data.get('changelog') or []
        fields = issue['fields']
        if fields.get('security'):
            continue
        links = set()
        for link in fields.get('issuelinks') or []:
            if (link.get('type') or {}).get('name') == 'Duplicate':
                for side in ('outwardIssue', 'inwardIssue'):
                    if link.get(side):
                        links.add(link[side]['key'])
        tickets[issue['key']] = {
            'key': issue['key'],
            'created': timestamp(fields['created']).isoformat(),
            'resolved': fields.get('resolutiondate') and timestamp(fields['resolutiondate']).isoformat(),
            'resolution': (fields.get('resolution') or {}).get('name') or '',
            'summary': initial(fields, changelog, 'summary', 'summary'),
            'description': initial(fields, changelog, 'description', 'description'),
            'environment': initial(fields, changelog, 'environment', 'environment'),
            'components': sorted(c.get('name', '') for c in fields.get('components') or []),
            'versions': initial_versions(fields, changelog),
            'duplicate_links': sorted(links),
        }
    return tickets


def unique(values, limit):
    seen = []
    for v in values:
        if v not in seen:
            seen.append(v)
    return seen[:limit]


def queries(t):
    """Query styles: what the Agent sent in the catalog run, and what it could send."""
    text = t['description'] + '\n' + t['environment']
    frames = unique(FRAME.findall(text), 5)
    exceptions = unique(EXCEPTION.findall(t['summary'] + '\n' + text), 3)
    identifiers = frames or unique(IDENTIFIER.findall(t['summary'] + '\n' + text), 1)
    q = {'browse': ''}
    if identifiers:
        q['identifier'] = identifiers[0]
    if t['components'] or t['versions']:
        q['component_version'] = ' '.join(t['components'][:1] + t['versions'][:1])
    q['title'] = t['summary']
    if exceptions:
        q['exceptions'] = ' '.join(exceptions)
        q['title_exceptions'] = t['summary'] + ' ' + ' '.join(exceptions)
    if frames:
        q['frames'] = ' '.join(frames)
    return q


def text(t):
    """The incoming ticket as filed, for signatures and ESA-built queries."""
    return {k: t[k] for k in ('summary', 'description', 'environment')}


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--raw', default='data/jira/raw')
    parser.add_argument('--output', required=True, help='New directory for corpus.jsonl and cases.json')
    parser.add_argument('--seed', type=int, default=20261021)
    args = parser.parse_args()
    out = Path(args.output)
    out.mkdir(parents=True)
    tickets = load(Path(args.raw))
    fixed = [t for t in tickets.values() if t['resolution'] == 'Fixed']
    with (out / 'corpus.jsonl').open('w') as f:
        for t in sorted(fixed, key=lambda t: t['created']):
            row = {k: t[k] for k in ('key', 'created', 'resolved', 'summary', 'description', 'environment', 'components', 'versions')}
            f.write(json.dumps(row) + '\n')
    cases, skipped = [], Counter()
    for t in sorted(tickets.values(), key=lambda t: t['created']):
        if t['resolution'] != 'Duplicate':
            continue
        originals = [k for k in t['duplicate_links']
                     if k in tickets and tickets[k]['resolution'] == 'Fixed' and tickets[k]['created'] < t['created']]
        if len(originals) != 1:
            skipped['no single earlier fixed original' if not originals else 'several originals'] += 1
            continue
        o = tickets[originals[0]]
        cases.append({'kind': 'duplicate', 'key': t['key'], 'created': t['created'], 'original': o['key'],
                      'original_fixed_before': bool(o['resolved'] and o['resolved'] <= t['created']), 'queries': queries(t), **text(t)})
    # Controls: tickets nobody marked as a duplicate of anything, from the same years.
    rng = random.Random(args.seed)
    years = Counter(c['created'][:4] for c in cases)
    pool = {}
    for t in fixed:
        if not t['duplicate_links']:
            pool.setdefault(t['created'][:4], []).append(t)
    for year, n in sorted(years.items()):
        for t in rng.sample(sorted(pool.get(year, []), key=lambda t: t['key']), min(n, len(pool.get(year, [])))):
            cases.append({'kind': 'control', 'key': t['key'], 'created': t['created'], 'queries': queries(t), **text(t)})
    (out / 'cases.json').write_text(json.dumps(sorted(cases, key=lambda c: c['created']), indent=1) + '\n')
    print(json.dumps({'fixed_records': len(fixed), 'duplicates': sum(c['kind'] == 'duplicate' for c in cases),
                      'controls': sum(c['kind'] == 'control' for c in cases), 'skipped_duplicates': dict(skipped)}))


if __name__ == '__main__':
    main()
