require 'net/http'
require 'json'
require 'uri'

module Esa
  class Error < StandardError; end
  class Client
    def initialize(projects, issues = nil)
      @projects = projects.map { |p| p.id.to_s }
      @issues = issues
    end

    def request(method, path, body = nil, query = {})
      raise Error, 'No authorized ESA project is available.' if @projects.empty?
      uri = URI(ENV.fetch('ESA_URL', 'http://esa:8090') + path)
      uri.query = URI.encode_www_form(query) unless query.empty?
      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = uri.scheme == 'https'
      http.open_timeout = 3
      http.read_timeout = 20
      req = (method == :get ? Net::HTTP::Get : Net::HTTP::Post).new(uri)
      req['Authorization'] = 'Bearer ' + ENV.fetch('ESA_API_TOKEN')
      req['X-ESA-Project-IDs'] = @projects.join(',')
      req['X-ESA-Ticket-IDs'] = @issues.join(',') if @issues
      req['Content-Type'] = 'application/json'
      req.body = JSON.generate(body) if body
      response = http.request(req)
      result = JSON.parse(response.body)
      raise Error, result.fetch('error', "ESA returned HTTP #{response.code}") unless response.is_a?(Net::HTTPSuccess)
      result
    rescue KeyError, JSON::ParserError, IOError, SocketError, SystemCallError, Timeout::Error => e
      raise Error, "ESA service is unavailable: #{e.class.name}."
    end
  end
end
