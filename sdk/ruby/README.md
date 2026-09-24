# mailx-sdk (Ruby)

Official Ruby SDK for MailX.

```ruby
gem "mailx-sdk"
```

## Usage

```ruby
require "mailx"

client = MailX::Client.new("your_api_key")

email = client.send_email(
  "from" => "you@yourdomain.com",
  "to" => ["recipient@example.com"],
  "subject" => "Hello from MailX",
  "html" => "<p>Hello!</p>"
)

puts email["id"]
```

## Retries

Requests that fail with `429` or `5xx` are retried automatically (default: 3 attempts),
honoring the `Retry-After` header when present, otherwise exponential backoff with jitter.

## Errors

Failed requests raise `MailX::APIError` with `status`, `type`, `code`, `request_id`, and
`retry_after` attributes.

## Coverage

`send_email`/`send_batch`/`get_email`/`list_emails` accept/return plain hashes mirroring the
OpenAPI schema field names exactly. All other resources (domains, DKIM/SPF/DMARC/BIMI,
templates, contacts, audiences, broadcasts, analytics, suppressions, webhooks) are covered
with one method per operation, same hash-in/hash-out shape — see your server's
`GET /openapi.json` for exact fields.

## Testing

```bash
ruby -Ilib -Ispec spec/client_test.rb
```

Live contract tests are skipped unless `MAILX_SDK_TEST_BASE_URL` and
`MAILX_SDK_TEST_API_KEY` are set.
