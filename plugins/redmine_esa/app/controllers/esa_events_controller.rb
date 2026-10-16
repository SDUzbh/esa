class EsaEventsController < ActionController::Base
  skip_forgery_protection
  before_action :authenticate_service

  def index
    events = EsaEvent.where(acknowledged_at: nil).where.not(forwarded_by: [nil, '']).where.not(forwarded_at: nil).order(:id).limit(20).map do |event|
      { id: "redmine_event_#{event.id}", origin: 'manual_forward', forwarded_by: event.forwarded_by, forwarded_at: event.forwarded_at.iso8601(6), force: event.force, occurred_at: event.created_at.iso8601(6), ticket: JSON.parse(event.payload) }
    end
    render json: { events: events }
  end

  # The first acknowledgement wins; a redelivered one never rewrites the outcome.
  def ack
    id, attributes = Esa::Acknowledgement.parse(params, Time.current)
    return head :bad_request unless id
    event = EsaEvent.find(id)
    event.update!(attributes) unless event.acknowledged_at
    render json: { acknowledged: true }
  end

  private

  def authenticate_service
    expected = ENV['ESA_API_TOKEN'].to_s
    provided = request.headers['Authorization'].to_s.delete_prefix('Bearer ')
    head :unauthorized unless expected.length >= 24 && request.headers['Authorization'].to_s.start_with?('Bearer ') && ActiveSupport::SecurityUtils.secure_compare(provided, expected)
  end
end
