# mailx-go

Official Go SDK for MailX.

```bash
go get github.com/Ferousco-dev/mailx-go
```

## Usage

```go
package main

import (
	"context"
	"log"

	mailx "github.com/Ferousco-dev/mailx-go"
)

func main() {
	client := mailx.NewClient("your_api_key")

	email, err := client.SendEmail(context.Background(), mailx.SendEmailRequest{
		From:    "you@yourdomain.com",
		To:      []string{"recipient@example.com"},
		Subject: "Hello from MailX",
		HTML:    "<p>Hello!</p>",
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Println(email.ID)
}
```

## Options

```go
client := mailx.NewClient("your_api_key",
	mailx.WithBaseURL("https://mail.yourdomain.com"),
	mailx.WithMaxRetries(5),
	mailx.WithHTTPClient(customHTTPClient),
)
```

## Retries

Requests that fail with `429` or `5xx` are retried automatically (default: 3 attempts),
honoring the `Retry-After` header when present, otherwise exponential backoff with jitter.

## Errors

Failed requests return a `*mailx.APIError` with `Status`, `Type`, `Code`, `Message`,
`RequestID`, and `RetryAfter` fields.

## Coverage

Emails and batch sending are fully typed (`SendEmailRequest`, `Email`, `EmailList`,
`BatchSendRequest`, `BatchSendResponse`). Domains, DKIM/SPF/DMARC/BIMI, templates,
contacts, audiences, broadcasts, analytics, suppressions, and webhooks are covered with
typed method signatures returning `mailx.JSON` (`map[string]any`) — see your server's
`GET /openapi.json` for exact response shapes.

## Testing

```bash
go test ./...
```

Live contract tests are skipped unless `MAILX_SDK_TEST_BASE_URL` and
`MAILX_SDK_TEST_API_KEY` are set.
