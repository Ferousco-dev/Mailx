# mailx/sdk (PHP)

Official PHP SDK for MailX.

```bash
composer require mailx/sdk
```

## Usage

```php
use MailX\Client;

$client = new Client('your_api_key');

$email = $client->sendEmail([
    'from' => 'you@yourdomain.com',
    'to' => ['recipient@example.com'],
    'subject' => 'Hello from MailX',
    'html' => '<p>Hello!</p>',
]);

echo $email['id'];
```

## Retries

Requests that fail with `429` or `5xx` are retried automatically (default: 3 attempts),
honoring the `Retry-After` header when present, otherwise exponential backoff with jitter.

## Errors

Failed requests throw `MailX\MailXException` with `status`, `type`, `apiCode`, `requestId`,
and `retryAfter` properties.

## Coverage

`sendEmail`/`sendBatch`/`getEmail`/`listEmails` accept/return plain associative arrays
mirroring the OpenAPI schema field names exactly. All other resources (domains,
DKIM/SPF/DMARC/BIMI, templates, contacts, audiences, broadcasts, analytics, suppressions,
webhooks) are covered with one method per operation, same array-in/array-out shape — see
your server's `GET /openapi.json` for exact fields.

## Testing

```bash
composer install
vendor/bin/phpunit tests
```

Live contract tests are skipped unless `MAILX_SDK_TEST_BASE_URL` and
`MAILX_SDK_TEST_API_KEY` are set.
