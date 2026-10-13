class EsaStateController < ActionController::Base
  skip_forgery_protection
  before_action :authenticate_service

  def show
    state = EsaBackendState.authority
    # Serialize reads with commits so the frame and entity rows have one revision.
    state.with_lock { render json: state.packet }
  end

  def update
    return head :payload_too_large if request.raw_post.bytesize > 64.megabytes
    input = JSON.parse(request.raw_post)
    expected = input.fetch('expected_revision')
    return head :bad_request unless expected.is_a?(Integer) && expected >= 0
    state = EsaBackendState.authority
    return head :conflict unless state.commit!(input.fetch('state'), expected)
    # Do not return a newer state's revision after another writer commits.
    render json: { revision: expected + 1 }
  rescue JSON::ParserError, KeyError, ArgumentError
    render json: { error: 'Invalid state transaction' }, status: :unprocessable_entity
  end

  private

  def authenticate_service
    return head :not_found unless ENV['ESA_STATE_API_ENABLED'] == '1'
    expected = ENV['ESA_API_TOKEN'].to_s
    provided = request.headers['Authorization'].to_s.delete_prefix('Bearer ')
    head :unauthorized unless expected.length >= 24 && request.headers['Authorization'].to_s.start_with?('Bearer ') && ActiveSupport::SecurityUtils.secure_compare(provided, expected)
  end
end
