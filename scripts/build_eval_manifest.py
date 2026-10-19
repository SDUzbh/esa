#!/usr/bin/env python3
"""Build a seeded Agent evaluation set from the preserved Kafka Jira export.

Writes a system_replay manifest, its labels and the provider release registry
for the releases it uses (see docs/TICKET_BACKTEST.md):

* fixed bugs, stratified by primary component, whose single affected version
  is a release tag older than the ticket, with only regular files and whose fix is a first-parent
  trunk commit that names the ticket and changes an existing file that is not a test or documentation
  inside the trace window (label analyze);
* later duplicates of eligible bugs (label reuse), chosen first; their
  originals join the bug sample within their stratum's quota and are marked
  selected=duplicate_original;
* synthetic requests that are not engineering work (label return_to_support).

Nothing here calls a model or a target. Run the manifest with
system-replay --pipeline --order time, then score it with -score.
"""
import argparse
import bisect
import json
import random
import re
import subprocess
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
RELEASE = re.compile(r'^[0-9]+(?:\.[0-9]+){1,3}$')
SUPPORT = [
    ('How do I enable SASL/SCRAM authentication for consumers?',
     'We are setting up a new cluster and want our consumers to authenticate with SCRAM. Which settings do we need on the client side?',
     'Bug'),
    ('Please add me as a contributor so I can assign tickets to myself',
     'I would like to start contributing. Could someone give my Jira account the contributor role?',
     'Task'),
    ('Which version should we upgrade to from our current release?',
     'We run an older release in production and plan an upgrade next quarter. Which release do you recommend and is there a guide?',
     'Wish'),
    ('Where is the documentation for producer configuration?',
     'I cannot find the list of producer settings and their defaults on the website. Could you point me to it?',
     'Improvement'),
    ('Looking for a commercial support contact',
     'Our company needs a support contract for our deployment. Who can we talk to?',
     'Task'),
    ('Please remove me from the mailing list',
     'I keep receiving emails from the users list and cannot find how to unsubscribe. Please remove my address.',
     'Bug'),
    ('How many partitions should a topic have?',
     'We expect about ten thousand messages per second. How should we choose the partition count for our topics?',
     'Wish'),
    ('Is this release supported on our Java version?',
     'Before upgrading, we want to confirm which Java versions are supported by the brokers and the clients.',
     'Improvement'),
    ('Cannot log in to my Jira account',
     'I forgot the password for my Apache Jira account and the reset email never arrives. Can someone help?',
     'Bug'),
    ('Talk proposal for the community meetup',
     'We would like to present our streaming architecture at the next meetup. Who organizes the schedule?',
     'Task'),
]


def git(repo, *args, stdin=None):
    return subprocess.run(['git', '-C', str(repo), *args], input=stdin, capture_output=True, text=True, check=True).stdout


def stamp(text):
    return datetime.fromisoformat(text.replace('Z', '+00:00').replace('+0000', '+00:00'))


def iso(moment):
    return moment.astimezone(timezone.utc).strftime('%Y-%m-%dT%H:%M:%S.%f')[:-3] + 'Z'


DOC_EXTENSIONS = {'.md', '.txt', '.rst', '.adoc', '.html', '.htm', '.png', '.jpg', '.jpeg', '.gif', '.svg'}
DOC_NAMES = ('LICENSE', 'NOTICE', 'README', 'CHANGELOG', 'COPYING')


def fix_file(name):
    """Not a test, documentation or legal text; the scorer (system_replay/score.go) uses the same rule."""
    path = Path(name)
    if path.suffix.lower() in DOC_EXTENSIONS or path.name.upper().startswith(DOC_NAMES) or 'docs' in path.parts[:-1]:
        return False
    return not test_file(name)


def test_file(name):
    parts = name.split('/')
    base = parts[-1]
    return any(p in ('test', 'tests') for p in parts[:-1]) or base.endswith(('Test.java', 'Tests.java')) or base.startswith('test_')


