require "minitest/autorun"
require_relative "../lib/mailx"

class ContractTest < Minitest::Test
  def test_live_send_email
    base_url = ENV["MAILX_SDK_TEST_BASE_URL"]
    api_key = ENV["MAILX_SDK_TEST_API_KEY"]
    skip "MAILX_SDK_TEST_BASE_URL/MAILX_SDK_TEST_API_KEY not set" unless base_url && api_key

    client = MailX::Client.new(api_key, base_url: base_url)
    result = client.send_email({
      "from" => "sdk-test@example.com",
      "to" => ["sdk-test-dest@example.com"],
      "subject" => "Ruby SDK contract test",
      "text" => "hello"
    })

    assert result["id"]
  end
end
