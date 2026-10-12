module EsaHelper
  def esa_label(value)
    value.to_s.tr('_', ' ').split.map(&:capitalize).join(' ')
  end

  def esa_sort_link(label, column)
    direction = @query['sort'] == column && @query['direction'] == 'asc' ? 'desc' : 'asc'
    link_to label, esa_path(@query.merge('sort' => column, 'direction' => direction, 'page' => 1))
  end

  def esa_row_state(row)
    record = row.dig('ticket', 'closed') ? row['case'] : row['latest_run']
    record&.fetch('state', nil) || 'awaiting_forward'
  end
end
