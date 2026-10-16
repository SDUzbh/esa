module Esa
  # Validates an outbox acknowledgement from Go. A bare {event_id} still means accepted.
  module Acknowledgement
    EVENT_ID = /\Aredmine_event_(\d+)\z/
    REASON_LIMIT = 1000

    # Returns [event record id, attributes for the first acknowledgement], or nil when invalid.
    def self.parse(params, now)
      match = EVENT_ID.match(params['event_id'].to_s)
      return nil unless match
      attributes = { acknowledged_at: now }
      case params['status']
      when nil, 'accepted'
      when 'rejected'
        reason = params['reason']
        return nil unless reason.is_a?(String) && !reason.strip.empty?
        attributes.merge!(rejected_at: now, rejection_reason: reason.strip[0, REASON_LIMIT])
      else
        return nil
      end
      [match[1].to_i, attributes]
    end
  end
end
