#!/usr/bin/env ruby
require 'minitest/autorun'
require_relative '../plugins/redmine_esa/lib/esa/acknowledgement'

class EsaAcknowledgementTest < Minitest::Test
  NOW = Time.utc(2026, 10, 16, 12)

  def parse(params)
    Esa::Acknowledgement.parse(params, NOW)
  end

  def test_event_id_only_body_is_still_accepted
    assert_equal [42, { acknowledged_at: NOW }], parse('event_id' => 'redmine_event_42')
  end

  def test_explicit_accepted_status_records_no_rejection
    assert_equal [42, { acknowledged_at: NOW }], parse('event_id' => 'redmine_event_42', 'status' => 'accepted')
  end

  def test_rejected_status_records_time_and_reason
    id, attributes = parse('event_id' => 'redmine_event_7', 'status' => 'rejected',
                           'reason' => "  conflict: ticket belongs to a different project \n")
    assert_equal 7, id
    assert_equal({ acknowledged_at: NOW, rejected_at: NOW, rejection_reason: 'conflict: ticket belongs to a different project' }, attributes)
  end

  def test_rejection_reason_is_capped
    _, attributes = parse('event_id' => 'redmine_event_7', 'status' => 'rejected', 'reason' => 'x' * 5000)
    assert_equal Esa::Acknowledgement::REASON_LIMIT, attributes[:rejection_reason].length
  end

  def test_invalid_bodies_are_refused
    [
      {},
      { 'event_id' => 'event_7' },
      { 'event_id' => "redmine_event_7\nredmine_event_8" },
      { 'event_id' => 'redmine_event_7', 'status' => 'ignored' },
      { 'event_id' => 'redmine_event_7', 'status' => '' },
      { 'event_id' => 'redmine_event_7', 'status' => 'rejected' },
      { 'event_id' => 'redmine_event_7', 'status' => 'rejected', 'reason' => '   ' },
      { 'event_id' => 'redmine_event_7', 'status' => 'rejected', 'reason' => ['conflict'] }
    ].each { |params| assert_nil parse(params), params.inspect }
  end
end
