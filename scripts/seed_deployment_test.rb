# Initialize only the fresh, isolated deployment-acceptance database.
require 'json'
raise 'Not a deployment fixture' unless ENV['ESA_DEPLOYMENT_TEST'] == '1'
raise 'Refusing to seed an existing installation' if Project.exists? || User.where(login: 'deployment_support').exists?
%w[esa_events esa_ticket_sources].each do |table|
  raise "Plugin migration missing: #{table}" unless ActiveRecord::Base.connection.table_exists?(table)
end
raise 'Plugin migration 005 missing: esa_state_records still exists' if ActiveRecord::Base.connection.table_exists?('esa_state_records')
Redmine::DefaultData::Loader.load('en') if Redmine::DefaultData::Loader.no_data?
Setting.default_language = 'en'
project = Project.create!(name: 'Deployment acceptance', identifier: 'deployment-check', is_public: false)
project.enabled_module_names = %w[issue_tracking esa]
project.trackers = [Tracker.first!]
support = Role.create!(name: 'Deployment support', permissions: %i[view_issues add_issues view_esa submit_esa])
engineer = Role.create!(name: 'Deployment engineer', permissions: %i[view_issues view_esa review_esa])
password = ENV.fetch('ESA_TEST_PASSWORD')
%w[support engineer outsider].each do |name|
  user = User.create!(login: "deployment_#{name}", firstname: 'Deployment', lastname: name,
                      mail: "#{name}@example.test", language: 'en', status: User::STATUS_ACTIVE,
                      password: password, password_confirmation: password, must_change_passwd: false)
  role = { 'support' => support, 'engineer' => engineer }[name]
  Member.create!(project: project, principal: user, roles: [role]) if role
end
puts 'ESA_TEST_SEED=' + JSON.generate(project_id: project.id, tracker_id: Tracker.first!.id,
                                    status_id: Tracker.first!.default_status.id,
                                    priority_id: IssuePriority.default.id)
