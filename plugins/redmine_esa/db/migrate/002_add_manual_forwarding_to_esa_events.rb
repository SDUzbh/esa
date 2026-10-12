class AddManualForwardingToEsaEvents < ActiveRecord::Migration[8.0]
  def change
    add_column :esa_events, :forwarded_by, :string
    add_column :esa_events, :forwarded_at, :datetime
    add_column :esa_events, :force, :boolean, default: false, null: false
  end
end
