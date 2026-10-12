class EsaEvent < ApplicationRecord
  def self.forward(issue, user, force: false)
    raise Esa::Error, 'ESA forwarding is not enabled for this project.' unless issue.project.module_enabled?(:esa)
    create!(issue_id: issue.id, payload: Esa::Payload.ticket(issue).to_json,
            forwarded_by: user.login, forwarded_at: Time.current, force: force)
  end
end
