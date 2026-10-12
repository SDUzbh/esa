#!/usr/bin/env ruby
# Verify that public journals do not multiply across metadata snapshot saves.
require 'json'
require 'time'
require 'ostruct'
require_relative '../plugins/redmine_esa/lib/esa/payload'

class EsaTicketSource
  class << self
    attr_accessor :record
    def find_by(**_criteria); record; end
  end
end
class PublicJournals
  attr_accessor :rows
  def initialize(rows); @rows = rows; end
  def where(*_args, **_options); self; end
  def not(**_criteria); self; end
  def map(&block); rows.map(&block); end
end
stamp = Time.utc(2020, 1, 1)
journal = OpenStruct.new(id: 10, notes: 'Current diagnostic evidence', created_on: stamp)
journals = PublicJournals.new([journal])
issue = OpenStruct.new(id: 1, project_id: 1, project: OpenStruct.new(name: 'Test'), subject: 'Symptom', description: 'Description', tracker: OpenStruct.new(name: 'Bug'), status: OpenStruct.new(name: 'New'), assigned_to: nil, journals: journals, created_on: stamp, updated_on: stamp)
def issue.closed?; false; end
def issue.is_private?; false; end
source = OpenStruct.new(snapshot: JSON.generate('comments' => [{ 'id' => 'imported:1', 'body' => 'Original evidence' }]), source_key: 'TEST-1', esa_upload_id: 1, updated_at: stamp)
EsaTicketSource.record = source
first = Esa::Payload.ticket(issue)
source.snapshot = JSON.generate(first)
second = Esa::Payload.ticket(issue)
raise 'Journal text was duplicated' unless second['comments'] == first['comments'] && second['comments'].length == 2
journals.rows = []
third = Esa::Payload.ticket(issue)
raise 'A withdrawn public journal survived in metadata' unless third['comments'].map { |comment| comment['id'] } == ['imported:1']
puts 'PASS: stable public journal snapshots'