class History:
    """First-parent mainline, its ticket references and release landings, read once."""

    def __init__(self, repo, mainline, project):
        self.repo = repo
        chain = []
        self.refs = {}
        pattern = re.compile(r'\b' + re.escape(project) + r'-\d+\b')
        log = git(repo, 'log', '--first-parent', '--reverse', '--format=%x1e%H %cI%x00%B', mainline)
        for record in log.split('\x1e')[1:]:
            head, _, body = record.partition('\x00')
            commit, date = head.split()
            for key in set(pattern.findall(body)):
                self.refs.setdefault(key, []).append(len(chain))
            chain.append((commit, stamp(date)))
        self.chain = chain
        self.index = {c: i for i, (c, _) in enumerate(chain)}
        # suffix minimum: the last node at or before a moment, as the trace finds it
        self.suffix = [None] * len(chain)
        low = None
        for i in range(len(chain) - 1, -1, -1):
            low = chain[i][1] if low is None or chain[i][1] < low else low
            self.suffix[i] = low
        self.tags, self.clean = {}, {}
        refs = git(repo, 'for-each-ref', '--format=%(refname:strip=2)%09%(objectname)%09%(*objectname)', 'refs/tags')
        ids = {}
        for line in refs.splitlines():
            name, obj, peeled = (line.split('\t') + ['', ''])[:3]
            if RELEASE.match(name):
                ids[name] = peeled or obj
        dates = {}
        shown = git(repo, 'log', '--no-walk=unsorted', '--stdin', '--format=%H %aI %cI', stdin='\n'.join(sorted(set(ids.values()))) + '\n')
        for line in shown.splitlines():
            commit, authored, committed = line.split()
            dates[commit] = (stamp(authored), stamp(committed))
        for name, commit in ids.items():
            if commit in dates:
                authored, committed = dates[commit]
                landing = self.index.get(commit, self.last_at(committed))
                self.tags[name] = (commit, max(authored, committed), committed, landing)

    def exportable(self, tag):
        """The replay snapshot accepts only regular files: no symlinks or submodules."""
        if tag not in self.clean:
            tree = git(self.repo, 'ls-tree', '-r', '--format=%(objectmode)', self.tags[tag][0])
            self.clean[tag] = all(mode in ('100644', '100755') for mode in tree.split())
        return self.clean[tag]

    def last_at(self, moment):
        lo, hi = 0, len(self.suffix)
        while lo < hi:
            mid = (lo + hi) // 2
            if self.suffix[mid] <= moment:
                lo = mid + 1
            else:
                hi = mid
        return lo - 1

    def window(self, cutoff, versions):
        """Mainline indices (anchor, stop] that the trace searches for a fix."""
        anchor = self.last_at(cutoff)
        landings = sorted({landing for _, _, committed, landing in self.tags.values() if committed > cutoff and landing > anchor})
        if not landings:
            return anchor, anchor
        return anchor, landings[min(versions, len(landings)) - 1]

    def fixes(self, key, cutoff, versions):
        anchor, stop = self.window(cutoff, versions)
        return [self.chain[i][0] for i in self.refs.get(key, []) if anchor < i <= stop]

    def changes_existing_source(self, commit):
        out = git(self.repo, 'diff-tree', '-r', '-M', '--no-commit-id', '--name-status', commit + '^', commit)
        for line in out.splitlines():
            status, *names = line.split('\t')
            if status[0] in 'MDR' and names and fix_file(names[0]):
                return True
        return False


def initial_versions(fields, changelog):
    """The affected versions at creation, or None when history edited them."""
    for entry in changelog:
        for item in entry.get('items', []):
            if item.get('field', '').lower() in ('version', 'versions', 'affected version'):
                return None
    return [v.get('name', '') for v in fields.get('versions') or []]


