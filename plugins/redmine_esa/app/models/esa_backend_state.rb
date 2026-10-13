class EsaBackendState < ApplicationRecord
  ENTITY_KEYS = %w[snapshots engineering_issues issue_ticket_links investigations investigation_groups solution_revisions].freeze
  IMMUTABLE_KEYS = %w[snapshots solution_revisions].freeze
  CORE_KEYS = %w[tickets events receipts runs latest cases].freeze

  def self.authority
    create_or_find_by!(name: 'primary') do |state|
      state.payload = ({ 'schema_version' => 1 }.merge(CORE_KEYS.to_h { |key| [key, {}] })).to_json
    end
  end

  # The state row is the transaction lock and CAS fence. Shared investigation entities are stored
  # separately; the core state frame never contains a second writable copy.
  def packet
    state = JSON.parse(payload)
    EsaStateRecord.order(:kind, :record_key).each do |record|
      (state[record.kind] ||= {})[record.record_key] = JSON.parse(record.payload)
    end
    { state: state, revision: revision }
  end

  def commit!(input, expected)
    raise ArgumentError, 'Invalid state schema' unless input.is_a?(Hash) && input['schema_version'] == 1 && CORE_KEYS.all? { |key| input[key].is_a?(Hash) }
    raise ArgumentError, 'Invalid entity map' unless ENTITY_KEYS.all? { |key| !input.key?(key) || input[key].is_a?(Hash) }
    self.class.transaction do
      lock!
      return false unless revision == expected
      existing = EsaStateRecord.all.index_by { |record| [record.kind, record.record_key] }
      ENTITY_KEYS.each do |kind|
        incoming = input.fetch(kind, {})
        incoming.each do |key, value|
          raise ArgumentError, 'Invalid record identity' unless value.is_a?(Hash) && value['id'] == key
          record = existing.delete([kind, key])
          if record && IMMUTABLE_KEYS.include?(kind) && JSON.parse(record.payload) != value
            raise ArgumentError, 'Immutable record changed'
          end
          encoded = value.to_json
          next if record && JSON.parse(record.payload) == value
          record ||= EsaStateRecord.new(kind: kind, record_key: key)
          record.project_id = value['project_id'] || value.dig('ticket', 'project_id')
          record.payload = encoded
          record.save!
        end
      end
      raise ArgumentError, 'Immutable record removed' if existing.values.any? { |record| IMMUTABLE_KEYS.include?(record.kind) }
      existing.each_value(&:destroy!)
      update!(payload: input.except(*ENTITY_KEYS).to_json, revision: revision + 1)
    end
    true
  end
end
