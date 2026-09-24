# mailx-sdk

Official Python client for the MailX API. Targets the OpenAPI spec as of
`internal/api/openapi.go` at repo commit `c05c751` (v0.43).

```python
from mailx_sdk import MailXClient

client = MailXClient(api_key="mx_...", base_url="https://your-mailx-host/v1")

email = client.send_email(
    "you@yourdomain.com",
    ["someone@example.com"],
    subject="Hello",
    text="Hi there",
)
```

Retries automatically on 429/5xx (honoring `Retry-After`), up to
`max_retries` (default 3). Errors raise `MailXError` with `.status`,
`.type`, `.code`, `.request_id`.

Covers every resource in the OpenAPI spec (emails, domains, DKIM/SPF/
DMARC/BIMI, webhooks, templates, contacts, audiences, broadcasts,
analytics, suppressions, events) — see `mailx_sdk/client.py`.

## Test

```
pip install -e .
python3 tests/test_client.py
```

`tests/test_contract.py` runs against a real MailX server when
`MAILX_SDK_TEST_BASE_URL`/`MAILX_SDK_TEST_API_KEY` are set; otherwise it
skips.