def load(raw_root):
    tickets = {}
    for path in sorted(raw_root.glob('*/*/*/*.json')):
        try:
            data = json.loads(path.read_text())
        except (OSError, ValueError):
            continue
        meta, issue = data.get('metadata', {}), data.get('issue', {})
        fields, changelog = issue.get('fields', {}), data.get('changelog', [])
        if not meta.get('history_complete') or meta.get('history_reported_total') != len(changelog) or fields.get('security'):
            continue
        named = lambda key: (fields.get(key) or {}).get('name', '')
        links = []
        for link in fields.get('issuelinks') or []:
            if (link.get('type') or {}).get('name') == 'Duplicate' and link.get('outwardIssue'):
                links.append(link['outwardIssue']['key'])
        tickets[issue['key']] = {
            'key': issue['key'], 'path': path, 'type': named('issuetype'), 'resolution': named('resolution'),
            'created': stamp(fields['created']), 'versions': initial_versions(fields, changelog),
            'component': sorted(c.get('name', '') for c in fields.get('components') or []) or ['none'], 'duplicates': links}
    return tickets


def usable_version(ticket, history):
    versions = ticket['versions']
    if not versions or len(versions) != 1 or not RELEASE.match(versions[0]) or versions[0] not in history.tags:
        return None
    _, latest, _, _ = history.tags[versions[0]]
    return versions[0] if latest <= ticket['created'] and history.exportable(versions[0]) else None


