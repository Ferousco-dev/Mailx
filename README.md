<div align="center">
  <img src="./assets/mailx.png" alt="MailX - developer-first email infrastructure built in Go" width="100%">

  <br>

  <a href="https://github.com/Ferousco-dev/Mailx/actions/workflows/go-ci.yml"><img src="https://github.com/Ferousco-dev/Mailx/actions/workflows/go-ci.yml/badge.svg" alt="Go CI status"></a>
  <a href="https://github.com/Ferousco-dev/Mailx/actions/workflows/security.yml"><img src="https://github.com/Ferousco-dev/Mailx/actions/workflows/security.yml/badge.svg" alt="Security workflow status"></a>
  <a href="./go.mod"><img src="https://img.shields.io/github/go-mod/go-version/Ferousco-dev/Mailx?style=flat-square&amp;color=00ADD8" alt="Go version"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/github/license/Ferousco-dev/Mailx?style=flat-square&amp;color=635BFF" alt="MIT license"></a>
  <a href="#project-status"><img src="https://img.shields.io/badge/status-active_development-F59E0B?style=flat-square" alt="Active development"></a>

  <p>
    <a href="#overview">Overview</a> ·
    <a href="#quick-start">Quick start</a> ·
    <a href="#api-example">API</a> ·
    <a href="#architecture">Architecture</a> ·
    <a href="#development">Development</a>
  </p>
</div>

## Overview

MailX is open-source, developer-first email infrastructure built from the protocol up in Go. It accepts mail over SMTP or a tenant-scoped REST API, stores the source message, processes delivery through a durable PostgreSQL and Redis pipeline, and exposes lifecycle events through signed webhooks.

The project is intentionally built as understandable infrastructure rather than a wrapper around a third-party email provider. Its design keeps durable lifecycle state in PostgreSQL, raw message data in local storage, and transient delivery scheduling in Redis.

> [!WARNING]
> MailX is under active development and is not production-ready. In particular, inbound and outbound STARTTLS/TLS are planned for v0.24 and are not currently implemented. Do not expose the current SMTP or HTTP listeners to untrusted networks.

## Capabilities

| | Capability | What MailX provides |
| --- | --- | --- |
| <img src="./assets/readme/icon-smtp.svg" width="28" alt=""> | SMTP engine | Bounded command and message handling, CRLF validation, dot-stuffing, recipient and connection limits, enhanced status codes, and graceful shutdown. |
| <img src="./assets/readme/icon-message.svg" width="28" alt=""> | MIME and storage | Plain text, HTML, nested multipart parsing, transfer decoding, attachment extraction, raw `.eml` preservation, and local message inspection. |
| <img src="./assets/readme/icon-api.svg" width="28" alt=""> | Developer API | Tenant-scoped email, domain, event, and webhook resources with API-key scopes, pagination, request IDs, OpenAPI documentation, and HTTP idempotency. |
| <img src="./assets/readme/icon-queue.svg" width="28" alt=""> | Durable processing | PostgreSQL outbox state, a Redis-backed leased queue, bounded workers, persistent delivery attempts, retries, and crash recovery. |
| <img src="./assets/readme/icon-delivery.svg" width="28" alt=""> | Mail delivery | DNS MX resolution, direct SMTP transfer, retry classification and scheduling, terminal outcomes, and safe bounce/DSN generation. |
| <img src="./assets/readme/icon-webhook.svg" width="28" alt=""> | Events and webhooks | Immutable lifecycle events, durable fan-out, HMAC-SHA256 signatures, retries, secret rotation, tenant isolation, and delivery inspection. |

## Quick start

### Requirements

- Docker Engine with Docker Compose v2
- `curl` for the API example
- Go 1.27.1 only when building or testing outside Docker

### Run the complete local stack

1. Clone the repository and enter it.

   ```bash
   git clone https://github.com/Ferousco-dev/Mailx.git
   cd Mailx
   ```

2. Create a local environment file.

   ```bash
   cp .env.example .env
   ```

   The checked-in defaults are for local development only. Never reuse them in a real deployment.

3. Build and start MailX, PostgreSQL, Redis, and the database migration job.

   ```bash
   docker compose up --build -d
   docker compose ps
   ```

