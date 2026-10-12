module Esa
  class Hooks < Redmine::Hook::ViewListener
    def view_issues_show_details_bottom(context = {})
      issue = context[:issue]
      return '' unless issue && User.current.allowed_to?(:view_esa, issue.project)
      context[:controller].view_context.link_to('Forward to ESA', { controller: 'esa', action: 'show', id: issue.id }, class: 'icon icon-help')
    end
  end
end
