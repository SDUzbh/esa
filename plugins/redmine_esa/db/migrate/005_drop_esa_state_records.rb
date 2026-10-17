# ESA keeps its state in a JSON file or its own Postgres database; the Redmine
# state API that used these tables was removed. No installation stored state
# here. Rolling back recreates the empty tables of migration 003.
class DropEsaStateRecords < ActiveRecord::Migration[8.0]
  def up
    drop_table :esa_state_records, if_exists: true
    drop_table :esa_backend_states, if_exists: true
  end

  def down
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
