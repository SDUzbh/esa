class EsaEvent < ApplicationRecord
  def self.enqueue(issue)
    return unless issue.project.module_enabled?(:esa)
    create!(issue_id: issue.id, payload: Esa::Payload.ticket(issue).to_json)
  end
end
