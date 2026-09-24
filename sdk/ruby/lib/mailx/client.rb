require "net/http"
require "json"
require "uri"

require_relative "error"

module MailX
  class Client
    DEFAULT_BASE_URL = "https://api.mailx.dev"

    def initialize(api_key, base_url: DEFAULT_BASE_URL, max_retries: 3, http_client: nil)
      @api_key = api_key
      @base_url = base_url.chomp("/")
      @max_retries = max_retries
      @http_client = http_client
    end

    def send_email(body)
      request(:post, "/v1/emails", body)
    end

    def send_batch(body)
      request(:post, "/v1/emails/batch", body)
    end

    def get_email(id)
      request(:get, "/v1/emails/#{id}")
    end

    def list_emails(query = "")
      request(:get, "/v1/emails#{query}")
    end

    def list_events(query = "")
      request(:get, "/v1/events#{query}")
    end

    def create_domain(body); request(:post, "/v1/domains", body); end
    def get_domain(id); request(:get, "/v1/domains/#{id}"); end
    def list_domains; request(:get, "/v1/domains"); end
    def delete_domain(id); request(:delete, "/v1/domains/#{id}"); end
    def verify_dkim(domain_id); request(:post, "/v1/domains/#{domain_id}/dkim/verify"); end
    def get_spf(domain_id); request(:get, "/v1/domains/#{domain_id}/spf"); end
    def get_dmarc(domain_id); request(:get, "/v1/domains/#{domain_id}/dmarc"); end
    def get_bimi(domain_id); request(:get, "/v1/domains/#{domain_id}/bimi"); end
    def verify_bimi(domain_id); request(:post, "/v1/domains/#{domain_id}/bimi/verify"); end

    def create_template(body); request(:post, "/v1/templates", body); end
    def get_template(id); request(:get, "/v1/templates/#{id}"); end
    def list_templates; request(:get, "/v1/templates"); end
    def update_template(id, body); request(:patch, "/v1/templates/#{id}", body); end
    def delete_template(id); request(:delete, "/v1/templates/#{id}"); end

    def create_contact(body); request(:post, "/v1/contacts", body); end
    def get_contact(id); request(:get, "/v1/contacts/#{id}"); end
    def list_contacts; request(:get, "/v1/contacts"); end
    def update_contact(id, body); request(:patch, "/v1/contacts/#{id}", body); end
    def delete_contact(id); request(:delete, "/v1/contacts/#{id}"); end

    def create_audience(body); request(:post, "/v1/audiences", body); end
    def get_audience(id); request(:get, "/v1/audiences/#{id}"); end
    def list_audiences; request(:get, "/v1/audiences"); end
    def delete_audience(id); request(:delete, "/v1/audiences/#{id}"); end

    def create_broadcast(body); request(:post, "/v1/broadcasts", body); end
    def get_broadcast(id); request(:get, "/v1/broadcasts/#{id}"); end
    def list_broadcasts; request(:get, "/v1/broadcasts"); end

    def get_analytics(query = ""); request(:get, "/v1/analytics#{query}"); end

    def list_suppressions; request(:get, "/v1/suppressions"); end
    def create_suppression(body); request(:post, "/v1/suppressions", body); end
    def delete_suppression(id); request(:delete, "/v1/suppressions/#{id}"); end

    def create_webhook(body); request(:post, "/v1/webhooks", body); end
    def get_webhook(id); request(:get, "/v1/webhooks/#{id}"); end
    def list_webhooks; request(:get, "/v1/webhooks"); end
    def update_webhook(id, body); request(:patch, "/v1/webhooks/#{id}", body); end
    def delete_webhook(id); request(:delete, "/v1/webhooks/#{id}"); end

    private

    def request(method, path, body = nil)
      payload = body ? JSON.generate(body) : nil
      attempt = 0
      last_retry_after = nil

      loop do
        sleep(backoff_seconds(attempt, last_retry_after)) if attempt > 0

        uri = URI.join(@base_url, path)
        req = build_request(method, uri, payload)

        status, headers, resp_body = perform(uri, req)

        if status >= 200 && status < 300
          return resp_body.nil? || resp_body.empty? ? {} : JSON.parse(resp_body)
        end

        # Every MailX error response is {"error": {type, code, message,
        # request_id}} - see internal/api/errors.go's errorBody.
        body = resp_body && !resp_body.empty? ? (JSON.parse(resp_body) rescue {}) : {}
        parsed = body["error"] || {}
        retry_after = headers["retry-after"] ? headers["retry-after"].to_i : nil
        error = APIError.new(
          status: status,
          type: parsed["type"] || "",
          code: parsed["code"] || "",
          message: parsed["message"] || "request failed with status #{status}",
          request_id: parsed["request_id"],
          retry_after: retry_after
        )

        retryable = status == 429 || status >= 500
        if retryable && attempt < @max_retries
          attempt += 1
          last_retry_after = retry_after
          next
        end
        raise error
      end
    end

    def build_request(method, uri, payload)
      klass = { get: Net::HTTP::Get, post: Net::HTTP::Post, put: Net::HTTP::Put,
                patch: Net::HTTP::Patch, delete: Net::HTTP::Delete }.fetch(method)
      req = klass.new(uri)
      req["Authorization"] = "Bearer #{@api_key}"
      if payload
        req["Content-Type"] = "application/json"
        req.body = payload
      end
      req
    end

    def perform(uri, req)
      if @http_client
        return @http_client.call(uri, req)
      end
      res = Net::HTTP.start(uri.host, uri.port, use_ssl: uri.scheme == "https") do |http|
        http.request(req)
      end
      [res.code.to_i, res.each_header.to_h, res.body]
    end

    def backoff_seconds(attempt, retry_after)
      return retry_after.to_f if retry_after && retry_after > 0
      base_ms = [1000 * (2 ** attempt), 10000].min
      jitter_ms = rand(250)
      (base_ms + jitter_ms) / 1000.0
    end
  end
end
