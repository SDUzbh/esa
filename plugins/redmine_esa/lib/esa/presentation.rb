module Esa
  # Small read-only UI projection; filling a form never changes workflow state.
  class Presentation
    ACTIVE = %w[queued running investigating].freeze

    # rejection describes the issue's newest acknowledged forward when ESA rejected it.
    def initialize(row, pending: false, rejection: nil)
      @row, @pending, @rejection = row, pending, rejection
    end

    # A newer forward still in the outbox supersedes an earlier rejection.
    def rejection
      @pending ? nil : @rejection
    end

    def state
      return 'queued' if @pending
      record = @row.dig('ticket', 'closed') ? @row['case'] : @row['latest_run']
      record&.fetch('state', nil) || 'awaiting_forward'
    end

    def active?
      ACTIVE.include?(state)
    end

    def stage
      return 'forwarding' if @pending
      active? ? @row.dig('investigation', 'phase') : nil
    end

    def report_available?
      !@pending && !active? && !!(@row['latest_run'] || @row['case'])
    end

    REVOKED = {
      %w[solution rejected] => 'rejected the agent proposal.', %w[solution withdrawn] => 'withdrew the reviewed solution.',
      %w[applicability rejected] => 'rejected applying the existing solution.', %w[applicability withdrawn] => "withdrew the existing solution's applicability."
    }.freeze

    REVIEW_NOTICES = {
      ['solution', 'approve'] => 'Reviewed solution published for this investigation scope.',
      ['solution', 'reject'] => 'Agent proposal rejected. Support can see your review note; forward a new analysis request to investigate again.',
      ['solution', 'withdraw'] => 'Solution withdrawn. Support can see your review note; earlier revisions remain in history.',
      ['applicability', 'approve'] => 'Applicability accepted. The existing solution now applies to this ticket.',
      ['applicability', 'reject'] => 'Applicability rejected. Support can see your review note; the existing solution is unchanged.',
      ['applicability', 'withdraw'] => 'Applicability withdrawn. Support can see your review note; the existing solution is unchanged.'
    }.freeze

    # The notice after an engineer's solution review, worded for the action taken.
    def self.review_notice(action, application:)
      REVIEW_NOTICES.fetch([application ? 'applicability' : 'solution', action.to_s], 'Review recorded.')
    end

    # Why the shared proposal or its applicability is no longer offered, so
    # support knows what the engineer asked for. Nil while nothing was revoked.
    def review_outcome
      application = @row['solution_application']
      if application && %w[rejected withdrawn].include?(application['state'])
        review = Array(application['reviews']).reverse.find { |r| r['state'] == application['state'] } || {}
        subject, state, reviewer, note = 'applicability', application['state'], review['reviewer'], review['note']
      else
        solution = @row['solution']
        return nil unless solution && %w[rejected withdrawn].include?(solution['state'])
        subject, state, reviewer, note = 'solution', solution['state'], solution['reviewer'], solution['note']
      end
      # Automatic withdrawals (a source became private or changed) have no engineer.
      by = ['', 'system'].include?(reviewer.to_s) ? 'ESA' : reviewer
      { 'summary' => "#{by} #{REVOKED[[subject, state]]}", 'note' => note.to_s.strip }
    end

    def suggestion_source
      @row.dig('solution', 'state') == 'trusted' || @row.dig('case', 'state') == 'trusted' ? 'Engineer-reviewed solution' : 'ESA draft — not verified'
    end

    def suggestion
      return {} unless report_available?
      return {} unless %w[awaiting_review completed draft trusted].include?(state)
      return {} if %w[rejected withdrawn invalidated archived failed].include?(@row.dig('investigation', 'state'))
      return {} if %w[rejected withdrawn].include?(@row.dig('solution_application', 'state'))
      report = @row.dig('latest_run', 'report') || {}
      resolution = if @row.dig('solution', 'state') == 'trusted'
                     @row.dig('solution', 'resolution') || {}
                   elsif %w[draft trusted].include?(@row.dig('case', 'state'))
                     @row.dig('case', 'resolution') || {}
                   else
                     return {} unless %w[awaiting_review completed].include?(state)
                     return {} if %w[rejected withdrawn].include?(@row.dig('solution', 'state'))
                     values = { 'reason' => report['summary'].to_s }
                     { 'cause' => ['root_cause_candidates', 'Agent-proposed cause (unverified):'],
                       'solution' => ['recommendations', 'Proposed solution (not applied):'],
                       'validation' => ['verification_steps', 'Proposed verification (not performed):'] }.each do |field, (section, prefix)|
                       text = Array(report[section]).map(&:to_s).reject { |s| s.strip.empty? }.join("\n\n")
                       values[field] = "#{prefix}\n#{text}" unless text.empty?
                     end
                     values
                   end
      { 'resolution_reason' => resolution['reason'], 'cause' => resolution['cause'],
        'solution' => resolution['solution'], 'validation' => resolution['validation'] }
        .select { |_, value| value.is_a?(String) && !value.strip.empty? }
    end
  end
end
