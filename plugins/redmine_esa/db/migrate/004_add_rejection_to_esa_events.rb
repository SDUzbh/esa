class AddRejectionToEsaEvents < ActiveRecord::Migration[8.0]
  def change
    add_column :esa_events, :rejected_at, :datetime
    add_column :esa_events, :rejection_reason, :text
  end
end
