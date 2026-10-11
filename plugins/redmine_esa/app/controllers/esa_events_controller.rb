class EsaEventsController < ActionController::Base
  skip_forgery_protection
  before_action :authenticate_service

  def index
    events = EsaEvent.where(acknowledged_at: nil).order(:id).limit(20).map do |event|
      { id: "redmine_event_#{event.id}", origin: 'redmine', occurred_at: event.created_at.iso8601(6), ticket: JSON.parse(event.payload) }
    end
    render json: { events: events }
  end

  def ack
    value = params[:event_id].to_s
    return head :bad_request unless value.match?(/\Aredmine_event_\d+\z/)
    event = EsaEvent.find(value.delete_prefix('redmine_event_'))
    event.update!(acknowledged_at: Time.current) unless event.acknowledged_at
    render json: { acknowledged: true }
  end

  private

  def authenticate_service
    expected = ENV['ESA_API_TOKEN'].to_s
    provided = request.headers['Authorization'].to_s.delete_prefix('Bearer ')
    head :unauthorized unless expected.length >= 24 && request.headers['Authorization'].to_s.start_with?('Bearer ') && ActiveSupport::SecurityUtils.secure_compare(provided, expected)
  end
end
