#!/usr/bin/env python3
"""Compile current ESA templates using the existing Redmine image, without a DB."""
import json
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]
PLUGIN = ROOT / 'plugins/redmine_esa'
RUBY = '''packet = JSON.parse(STDIN.read)
packet.fetch('templates').each do |name, source|
  generated = ActionView::Template::Handlers::ERB::Erubi.new(source).src
  RubyVM::InstructionSequence.compile(generated, name)
end
packet.fetch('ruby').each { |name, source| RubyVM::InstructionSequence.compile(source, name) }
puts JSON.generate({templates: packet.fetch('templates').length, ruby_files: packet.fetch('ruby').length, status: 'passed'})
'''

def main():
    packet = {
        'templates': {str(path.relative_to(ROOT)): path.read_text() for path in (PLUGIN / 'app/views/esa').glob('*.erb')},
        'ruby': {str(path.relative_to(ROOT)): path.read_text() for path in PLUGIN.rglob('*.rb')},
    }
    subprocess.run(['docker', '--context', 'colima-redmine', 'run', '--rm', '-i', '--network', 'none', '--read-only', '--tmpfs', '/tmp:rw,nosuid,size=32m', '--entrypoint', 'bundle', 'esa-redmine-local:7.0.2', 'exec', 'ruby', '-rjson', '-raction_view', '-e', RUBY], input=json.dumps(packet), text=True, check=True, timeout=60)

if __name__ == '__main__':
    main()
