#!/usr/bin/env python3
"""Audit preserved Jira version evidence without changing data or installing software."""
import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import subprocess

from jira_dataset import issue_files

ROOT = Path(__file__).resolve().parents[1]
VERSION = re.compile(r'(?<![\w.])\d+\.\d+(?:\.\d+){0,2}(?:-(?:SNAPSHOT|beta\d*|rc\d*|cp))?(?![\w.])', re.I)
VERSION_FIELDS = {'version', 'versions', 'affected version', 'affects version/s', 'affected versions'}


def timestamp(value):
    if not value:
        return None
    try:
        normalized = value.replace('Z', '+00:00')
        normalized = re.sub(r'([+-]\d{2})(\d{2})$', r'\1:\2', normalized)
        return datetime.fromisoformat(normalized)
    except ValueError:
        return None


def names(values):
    return list(dict.fromkeys(v['name'] for v in (values or []) if isinstance(v, dict) and v.get('name')))


def tags(repo):
    fmt = '%(refname:short)%09%(authordate:iso-strict)%09%(committerdate:iso-strict)%09%(*authordate:iso-strict)%09%(*committerdate:iso-strict)'
    result = subprocess.run(['git', '-C', str(repo), 'for-each-ref', '--format=' + fmt, 'refs/tags'], check=True, text=True, capture_output=True)
    out = {}
    for line in result.stdout.splitlines():
        name, author, committer, peeled_author, peeled_committer = line.split('\t')
        out[name] = (timestamp(peeled_author or author), timestamp(peeled_committer or committer))
    return out


def inspect(path, git_tags):
    raw = path.read_bytes()
    export = json.loads(raw)
    issue = export['issue']; fields = issue['fields']; metadata = export.get('metadata', {})
    affected = names(fields.get('versions'))
    exact = [v for v in affected if v in git_tags]
    created = timestamp(fields.get('created'))
    eligible = [v for v in exact if created and all(t and t <= created for t in git_tags[v])]
    history = export.get('changelog', [])
    changes = []
    for entry in history:
        at = timestamp(entry.get('created'))
        if created and at and at > created:
            changes.extend(item for item in entry.get('items', []) if item.get('field', '').lower() in VERSION_FIELDS)
    text_candidates = []
    for field in ('summary', 'description', 'environment'):
        value = fields.get(field)
        if not isinstance(value, str):
            continue
        for match in VERSION.finditer(value):
            candidate = match.group()
            if candidate in git_tags:
                text_candidates.append({'field': field, 'candidate': candidate,
                                        'context': ' '.join(value[max(0, match.start()-45):match.end()+45].split())})
    unique_text = list(dict.fromkeys(x['candidate'] for x in text_candidates))
    history_complete = bool(metadata.get('history_complete')) and metadata.get('history_reported_total') == len(history)
    return {'key': issue['key'], 'path': str(path.relative_to(ROOT)), 'sha256': hashlib.sha256(raw).hexdigest(),
            'type': fields.get('issuetype', {}).get('name'), 'subject': fields.get('summary'),
            'created': fields.get('created'), 'affected_versions': affected,
            'fix_versions': names(fields.get('fixVersions')), 'components': names(fields.get('components')),
            'environment_present': bool(fields.get('environment')), 'exact_local_tags': exact,
            'tags_not_after_creation': eligible, 'complete_count_checked_history': history_complete,
            'affected_version_changes_after_creation': len(changes),
            'text_tag_candidates': unique_text, 'text_candidate_examples': text_candidates[:3]}


