class CreateEsaTables < ActiveRecord::Migration[8.0]
  def change
    create_table :esa_events do |t|
      t.integer :issue_id, null: false
      t.text :payload, null: false
      t.datetime :acknowledged_at
      t.timestamps
    end
    add_index :esa_events, [:acknowledged_at, :id]
    create_table :esa_uploads do |t|
      t.integer :user_id, null: false
      t.integer :project_id, null: false
      t.string :filename, null: false
      t.text :original_data, null: false
      t.text :preview_data, null: false
      t.datetime :imported_at
      t.timestamps
    end
    create_table :esa_ticket_sources do |t|
      t.integer :issue_id, null: false
      t.integer :project_id, null: false
      t.integer :esa_upload_id
      t.string :source_key, null: false
      t.text :snapshot, null: false
      t.timestamps
    end
    add_index :esa_ticket_sources, :issue_id, unique: true
    add_index :esa_ticket_sources, [:project_id, :source_key], unique: true
  end
end
