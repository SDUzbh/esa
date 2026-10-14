#!/usr/bin/env python3
"""Exercise the current plugin persistence models in an isolated PostgreSQL DB."""
import json
from pathlib import Path
import subprocess
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
DOCKER = ['docker', '--context', 'colima-redmine']
RUBY = '''
require 'json'
require 'active_record'
ActiveRecord::Base.establish_connection(adapter: 'postgresql', host: 'db', database: 'esa_test', username: 'esa_test', password: 'fixture-only', pool: 5)
class ApplicationRecord < ActiveRecord::Base
  self.abstract_class = true
end
sources = JSON.parse(STDIN.read)
sources.each { |name, source| eval(source, TOPLEVEL_BINDING, name) }
ActiveRecord::Migration.verbose = false
CreateEsaStateRecords.new.migrate(:up)
state = EsaBackendState.authority
seed = state.packet.fetch(:state)
input = Marshal.load(Marshal.dump(seed))
input['snapshots'] = {'snapshot_1' => {'id' => 'snapshot_1', 'ticket' => {'project_id' => '1'}, 'hash' => 'original'}}
input['engineering_issues'] = {'issue_1' => {'id' => 'issue_1', 'project_id' => '1', 'title' => 'Retention fixture'}}
input['solution_revisions'] = {'solution_1' => {'id' => 'solution_1', 'project_id' => '1', 'issue_id' => 'issue_1'}}
results = Queue.new
threads = 2.times.map do
  Thread.new do
    ActiveRecord::Base.connection_pool.with_connection do
      results << EsaBackendState.authority.commit!(input, 0)
    end
  end
end
threads.each(&:join)
raise 'CAS allowed duplicate commits' unless 2.times.map { results.pop }.sort_by(&:to_s) == [false, true]
packet = EsaBackendState.authority.packet
raise 'wrong revision' unless packet[:revision] == 1
raise 'duplicate authoritative frame' if JSON.parse(EsaBackendState.authority.payload).key?('snapshots')
changed = Marshal.load(Marshal.dump(packet[:state]))
changed['snapshots']['snapshot_1']['hash'] = 'changed'
begin
  EsaBackendState.authority.commit!(changed, 1)
  raise 'immutable snapshot changed'
rescue ArgumentError
end
raise 'rollback failed' unless EsaBackendState.authority.packet == packet
removed = Marshal.load(Marshal.dump(packet[:state]))
removed.delete('snapshots')
begin
  EsaBackendState.authority.commit!(removed, 1)
  raise 'immutable snapshot removed'
rescue ArgumentError
end
raise 'delete rollback failed' unless EsaBackendState.authority.packet == packet
pruned = Marshal.load(Marshal.dump(packet[:state]))
pruned['engineering_issues'] = {}
raise 'issue eviction failed' unless EsaBackendState.authority.commit!(pruned, 1)
retained = EsaBackendState.authority.packet[:state]
raise 'issue retained' unless retained.fetch('engineering_issues', {}).empty?
raise 'eviction changed immutable history' unless retained['snapshots'] == packet[:state]['snapshots'] && retained['solution_revisions'] == packet[:state]['solution_revisions']
puts JSON.generate(status: 'passed', checks: ['transactional_claim', 'immutable_snapshot', 'rollback', 'single_authority', 'issue_eviction_retains_history'])
'''

def run(args, **kwargs):
    result = subprocess.run(DOCKER + args, text=True, capture_output=True, timeout=60, **kwargs)
    if result.returncode:
        raise RuntimeError(result.stderr[-4000:])
    return result

def main():
    suffix = uuid.uuid4().hex[:12]
    network, database = 'esa-state-test-' + suffix, 'esa-state-db-' + suffix
    try:
        run(['network', 'create', '--internal', network])
        run(['run', '-d', '--name', database, '--network', network, '--network-alias', 'db', '--label', 'esa.test.owner=' + suffix,
             '-e', 'POSTGRES_DB=esa_test', '-e', 'POSTGRES_USER=esa_test', '-e', 'POSTGRES_PASSWORD=fixture-only', 'postgres:17-alpine'])
        for _ in range(40):
            result = subprocess.run(DOCKER + ['exec', database, 'pg_isready', '-U', 'esa_test', '-d', 'esa_test'], capture_output=True, timeout=5)
            if result.returncode == 0:
                break
            time.sleep(0.25)
        else:
            raise RuntimeError('Fixture database did not become ready')
        plugin = ROOT / 'plugins/redmine_esa'
        paths = [plugin / 'db/migrate/003_create_esa_state_records.rb', plugin / 'app/models/esa_state_record.rb', plugin / 'app/models/esa_backend_state.rb']
        sources = {str(path.relative_to(ROOT)): path.read_text() for path in paths}
        # Without database.yml, Redmine's Gemfile excludes adapter gems from the
        # bundle. Use the already installed gems directly in this model-only test.
        result = run(['run', '--rm', '-i', '--network', network, '--entrypoint', 'ruby', 'esa-redmine-local:7.0.2', '-e', RUBY], input=json.dumps(sources))
        print(result.stdout.strip())
    finally:
        subprocess.run(DOCKER + ['rm', '-f', '-v', database], capture_output=True, timeout=30)
        subprocess.run(DOCKER + ['network', 'rm', network], capture_output=True, timeout=30)

if __name__ == '__main__':
    main()