def allocate(strata, total):
    """Proportional allocation with largest remainders, at most each stratum's size."""
    pool = sum(len(v) for v in strata.values())
    quota = {k: min(len(v), total * len(v) // pool) for k, v in strata.items()}
    order = sorted(strata, key=lambda k: (-(total * len(strata[k]) % pool), k))
    while sum(quota.values()) < min(total, pool):
        for k in order:
            if quota[k] < len(strata[k]) and sum(quota.values()) < total:
                quota[k] += 1
    return quota


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--raw', default=str(ROOT / 'data/jira/raw'), help='Preserved Jira export, laid out as YYYY/MM/DD/KEY.json')
    parser.add_argument('--repo', default=str(ROOT / 'kafka'), help='Full product checkout')
    parser.add_argument('--mainline', default='origin/trunk')
    parser.add_argument('--project', default='KAFKA')
    parser.add_argument('--bugs', type=int, default=60, help='Fixed bugs to sample (50-100 for a full run, about 10 for a pilot)')
    parser.add_argument('--duplicates', type=int, default=10, help='At most this many later duplicates of sampled bugs')
    parser.add_argument('--support', type=int, default=10, help='Synthetic non-engineering tickets (at most %d)' % len(SUPPORT))
    parser.add_argument('--versions', type=int, default=3, help='Future releases the trace searches for each fix')
    parser.add_argument('--seed', type=int, default=20261019)
    parser.add_argument('--mode', choices=('fixture', 'provider'), default='fixture',
                        help='fixture: deterministic provider, no model calls; provider: ESA_ANALYZER_URL and ESA_ANALYZER_TOKEN come from the environment')
    parser.add_argument('--issues-directory', help='provider mode: the Issue projection directory the provider reads')
    parser.add_argument('--timeout', type=int, default=0, help='Seconds to wait for each ticket (default 30 for fixture, 1800 for provider)')
    parser.add_argument('--output', required=True, help='New directory for manifest.json and labels.json')
    args = parser.parse_args()
    if args.mode == 'provider' and not args.issues_directory:
        parser.error('provider mode needs --issues-directory, shared with the provider')
    out = Path(args.output).resolve()
    out.mkdir(parents=True)
    repo = Path(args.repo).resolve()
    history = History(repo, args.mainline, args.project)
    tickets = load(Path(args.raw))
    strata, version_of = {}, {}
    for t in tickets.values():
        if t['type'] != 'Bug' or t['resolution'] != 'Fixed':
            continue
        version = usable_version(t, history)
        if version and history.fixes(t['key'], t['created'], args.versions):
            version_of[t['key']] = version
            strata.setdefault(t['component'][0], []).append(t['key'])
    pool = sum(len(v) for v in strata.values())
    # Small components share one stratum so every stratum can be sampled.
    small = [k for k, v in strata.items() if len(v) < max(2, pool // 50)]
    for k in small:
        strata.setdefault('other', []).extend(strata.pop(k))
    rng = random.Random(args.seed)
    for k in sorted(strata):
        strata[k].sort()
        rng.shuffle(strata[k])
    quota = allocate(strata, args.bugs)
    stratum_of = {key: k for k, keys in strata.items() for key in keys}
    duplicates = []
    for t in sorted(tickets.values(), key=lambda t: t['key']):
        originals = [o for o in t['duplicates'] if o in stratum_of]
        if t['resolution'] == 'Duplicate' and len(originals) == 1 and t['created'] > tickets[originals[0]]['created']:
            version = usable_version(t, history)
            if version:
                duplicates.append((t['key'], originals[0]))
                version_of[t['key']] = version
    rng.shuffle(duplicates)
    kept, used = [], {k: 0 for k in strata}
    for key, original in duplicates:
        if len(kept) == args.duplicates:
            break
        k = stratum_of[original]
        if any(o == original for _, o in kept) or used[k] < quota[k]:
            used[k] += not any(o == original for _, o in kept)
            kept.append((key, original))
    duplicates = sorted(kept)
    originals = {o for _, o in duplicates}
    sample = sorted(originals)
    for k in sorted(strata):
        taken = used[k]
        for key in strata[k]:
            if taken == quota[k]:
                break
            # The trace must find source that existed before the fix.
            if key not in originals and any(history.changes_existing_source(c) for c in history.fixes(key, tickets[key]['created'], args.versions)):
                sample.append(key)
                taken += 1

    def relative(path):
        return str(Path(path).resolve().relative_to(out)) if Path(path).resolve().is_relative_to(out) else str(Path(path).resolve())

    jobs, labels = [], {}
    for key in sorted(sample):
        jobs.append({'id': key, 'raw': relative(tickets[key]['path'])})
        labels[key] = {'expected_route': 'analyze', 'stratum': stratum_of[key], 'selected': 'duplicate_original' if key in originals else 'stratified'}
    for key, original in duplicates:
        jobs.append({'id': key, 'raw': relative(tickets[key]['path'])})
        labels[key] = {'expected_route': 'reuse', 'original': original, 'stratum': 'duplicate'}
    anchors = sorted(sample, key=lambda k: tickets[k]['created'])
    for n, (subject, description, tracker) in enumerate(SUPPORT[:args.support]):
        anchor = anchors[rng.randrange(len(anchors))]
        version = version_of[anchor]
        at = iso(tickets[anchor]['created'] + timedelta(hours=1, minutes=n))
        key = 'SUPPORT-%d' % (n + 1)
        jobs.append({'id': key, 'at': at, 'ref': 'refs/tags/' + version, 'version': version, 'ticket': {
            'id': key, 'source_key': key, 'project_id': '1', 'subject': subject, 'description': description, 'tracker': tracker,
            'status': 'Open', 'source': 'historical_replay', 'components': [], 'versions': [version], 'environment': {},
            'error_codes': [], 'comments': [], 'created_at': at, 'updated_at': at, 'resolution': {}, 'closed': False, 'private': False}})
        labels[key] = {'expected_route': 'return_to_support', 'stratum': 'support'}
    if not jobs:
        sys.exit('no eligible tickets')
    first = min(min(tickets[k]['created'] for k in sample), *(stamp(j['at']) for j in jobs if 'at' in j)) - timedelta(seconds=1)
    bin_dir = str(ROOT / 'bin')
    if args.mode == 'fixture':
        start = ['python3', str(ROOT / 'scripts/esa_fixture_target.py'), '--esa-bin', bin_dir + '/esa-host', '--reports-dir', '{{run_dir}}/target',
                 '--rules', str(ROOT / 'config/rules.json'), '--listen', '127.0.0.1:18090', '--replay-at', iso(first), '--replay-projects', '1']
        env = {'ESA_API_TOKEN': 'api-{{token}}', 'ESA_OPERATOR_TOKEN': '{{token}}', 'ESA_ANALYZER_TOKEN': 'analyzer-{{token}}'}
    else:
        start = [bin_dir + '/esa-host', '-listen', '127.0.0.1:18090', '-ephemeral-schema', '-rules', str(ROOT / 'config/rules.json'),
                 '-issues-directory', str(Path(args.issues_directory).resolve()), '-replay-at', iso(first), '-replay-projects', '1']
        # ESA_ANALYZER_URL and ESA_ANALYZER_TOKEN are inherited: the provider's.
        env = {'ESA_API_TOKEN': 'api-{{token}}', 'ESA_OPERATOR_TOKEN': '{{token}}', 'ESA_REDMINE_URL': ''}
    manifest = {
        'schema_version': 1,
        'target': {
            'url': 'http://127.0.0.1:18090', 'health_path': '/healthz?role=active', 'start': {'args': start, 'env': env},
            'headers': {'X-ESA-Project-IDs': '{{project_id}}'},
            'submit': {'method': 'POST', 'path': '/v1/events', 'body': {
                'id': '{{event_id}}', 'origin': 'system_replay', 'occurred_at': '{{event_time}}',
                'ticket': '{{ticket}}', 'forwarded_by': 'evaluation', 'forwarded_at': '{{event_time}}'}},
            'observe': {'path': '/v1/tickets/{{ticket_id}}'},
            'advance_clock': {'method': 'POST', 'path': '/v1/replay/clock', 'body': {'time': '{{event_time}}'}},
            'extract': {'state': ['/ticket/latest_run/state', '/ticket/case/state'],
                        'route': ['/ticket/latest_run/report/routing/action', '/ticket/latest_run/route'],
                        'run_id': ['/ticket/latest_run/id'], 'issue_id': ['/ticket/engineering_issue/id', '/ticket/latest_run/report/issue/existing_id'],
                        'findings': ['/ticket/latest_run/report/findings'], 'usage': ['/ticket/latest_run/report/usage']},
            'capture': {'run_id': '/ticket/latest_run/id'},
            # Every run state Routing or Engineering can end in, or a support
            # return would wait out the whole timeout.
            'terminal_states': ['completed', 'awaiting_configuration', 'awaiting_forward', 'awaiting_information', 'awaiting_review',
                                'returned_to_support', 'joined', 'superseded', 'issue_removed', 'membership_reviewed', 'failed',
                                'restricted', 'invalidated', 'draft', 'trusted', 'rejected', 'duplicate', 'archived'],
            'timeout_seconds': args.timeout or (30 if args.mode == 'fixture' else 1800), 'poll_milliseconds': 100 if args.mode == 'fixture' else 2000},
        'jobs': jobs, 'repo': str(repo), 'mainline': args.mainline, 'versions': args.versions}
    # Engineering is admitted only for a registered release, so the provider
    # must read this registry (esa-codex -registry) in provider mode.
    used = sorted({j.get('version') or version_of[j['id']] for j in jobs}, key=lambda v: [int(x) for x in v.split('.')])
    releases = [{'project_id': '1', 'version': v, 'ref': 'refs/tags/' + v, 'repository': str(repo), 'scope': 'full',
                 'available_at': history.tags[v][1].astimezone(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')} for v in used]
    (out / 'releases.json').write_text(json.dumps({'releases': releases}, indent=2) + '\n')
    by_stratum = {k: len(v) for k, v in sorted(strata.items())}
    (out / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    (out / 'labels.json').write_text(json.dumps({'seed': args.seed, 'jobs': labels, 'pool': by_stratum, 'meta': {
        'mode': args.mode, 'bugs': len(sample), 'duplicates': len(duplicates), 'support': min(args.support, len(SUPPORT)),
        'eligible_bugs': pool, 'versions': args.versions, 'mainline': args.mainline}}, indent=2) + '\n')
    print(json.dumps({'manifest': str(out / 'manifest.json'), 'labels': str(out / 'labels.json'), 'releases': str(out / 'releases.json'), 'eligible_bugs': pool,
                      'bugs': len(sample), 'duplicates': len(duplicates), 'support': min(args.support, len(SUPPORT))}))


if __name__ == '__main__':
    main()
