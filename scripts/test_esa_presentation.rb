#!/usr/bin/env ruby
require 'minitest/autorun'
require_relative '../plugins/redmine_esa/lib/esa/presentation'

class EsaPresentationTest < Minitest::Test
  def row
    { 'ticket' => { 'closed' => false }, 'latest_run' => { 'state' => 'awaiting_review', 'report' => {
      'summary' => 'Metadata lookup failure', 'root_cause_candidates' => ['Missing disconnect check'],
      'recommendations' => ['Retry before parsing'], 'verification_steps' => ['Exercise disconnected responses']
    } }, 'solution' => { 'state' => 'proposed' } }
  end

  def test_pending_forward_hides_stale_report_and_suggestions
    view = Esa::Presentation.new(row, pending: true)
    assert_equal 'queued', view.state
    assert_equal 'forwarding', view.stage
    assert view.active?
    refute view.report_available?
    assert_empty view.suggestion
  end

  def test_unforwarded_and_active_states_do_not_fill_metadata
    assert_equal 'awaiting_forward', Esa::Presentation.new({}).state
    %w[queued running investigating awaiting_information failed returned_to_support].each do |state|
      input = row
      input['latest_run']['state'] = state
      assert_empty Esa::Presentation.new(input).suggestion
    end
  end

  def test_agent_draft_is_mapped_without_claiming_a_verified_repair
    input = row
    original = Marshal.dump(input)
    values = Esa::Presentation.new(input).suggestion
    assert_equal %w[cause resolution_reason solution validation], values.keys.sort
    assert_includes values['cause'], 'unverified'
    assert_includes values['solution'], 'not applied'
    assert_includes values['validation'], 'not performed'
    assert_includes values['validation'], 'Exercise disconnected responses'
    assert_equal original, Marshal.dump(input)
  end

  def test_reviewed_solution_overrides_raw_agent_advice
    input = row
    input['solution'] = { 'state' => 'trusted', 'resolution' => { 'cause' => 'Reviewed cause', 'solution' => 'Reviewed repair', 'validation' => 'Regression passed' } }
    view = Esa::Presentation.new(input)
    assert_equal 'Reviewed cause', view.suggestion['cause']
    assert_equal 'Regression passed', view.suggestion['validation']
    assert_equal 'Engineer-reviewed solution', view.suggestion_source
    input['latest_run']['state'] = 'failed'
    assert_empty Esa::Presentation.new(input).suggestion
  end

  def test_withdrawn_or_rejected_advice_is_not_available_for_filling
    %w[rejected withdrawn].each do |state|
      input = row
      input['solution']['state'] = state
      assert_empty Esa::Presentation.new(input).suggestion
      input = row
      input['investigation'] = { 'state' => state }
      assert_empty Esa::Presentation.new(input).suggestion
      input = row
      input['solution_application'] = { 'state' => state }
      assert_empty Esa::Presentation.new(input).suggestion
    end
  end

  def test_closed_case_and_missing_fields
    input = { 'ticket' => { 'closed' => true }, 'case' => { 'state' => 'trusted', 'resolution' => { 'reason' => 'Resolved', 'cause' => '', 'solution' => 'Known resolution' } } }
    view = Esa::Presentation.new(input)
    assert_equal 'trusted', view.state
    assert_equal({ 'resolution_reason' => 'Resolved', 'solution' => 'Known resolution' }, view.suggestion)
    assert_equal 'Engineer-reviewed solution', view.suggestion_source
  end
end
