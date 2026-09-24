require "minitest/autorun"
require_relative "../lib/mailx"

class ClientTest < Minitest::Test
  def test_send_email_success
    calls = []
    fake = lambda do |uri, req|
      calls << [uri.path, req["Authorization"]]
      [200, {}, JSON.generate({ "id" => "em_1", "status" => "queued" })]
    end
    client = MailX::Client.new("test-key", http_client: fake)

    result = client.send_email({ "from" => "a@b.com", "to" => ["c@d.com"] })

    assert_equal "em_1", result["id"]
    assert_equal "/v1/emails", calls[0][0]
    assert_equal "Bearer test-key", calls[0][1]
  end

  def test_retries_on_429_then_succeeds
    responses = [
      [429, { "retry-after" => "0" }, JSON.generate({ "error" => { "type" => "rate_limited", "code" => "too_many_requests", "message" => "slow down" } })],
      [200, {}, JSON.generate({ "id" => "em_2" })]
    ]
    fake = lambda { |uri, req| responses.shift }
    client = MailX::Client.new("test-key", http_client: fake)

    result = client.send_email({ "from" => "a@b.com", "to" => ["c@d.com"] })

    assert_equal "em_2", result["id"]
  end

  def test_non_retryable_error_raises
    fake = lambda do |uri, req|
      [400, {}, JSON.generate({ "error" => { "type" => "invalid_request", "code" => "missing_field", "message" => "from is required" } })]
    end
    client = MailX::Client.new("test-key", http_client: fake)

    error = assert_raises(MailX::APIError) { client.send_email({}) }
    assert_equal 400, error.status
    assert_equal "missing_field", error.code
  end

  def test_get_email
    fake = lambda do |uri, req|
      assert_equal "/v1/emails/em_3", uri.path
      [200, {}, JSON.generate({ "id" => "em_3", "status" => "delivered" })]
    end
    client = MailX::Client.new("test-key", http_client: fake)

    result = client.get_email("em_3")

    assert_equal "delivered", result["status"]
  end
end
