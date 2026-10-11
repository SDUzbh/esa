module Esa
  module Payload
    def self.ticket(issue)
      source = EsaTicketSource.find_by(issue_id: issue.id)
      data = source ? JSON.parse(source.snapshot) : {}
      data.merge(
        'id' => "redmine:#{issue.id}", 'source_key' => source&.source_key || "REDMINE-#{issue.id}",
        'project_id' => issue.project_id.to_s, 'project' => issue.project.name,
        'subject' => issue.subject, 'description' => issue.description.to_s,
        'tracker' => issue.tracker.name, 'status' => issue.status.name,
        'assignee' => issue.assigned_to&.name.to_s, 'source' => source&.esa_upload_id ? 'manual_upload' : 'redmine',
        'components' => data.fetch('components', []), 'versions' => data.fetch('versions', []),
        'environment' => data.fetch('environment', {}), 'error_codes' => data.fetch('error_codes', []),
        'comments' => Array(data['comments']) + issue.journals.where(private_notes: false).where.not(notes: [nil, '']).map { |j|
          { 'id' => "redmine-journal:#{j.id}", 'body' => j.notes, 'created_at' => j.created_on.iso8601(6), 'updated_at' => j.created_on.iso8601(6) }
        },
        'created_at' => issue.created_on.iso8601(6), 'updated_at' => [issue.updated_on, source&.updated_at].compact.max.iso8601(6),
        'closed' => issue.closed?, 'private' => issue.is_private?, 'resolution' => data.fetch('resolution', { 'reason' => issue.closed? ? 'closed' : '', 'cause' => '', 'solution' => '', 'validation' => '' })
      )
    end
  end
end
