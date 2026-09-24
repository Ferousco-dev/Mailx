# @mailx/sdk

Official Node/TypeScript client for the MailX API. Targets the OpenAPI spec
as of `internal/api/openapi.go` at repo commit `c05c751` (v0.43).

```ts
import { MailXClient } from "@mailx/sdk";

const client = new MailXClient({ apiKey: process.env.MAILX_API_KEY!, baseUrl: "https://your-mailx-host/v1" });

const email = await client.sendEmail({
  from: "you@yourdomain.com",
  to: ["someone@example.com"],
  subject: "Hello",
  text: "Hi there",
});
```

Retries automatically on 429/5xx (honoring `Retry-After`), up to `maxRetries`
(default 3). Errors are thrown as `MailXError` with `.status`, `.type`,
`.code`, `.requestId`.

`sendEmail`/`sendBatch`/`getEmail`/`listEmails` are fully typed. Every other
resource (domains, DKIM/SPF/DMARC/BIMI, webhooks, templates, contacts,
audiences, broadcasts, analytics, suppressions, events) has a typed method
signature with a loosely-typed JSON body/result — see `GET /openapi.json`
on your server for exact shapes.

## React Email templates

No special integration needed: render your `.tsx` template with
[`@react-email/render`](https://react.email) yourself, then pass the
result straight into `sendEmail`'s `html`/`text` fields — the exact shape
`POST /v1/emails` already accepts.

```ts
import { render } from "@react-email/render";
import MyTemplate from "./emails/my-template";

const html = await render(MyTemplate({ name: "Ada" }));
await client.sendEmail({ from: "you@yourdomain.com", to: ["a@example.com"], subject: "Hi", html });
```

## Build & test

```
npm install
npm run build
npm test
```

`test/contract.test.js` runs against a real MailX server when
`MAILX_SDK_TEST_BASE_URL`/`MAILX_SDK_TEST_API_KEY` are set; otherwise it
skips.
