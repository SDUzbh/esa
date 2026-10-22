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

  def test_rejected_forward_is_shown_until_a_newer_forward_is_pending
    rejection = { 'forwarded_by' => 'support-user', 'reason' => 'conflict: ticket belongs to a different project' }
    assert_equal rejection, Esa::Presentation.new({}, rejection: rejection).rejection
    assert_equal 'awaiting_forward', Esa::Presentation.new({}, rejection: rejection).state
    assert_nil Esa::Presentation.new(row, pending: true, rejection: rejection).rejection
    assert_nil Esa::Presentation.new(row).rejection
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

  def test_revoked_proposal_shows_the_review_note
    input = row
    assert_nil Esa::Presentation.new(input).review_outcome
    input['solution'] = { 'state' => 'rejected', 'reviewer' => 'engineer-1', 'note' => ' Need the JVM and map-count limits. ' }
    assert_equal({ 'summary' => 'engineer-1 rejected the agent proposal.', 'note' => 'Need the JVM and map-count limits.' },
                 Esa::Presentation.new(input).review_outcome)
    input['solution'] = { 'state' => 'withdrawn', 'reviewer' => '', 'note' => 'The source ticket became private' }
    assert_equal 'ESA withdrew the reviewed solution.', Esa::Presentation.new(input).review_outcome['summary']
    input['solution'] = { 'state' => 'trusted', 'reviewer' => 'engineer-1', 'note' => 'ok' }
    assert_nil Esa::Presentation.new(input).review_outcome
  end

  def test_revoked_applicability_uses_its_latest_matching_review
    input = row
    input['solution'] = { 'state' => 'trusted', 'reviewer' => 'engineer-1', 'note' => 'Original review' }
    input['solution_application'] = { 'state' => 'rejected', 'reviews' => [
      { 'state' => 'rejected', 'reviewer' => 'engineer-1', 'note' => 'First' },
      { 'state' => 'accepted', 'reviewer' => 'engineer-1', 'note' => 'Accepted' },
      { 'state' => 'rejected', 'reviewer' => 'engineer-2', 'note' => 'Different stack' }
    ] }
    assert_equal({ 'summary' => 'engineer-2 rejected applying the existing solution.', 'note' => 'Different stack' },
                 Esa::Presentation.new(input).review_outcome)
    input['solution_application'] = { 'state' => 'withdrawn', 'reviews' => [{ 'state' => 'withdrawn', 'reviewer' => 'system', 'note' => 'The referenced source changed or was withdrawn' }] }
    assert_equal "ESA withdrew the existing solution's applicability.", Esa::Presentation.new(input).review_outcome['summary']
    input['solution_application'] = { 'state' => 'accepted', 'reviews' => [] }
    assert_nil Esa::Presentation.new(input).review_outcome
  end

  def test_review_notice_matches_the_action
    assert_match(/published/, Esa::Presentation.review_notice('approve', application: false))
    assert_match(/\AAgent proposal rejected/, Esa::Presentation.review_notice('reject', application: false))
    assert_match(/\ASolution withdrawn/, Esa::Presentation.review_notice('withdraw', application: false))
    assert_match(/\AApplicability accepted/, Esa::Presentation.review_notice('approve', application: true))
    assert_match(/\AApplicability rejected/, Esa::Presentation.review_notice('reject', application: true))
    assert_match(/\AApplicability withdrawn/, Esa::Presentation.review_notice('withdraw', application: true))
    %w[reject withdraw].each { |action| refute_match(/published/, Esa::Presentation.review_notice(action, application: false)) }
  end

  def test_closed_case_and_missing_fields
    input = { 'ticket' => { 'closed' => true }, 'case' => { 'state' => 'trusted', 'resolution' => { 'reason' => 'Resolved', 'cause' => '', 'solution' => 'Known resolution' } } }
    view = Esa::Presentation.new(input)
    assert_equal 'trusted', view.state
    assert_equal({ 'resolution_reason' => 'Resolved', 'solution' => 'Known resolution' }, view.suggestion)
    assert_equal 'Engineer-reviewed solution', view.suggestion_source
  end
end
