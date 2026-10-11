# Configure English labels without changing administrator credentials.
Setting.default_language = 'en'
User.find_by!(login: 'admin').update!(language: 'en')
tracker_names = { "\u9519\u8bef" => 'Bug', "\u529f\u80fd" => 'Feature', "\u652f\u6301" => 'Support' }
status_names = { "\u65b0\u5efa" => 'New', "\u8fdb\u884c\u4e2d" => 'In Progress', "\u5df2\u89e3\u51b3" => 'Resolved', "\u53cd\u9988" => 'Feedback', "\u5df2\u5173\u95ed" => 'Closed', "\u5df2\u62d2\u7edd" => 'Rejected' }
priority_names = { "\u4f4e" => 'Low', "\u666e\u901a" => 'Normal', "\u9ad8" => 'High', "\u7d27\u6025" => 'Urgent', "\u7acb\u523b" => 'Immediate' }
tracker_names.each { |old, name| Tracker.where(name: old).update_all(name: name) }
status_names.each { |old, name| IssueStatus.where(name: old).update_all(name: name) }
priority_names.each { |old, name| IssuePriority.where(name: old).update_all(name: name) }
project = Project.find_or_initialize_by(identifier: 'esa-demo')
project.name = 'ESA Demo'
project.description = 'Synthetic tickets for validating ESA workflows. No customer data is imported automatically.'
project.is_public = false
project.save!
project.enabled_module_names = (project.enabled_module_names + %w[issue_tracking esa]).uniq
project.trackers = Tracker.all
puts({ plugin: Redmine::Plugin.find(:redmine_esa).version, project: project.identifier, trackers: Tracker.pluck(:name), statuses: IssueStatus.pluck(:name) }.to_json)