4. Confirm the API is available.

   ```bash
   curl --fail http://localhost:8080/health/live
   curl --fail http://localhost:8080/health/ready
   ```

The local services are available at:

| Service | Address |
| --- | --- |
| REST API | `http://localhost:8080` |
| Interactive API documentation | `http://localhost:8080/docs` |
| OpenAPI contract | `http://localhost:8080/openapi.json` |
| SMTP receiver | `localhost:2525` |
| PostgreSQL | `localhost:5432` |
| Redis | `localhost:6379` |

View logs or stop the stack with:

```bash
docker compose logs -f mailx
docker compose down
```

Use `docker compose down -v` only when you intentionally want to delete all local MailX, PostgreSQL, and Redis data.

### Small hosts (about 1 GB RAM)

An opt-in low-memory PostgreSQL profile is available for small deployments. It is not the default and it keeps PostgreSQL's durability settings on:

```bash
docker compose -f compose.yaml -f compose.low-memory.yaml up -d
```

See `docs/low-memory-deployment.md` for what it changes, the measured effect and the trade-offs.

## Client libraries

Official SDKs live in their own repos under the [UseMailx](https://github.com/UseMailx) organization, not in this one — a self-hosted server checkout doesn't need client-side code in five languages sitting in it. Each SDK defaults to the hosted API's base URL but takes an override, so the same client code works against a self-hosted deployment.

| Language | Repo |
| --- | --- |
| Node.js / TypeScript | [UseMailx/mailx-node](https://github.com/UseMailx/mailx-node) |
| Python | [UseMailx/mailx-python](https://github.com/UseMailx/mailx-python) |
| Go | [UseMailx/mailx-go](https://github.com/UseMailx/mailx-go) |
| PHP | [UseMailx/mailx-php](https://github.com/UseMailx/mailx-php) |
| Ruby | [UseMailx/mailx-ruby](https://github.com/UseMailx/mailx-ruby) |

## API example

Every `/v1` endpoint requires a scoped MailX API key. Tenant and key management are deliberately operator-only CLI operations.

1. Create a tenant.

   ```bash
   docker compose exec mailx /app/mailx create-tenant -name local-development
   ```

2. Create a key using the `tenant_id` printed by the previous command.

   ```bash
   docker compose exec mailx /app/mailx create-api-key \
     -tenant TENANT-ID \
     -name local-client \
     -scopes emails:send,emails:read,domains:read,domains:write,webhooks:read,webhooks:write
   ```

   Save the printed `key` immediately. MailX displays the raw key only once.

3. Submit an email using that key.

   ```bash
   export MAILX_API_KEY='PASTE-THE-KEY-HERE'

   curl --request POST http://localhost:8080/v1/emails \
     --header "Authorization: Bearer ${MAILX_API_KEY}" \
     --header 'Content-Type: application/json' \
     --header 'Idempotency-Key: local-example-001' \
     --data '{
       "from": "sender@example.com",
       "to": ["recipient@example.net"],
       "subject": "Hello from MailX",
       "text": "MailX accepted this message for asynchronous delivery."
     }'
   ```

An HTTP `202 Accepted` response means MailX durably recorded responsibility for the message. It does not mean that a remote SMTP server accepted the message or that it reached an inbox. Use `GET /v1/emails/{id}` and `GET /v1/events` to inspect durable lifecycle state.

For the complete API, including domains, webhooks, pagination, scopes, and error schemas, open the [interactive documentation](http://localhost:8080/docs) after starting MailX.

## Architecture

<div align="center">
  <img src="./assets/readme/architecture.svg" alt="MailX architecture: SMTP and HTTP inputs flow through durable PostgreSQL state, local message storage, Redis scheduling, delivery workers, SMTP transfer, lifecycle events, and signed webhooks" width="100%">
</div>

MailX separates durable truth from scheduling:

- **PostgreSQL** owns tenants, API keys, email lifecycle state, delivery attempts, domains, idempotency records, immutable events, and webhook deliveries.
- **Local file storage** preserves raw messages and extracted attachment data.
- **Redis** coordinates available and leased jobs; it is not the authoritative delivery-history store.
- **Workers** persist delivery outcomes before acknowledging or releasing queue jobs.
- **Webhooks** observe immutable events and never change email delivery state.

This architecture provides at-least-once processing. MailX does not claim exactly-once SMTP delivery: a crash after remote acceptance but before local outcome persistence is inherently ambiguous.

## Configuration

The complete development configuration is documented in [`.env.example`](./.env.example). The main runtime settings are:

| Variable | Purpose | Local default |
| --- | --- | --- |
| `DATABASE_URL` | PostgreSQL connection string; enables the full API, queue, worker, and webhook runtime | Set by Compose |
| `REDIS_ADDR` | Redis queue address | `localhost:6379` outside Compose |
| `MAILX_HTTP_ADDR` | HTTP API listener | `:8080` |
| `MAILX_SMTP_ADDR` | SMTP listener | `:2525` |
| `MAILX_STORAGE_ROOT` | Raw message and attachment storage directory | MailX storage default |
| `MAILX_API_KEY_PEPPER` | Optional HMAC pepper for stored API-key verifiers | Development-only value in Compose |
| `MAILX_WEBHOOK_MASTER_KEY` | Base64-encoded 32-byte key used to protect webhook secrets | Development-only value in Compose |
| `MAILX_WEBHOOK_ALLOW_INSECURE` | Allows HTTP/private webhook targets for local development | `true` in Compose |
| `MAILX_WEBHOOK_WORKERS` | Number of webhook delivery workers | `4` |

Do not commit a real `.env` file. Use distinct random secrets and restricted network bindings outside local development.

### SMTP-only mode

When `DATABASE_URL` is unset, running MailX starts the original SMTP receiver and local file store without the REST API, PostgreSQL pipeline, Redis queue, or webhooks:

```bash
go run ./cmd/mailx
```

This mode is useful for parser and receiver development; it is not equivalent to the complete stack.

## Local message inspection

Messages accepted by the SMTP receiver can be listed and inspected from the local file store:

```bash
go run ./cmd/mailx list
go run ./cmd/mailx inspect MAILX-ID
```

Set `MAILX_STORAGE_ROOT` when the messages are not stored in the default location. The inspection command reports metadata and attachment information without rendering HTML or executing remote content.

## Development

Start the infrastructure dependencies, then run the full validation suite:

```bash
docker compose up -d postgres redis

export MAILX_TEST_DATABASE_URL='postgres://mailx:mailx-dev-only@127.0.0.1:5432/mailx?sslmode=disable'
export REDIS_ADDR='127.0.0.1:6379'

go fmt ./...
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
go build ./...
git diff --check
```

The test suite includes unit, integration, concurrency, protocol-boundary, persistence, tenant-isolation, retry, and security regression coverage. PostgreSQL and Redis integration tests require those services to be reachable.

Read [CONTRIBUTING.md](./CONTRIBUTING.md) before opening a change. Pull requests should be focused, tested, and use Conventional Commit messages.

## Project status

MailX is an educational and engineering project under active development. The current codebase implements milestones through durable webhook delivery; operational observability and transport security remain ongoing roadmap work.

The following are important current boundaries:

- No inbound or outbound STARTTLS/TLS yet.
- No SMTP AUTH yet.
- DNS ownership verification does not imply DKIM, SPF, DMARC, or inbox placement.
- API acceptance and SMTP acceptance have distinct meanings.
- Delivery and webhook processing are at-least-once, not exactly-once.
- The supplied Compose credentials and webhook settings are local-development defaults only.

## Security

MailX handles private message data and security-sensitive credentials. Do not include real API keys, webhook secrets, SMTP credentials, message bodies, or connection strings in issues or logs.

Report vulnerabilities privately according to [SECURITY.md](./SECURITY.md). Do not open a public issue containing exploit details.

## Community

- MailX is maintained by Feranmi Oresajo with contributions welcomed from the community.
- Read the [contribution guide](./CONTRIBUTING.md) before submitting code.
- Follow the [Code of Conduct](./CODE_OF_CONDUCT.md) in all project spaces.
- Use [GitHub Issues](https://github.com/Ferousco-dev/Mailx/issues) for reproducible bugs and scoped feature proposals.
- Use the generated [OpenAPI contract](http://localhost:8080/openapi.json) as the source of truth for the current HTTP surface.

## License

MailX is available under the [MIT License](./LICENSE).