def summarize(rows):
    single = [r for r in rows if len(r['affected_versions']) == 1]
    return {'total': len(rows), 'with_affected_versions': sum(bool(r['affected_versions']) for r in rows),
            'single_affected_version': len(single),
            'multiple_affected_versions': sum(len(r['affected_versions']) > 1 for r in rows),
            'missing_affected_versions': sum(not r['affected_versions'] for r in rows),
            'single_version_with_exact_local_tag': sum(len(r['exact_local_tags']) == 1 for r in single),
            'single_exact_tag_not_after_creation': sum(len(r['tags_not_after_creation']) == 1 for r in single),
            'single_exact_tag_time_checked_no_version_changes_complete_history': sum(
                len(r['tags_not_after_creation']) == 1 and not r['affected_version_changes_after_creation'] and r['complete_count_checked_history'] for r in single),
            'at_least_one_exact_local_tag': sum(bool(r['exact_local_tags']) for r in rows),
            'environment_present': sum(r['environment_present'] for r in rows),
            'missing_field_with_text_tag_candidates': sum(not r['affected_versions'] and bool(r['text_tag_candidates']) for r in rows),
            'affected_version_changes_after_creation': sum(bool(r['affected_version_changes_after_creation']) for r in rows),
            'complete_count_checked_history': sum(r['complete_count_checked_history'] for r in rows)}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--out', default='reports/ticket-version-audit.json')
    args = parser.parse_args()
    files = sorted(issue_files(ROOT / 'data/jira/raw'))
    git_tags = tags(ROOT / 'kafka')
    rows = []; problems = []
    for path in files:
        try:
            rows.append(inspect(path, git_tags))
        except (ValueError, KeyError, TypeError) as error:
            problems.append({'path': str(path.relative_to(ROOT)), 'error': str(error)})
    rows.sort(key=lambda r: int(r['key'].split('-')[-1]))
    bugs = [r for r in rows if r['type'] == 'Bug']
    example_keys = ('KAFKA-2880', 'KAFKA-1145', 'KAFKA-1146', 'KAFKA-974', 'KAFKA-1665', 'KAFKA-1869', 'KAFKA-3171')
    by_key = {r['key']: r for r in rows}
    samples = [by_key[key] for key in example_keys if key in by_key]
    predicates = [
        lambda r: len(r['affected_versions']) == 1 and bool(r['tags_not_after_creation']) and not r['affected_version_changes_after_creation'],
        lambda r: len(r['affected_versions']) > 1 and len(r['exact_local_tags']) > 1,
        lambda r: not r['affected_versions'] and bool(r['text_tag_candidates']),
        lambda r: len(r['affected_versions']) == 1 and not r['exact_local_tags'],
        lambda r: not r['affected_versions'] and not r['text_tag_candidates'],
    ]
    for predicate in predicates:
        samples.extend([r for r in bugs if predicate(r) and r['key'] not in {s['key'] for s in samples}][:2])
    unmatched = Counter(v for r in rows for v in r['affected_versions'] if v not in git_tags)
    report = {'audited_at_utc': datetime.now(timezone.utc).isoformat(), 'file_count_at_start': len(files),
              'all': summarize(rows), 'bug': summarize(bugs), 'types': dict(Counter(r['type'] for r in rows)),
              'unmapped_version_occurrences': dict(unmatched), 'samples': samples, 'problems': problems,
              'limitations': [
                  'Counts describe downloaded current fields, not a reconstructed initial ticket snapshot.',
                  'Creation-time tag checks alone do not prove a field was visible then. Historical reconstruction remains required.',
                  'Text candidates can refer to dependencies, fixed versions, or unrelated numbers; they are not authoritative runtime versions.',
                  'Release names and dates inside Jira version objects reflect export-time metadata and are not historical truth.',
                  'Exact Git tag matches do not establish availability of a runnable artifact or a complete customer environment.',
                  'Fix versions and post-cutoff comments are excluded from affected-version candidate selection.',
                  'Downloads may continue after this audit; this report preserves the files seen at start and their hashes.'
              ], 'tickets': rows}
    out = ROOT / args.out; out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(report, indent=2, ensure_ascii=False) + '\n')
    lines = ['# Jira version evidence audit', '', 'Snapshot: ' + report['audited_at_utc'], '',
             '| Metric | All tickets | Bugs |', '| --- | ---: | ---: |']
    for key, value in report['all'].items():
        lines.append(f'| {key} | {value} | {report["bug"][key]} |')
    lines += ['', '## Representative samples', '', '| Ticket | Affected versions | Exact local tags | Text candidates |', '| --- | --- | --- | --- |']
    for row in samples:
        lines.append('| ' + ' | '.join([row['key'], ', '.join(row['affected_versions']) or 'None', ', '.join(row['exact_local_tags']) or 'None', ', '.join(row['text_tag_candidates']) or 'None']) + ' |')
    lines += ['', '## Limits', ''] + ['- ' + x for x in report['limitations']]
    out.with_suffix('.md').write_text('\n'.join(lines) + '\n')
    print(json.dumps({k: report[k] for k in ('audited_at_utc', 'all', 'bug', 'unmapped_version_occurrences', 'samples', 'problems')}, indent=2, ensure_ascii=False))


if __name__ == '__main__':
    main()
