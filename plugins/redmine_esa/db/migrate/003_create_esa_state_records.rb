class CreateEsaStateRecords < ActiveRecord::Migration[8.0]
  def change
    create_table :esa_backend_states do |t|
      t.string :name, null: false
      t.bigint :revision, null: false, default: 0
      t.text :payload, null: false
      t.timestamps
    end
    add_index :esa_backend_states, :name, unique: true
    create_table :esa_state_records do |t|
      t.string :kind, null: false
      t.string :record_key, null: false
      t.string :project_id
      t.text :payload, null: false
      t.timestamps
    end
    add_index :esa_state_records, [:kind, :record_key], unique: true
    add_index :esa_state_records, [:project_id, :kind]
  end
end
