require 'redmine'
require_relative 'lib/esa/client'
require_relative 'lib/esa/payload'
require_relative 'lib/esa/hooks'

Redmine::Plugin.register :redmine_esa do
  name 'ESA'
  author 'ESA Project'
  description 'Read-only engineering support workflows powered by Go.'
  version '0.1.0'
  requires_redmine version_or_higher: '7.0.0'
  project_module :esa do
    permission :view_esa, { esa: [:index, :show] }, read: true
    permission :submit_esa, { esa: [:upload, :preview, :import, :analyze, :metadata] }
    permission :review_esa, { esa: [:review_case, :review_run] }
  end
  menu :top_menu, :esa, { controller: 'esa', action: 'index' }, caption: 'ESA', if: Proc.new { User.current.logged? }
end
