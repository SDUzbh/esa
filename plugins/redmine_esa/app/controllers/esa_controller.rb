class EsaController < ApplicationController
  before_action :require_login
  before_action :load_scope
  before_action :load_issue, only: [:show, :status, :report, :metadata_suggestion, :forward, :metadata, :review_case, :review_investigation, :review_membership, :classifier_predict, :classifier_feedback]
  rescue_from Esa::Error, with: :service_error

  def index
    @query = params.permit(:project_id, :tracker, :status, :assignee, :source, :esa_state, :route, :q, :sort, :direction, :page, :limit, :updated_from, :updated_to).to_h
    @result = @projects.empty? ? { 'tickets' => [], 'total_count' => 0, 'page' => 1, 'limit' => 25 } : @client.request(:get, '/v1/tickets', nil, @query)
  end

  def engineering_issues
    @result = @client.request(:get, '/v1/engineering-issues')
  end

  def show
    load_result
    @metadata = Esa::Payload.ticket(@issue)
  end

  def report
    load_result
    response.headers['Cache-Control'] = 'no-store'
  end

  def status
    load_result
    response.headers['Cache-Control'] = 'no-store'
    render json: { html: render_to_string(partial: 'status', formats: [:html]), active: @presentation.active? }
  end

  def metadata_suggestion
    return unless require_permission(:submit_esa)
    load_result
    response.headers['Cache-Control'] = 'no-store'
    render json: { values: @presentation.suggestion, source: @presentation.suggestion_source }
  end

  def upload
    @submit_projects = @projects.select { |p| User.current.allowed_to?(:submit_esa, p) }
  end

  def preview
    project = permitted_project(:submit_esa)
    file = params[:file]
    raise Esa::Error, 'Select a UTF-8 JSON or CSV file of at most 10 MB.' unless file && file.size <= 10.megabytes && %w[.json .csv].include?(File.extname(file.original_filename).downcase)
    original = file.read.force_encoding(Encoding::UTF_8)
    raise Esa::Error, 'The upload must contain valid UTF-8 text.' unless original.valid_encoding?
    preview = @client.request(:post, '/v1/import/preview', { filename: file.original_filename, project_id: project.id.to_s, content: original })
    @upload = EsaUpload.create!(user: User.current, project: project, filename: File.basename(file.original_filename), original_data: original, preview_data: preview.to_json)
    @preview = preview
    @existing = EsaTicketSource.where(project_id: project.id).pluck(:source_key, :issue_id).to_h
  end

  def import
    upload = EsaUpload.find_by!(id: params[:id], user_id: User.current.id)
    project = @projects.find { |p| p.id == upload.project_id }
    return deny_access unless project && User.current.allowed_to?(:submit_esa, project) && User.current.allowed_to?(:add_issues, project)
    count = 0
    skipped = 0
    EsaUpload.transaction do
      upload.lock!
      raise Esa::Error, 'This upload has already been imported.' if upload.imported_at
      JSON.parse(upload.preview_data).fetch('tickets').each do |ticket|
        key = ticket.fetch('source_key')
        if EsaTicketSource.exists?(project_id: project.id, source_key: key)
          skipped += 1
          next
        end
        tracker = project.trackers.find_by(name: ticket['tracker']) || project.trackers.find_by(name: 'Bug') || project.trackers.first
        raise Esa::Error, 'Enable an issue tracker for this project before importing.' unless tracker
        status = IssueStatus.find_by(name: ticket['status'])
        status = IssueStatus.where(is_closed: true).first if ticket['closed'] && !status&.is_closed?
        status ||= tracker.default_status
        raise Esa::Error, 'Configure a default issue status before importing.' unless status
        issue = Issue.new(project: project, tracker: tracker, author: User.current, subject: ticket.fetch('subject'), description: ticket['description'].to_s, status: status)
        issue.save!
        EsaTicketSource.create!(issue: issue, project_id: project.id, source_key: key, esa_upload: upload, snapshot: ticket.to_json)
        count += 1
      end
      upload.update!(imported_at: Time.current)
    end
    flash[:notice] = "Imported #{count} tickets; skipped #{skipped} existing source keys. Open each issue and choose Forward to ESA when engineering assistance is needed."
    redirect_to project_issues_path(project)
  rescue ActiveRecord::RecordInvalid, ActiveRecord::RecordNotUnique => e
    raise Esa::Error, "Import was rolled back: #{e.message}"
  end

  def forward
    return unless require_permission(:submit_esa)
    EsaEvent.forward(@issue, User.current, force: params[:force] == '1')
    flash[:notice] = 'Manual forwarding queued. ESA will receive this snapshot through the durable outbox.'
    redirect_to esa_ticket_path(@issue)
  end

  def classifier_predict
    return unless require_permission(:submit_esa)
    @client.request(:post, "/v1/tickets/redmine:#{@issue.id}/classifier/predict", {})
    flash[:notice] = 'Label suggestions generated for the last forwarded snapshot. Human confirmation is required.'
    redirect_to esa_ticket_path(@issue)
  end

  def classifier_feedback
    return unless require_permission(:review_esa)
    @client.request(:post, "/v1/tickets/redmine:#{@issue.id}/classifier/feedback", {
      id: params[:feedback_id].to_s, prediction_id: params[:prediction_id].to_s,
      labels: Array(params[:labels]).reject(&:blank?), reviewer: User.current.login, note: params[:feedback_note].to_s
    })
    flash[:notice] = 'Human labels recorded for future supervised training. Engineering routing is unchanged.'
    redirect_to esa_ticket_path(@issue)
  end

  def metadata
    return unless require_permission(:submit_esa)
    data = Esa::Payload.ticket(@issue)
    data['components'] = params[:components].to_s.split(';').map(&:strip).reject(&:blank?)
    data['versions'] = params[:versions].to_s.split(';').map(&:strip).reject(&:blank?)
    environment = JSON.parse(params[:environment].presence || '{}')
    raise Esa::Error, 'Environment must be a JSON object containing string values.' unless environment.is_a?(Hash) && environment.values.all? { |v| v.is_a?(String) }
    data['environment'] = environment
    data.delete('labels')
    data.delete('category_override')
    data['resolution'] = { 'reason' => params[:resolution_reason].to_s, 'cause' => params[:cause].to_s, 'solution' => params[:solution].to_s, 'validation' => params[:validation].to_s }
    EsaTicketSource.transaction do
      source = EsaTicketSource.find_or_initialize_by(issue_id: @issue.id)
      source.project_id = @issue.project_id
      source.source_key ||= "REDMINE-#{@issue.id}"
      source.snapshot = data.to_json
      source.save!
    end
    flash[:notice] = 'Metadata saved locally. Forward the issue to ESA to submit this snapshot.'
    redirect_to esa_ticket_path(@issue)
  rescue JSON::ParserError
    raise Esa::Error, 'Environment must be valid JSON.'
  end

  def review_case
    return unless require_permission(:review_esa)
    row = @client.request(:get, "/v1/tickets/redmine:#{@issue.id}").fetch('ticket')
    record = row.fetch('case') { raise Esa::Error, 'No collected case is available.' }
    @client.request(:post, "/v1/cases/#{record.fetch('id')}/review", { action: params[:review_action], reviewer: User.current.login, note: params[:note].to_s })
    flash[:notice] = 'Case review recorded. Applicable recommendations were refreshed without model calls.'
    redirect_to esa_ticket_path(@issue)
  end

  def review_investigation
    return unless require_permission(:review_esa)
    row = @client.request(:get, "/v1/tickets/redmine:#{@issue.id}").fetch('ticket')
    task = row.fetch('investigation') { raise Esa::Error, 'No shared investigation is available.' }
    @client.request(:post, "/v1/investigations/#{task.fetch('id')}/review", {
      expected_solution_id: params[:solution_id], expected_application_revision: params[:application_revision].to_i, action: params[:review_action],
      reviewer: User.current.login, note: params[:note].to_s,
      resolution: { cause: params[:cause].to_s, solution: params[:solution].to_s, validation: params[:validation].to_s }
    })
    flash[:notice] = 'A new solution revision was published for this investigation scope.'
    redirect_to esa_ticket_path(@issue)
  end

  def review_membership
    return unless require_permission(:review_esa)
    @client.request(:post, "/v1/tickets/redmine:#{@issue.id}/membership/review", {
      expected_snapshot_id: params[:snapshot_id], expected_membership_id: params[:membership_id].to_s, issue_id: params[:engineering_issue_id].to_s.strip,
      reviewer: User.current.login, reason: params[:membership_reason].to_s
    })
    flash[:notice] = 'Issue membership revised. Frozen investigation inputs and earlier solutions remain in history.'
    redirect_to esa_ticket_path(@issue)
  end

  private

  def load_result
    @result = begin
      @client.request(:get, "/v1/tickets/redmine:#{@issue.id}")
    rescue Esa::NotFound
      { 'ticket' => { 'ticket' => { 'closed' => @issue.closed? } }, 'history' => [] }
    end
    @row = @result.fetch('ticket')
    pending = EsaEvent.where(issue_id: @issue.id, acknowledged_at: nil).where.not(forwarded_at: nil).exists?
    @presentation = Esa::Presentation.new(@row, pending: pending)
  end

  def load_scope
    @projects = Project.visible(User.current).to_a.select { |p| p.module_enabled?(:esa) && User.current.allowed_to?(:view_esa, p) }
    # Issue visibility can be narrower than project visibility.
    visible = Issue.visible(User.current).where(project_id: @projects.map(&:id), is_private: false).pluck(:id).map { |id| "redmine:#{id}" }
    raise Esa::Error, 'This installation supports at most 2,000 visible ESA issues per request. Narrow the deployment or add database-backed authorization before scaling.' if visible.length > 2000
    @client = Esa::Client.new(@projects, visible.empty? ? ['none'] : visible)
  end

  def load_issue
    @issue = Issue.visible(User.current).find(params[:id])
    deny_access unless @projects.any? { |p| p.id == @issue.project_id } && !@issue.is_private?
  end

  def permitted_project(permission)
    project = @projects.find { |p| p.id.to_s == params[:project_id].to_s }
    raise Esa::Error, 'This action is not authorized for the selected project.' unless project && User.current.allowed_to?(permission, project)
    project
  end

  def require_permission(permission)
    return true if User.current.allowed_to?(permission, @issue.project)
    deny_access
    false
  end

  def service_error(error)
    @esa_error = error.message
    if request.format.json?
      render json: { error: @esa_error }, status: :unprocessable_entity
    else
      render :error, status: :unprocessable_entity
    end
  end
end
