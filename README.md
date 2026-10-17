<div align="center">
  <img src="./assets/mailx.png" alt="MailX" width="100%">

  <p>
    <a href="https://github.com/Ferousco-dev/Mailx/actions/workflows/go-ci.yml"><img src="https://github.com/Ferousco-dev/Mailx/actions/workflows/go-ci.yml/badge.svg" alt="Go CI"></a>
    <a href="https://github.com/Ferousco-dev/Mailx/actions/workflows/security.yml"><img src="https://github.com/Ferousco-dev/Mailx/actions/workflows/security.yml/badge.svg" alt="Security checks"></a>
    <a href="./go.mod"><img src="https://img.shields.io/github/go-mod/go-version/Ferousco-dev/Mailx?style=flat-square&amp;color=00ADD8" alt="Go version"></a>
    <a href="./LICENSE"><img src="https://img.shields.io/github/license/Ferousco-dev/Mailx?style=flat-square" alt="MIT license"></a>
  </p>

  <p>
    <a href="#quick-start">Quick start</a> ·
    <a href="#send-your-first-email">Send an email</a> ·
    <a href="#architecture">Architecture</a> ·
    <a href="#broadcast-controls">Broadcasts</a> ·
    <a href="#development">Development</a>
  </p>
</div>

MailX is open-source email infrastructure written in Go. Submit mail through a tenant-scoped REST API or authenticated SMTP submission, send directly to recipient mail servers or through a trusted relay, and follow each message through signed webhooks.

PostgreSQL records delivery history. Redis schedules work, and local storage keeps the MIME message. The protocol, queue and delivery code live in this repository.

> [!WARNING]
> MailX is under active development and is not production-ready. The supplied stack is for local development. Keep the unauthenticated SMTP receiver and operator endpoints off untrusted networks.

## Capabilities

| Area | Included |
| --- | --- |
| Sending | Plain text and HTML, templates, scheduled emails, batch submission, direct MX delivery and authenticated relay delivery |
| Campaigns | Contacts, audiences, personalized broadcasts, scheduling, rescheduling before expansion and cancellation of further message handoffs |
| Delivery safety | Verified sender domains, DKIM signing, recipient suppression, tenant limits, bounded workers and durable retry history |
| Transport | Verified outbound STARTTLS and optional SMTP submission over implicit TLS with API-key authentication |
| Events | Immutable email events, signed webhooks, retry history and webhook secret rotation |
| Visibility | Delivery analytics, optional open/click tracking, structured logs, Prometheus metrics and health checks |
| Accounts | Human accounts, organizations, invitations, scoped API keys, optional OAuth and TOTP MFA, and optional Paystack billing |
| Operations | Sending pools, DNS readiness checks for SPF/DMARC/BIMI, retention controls and operator GDPR export/delete tools |

## Quick start

You need Docker Engine with Compose v2 and `curl`. Building outside Docker requires Go 1.27.1 or newer.

### Start the local stack

```bash
git clone https://github.com/Ferousco-dev/Mailx.git
cd Mailx
cp .env.example .env
docker compose up --build -d
docker compose ps
```

Compose starts PostgreSQL, Redis, a migration job and MailX. It publishes the following ports on loopback only:

| Service | Local address |
| --- | --- |
| REST API | `http://localhost:8080` |
| Interactive API docs | `http://localhost:8080/docs` |
| OpenAPI contract | `http://localhost:8080/openapi.json` |
| SMTP capture receiver | `localhost:2525` |
| Operator metrics and health | `http://localhost:9090` |
| PostgreSQL | `localhost:5432` |
| Redis | `localhost:6379` |

Check that MailX can reach its dependencies:

```bash
curl --fail http://localhost:8080/health/live
curl --fail http://localhost:8080/health/ready
```

Liveness reports whether the process is running. Readiness requires PostgreSQL and Redis; an unavailable dependency returns `503` with its name.

<details>
<summary>Logs, shutdown and smaller hosts</summary>

```bash
docker compose logs -f mailx
docker compose down
```

`docker compose down` preserves the named volumes. Adding `-v` deletes the stored messages, database and queue data.

For hosts with about 1 GB RAM, use the optional PostgreSQL profile:

```bash
docker compose -f compose.yaml -f compose.low-memory.yaml up -d
```

The profile keeps durability enabled. See the [deployment guide](./docs/low-memory-deployment.md) for its settings and trade-offs. A separate [DNS override](./compose.dns-workaround.yaml) is available for environments that need it.

</details>

## Send your first email

The sender's domain must belong to your tenant and pass DNS ownership verification. Create the tenant and API key first, then verify the domain before submitting mail.

### 1. Create a tenant and API key

```bash
docker compose exec mailx /app/mailx create-tenant -name local-development
```

Use the printed `tenant_id` in the next command:

```bash
docker compose exec mailx /app/mailx create-api-key \
  -tenant TENANT-ID \
  -name local-client \
  -scopes emails:send,emails:read,domains:read,domains:write,webhooks:read
```

Save the printed `key`. MailX displays the raw key once.

```bash
export MAILX_API_KEY='PASTE-THE-KEY-HERE'
export MAILX_DOMAIN='your-domain.example'
```

Replace `your-domain.example` with a public domain you own. The values above are placeholders.

### 2. Verify your sending domain

```bash
curl --fail-with-body --request POST http://localhost:8080/v1/domains \
  --header "Authorization: Bearer ${MAILX_API_KEY}" \
  --header 'Content-Type: application/json' \
  --data "{\"name\":\"${MAILX_DOMAIN}\"}"
```

Publish the TXT record returned in `records` at your DNS provider. Set `MAILX_DOMAIN_ID` to the response's `id`, then verify it after DNS has propagated:

```bash
export MAILX_DOMAIN_ID='DOMAIN-ID'

curl --fail-with-body --request POST \
  "http://localhost:8080/v1/domains/${MAILX_DOMAIN_ID}/verify" \
  --header "Authorization: Bearer ${MAILX_API_KEY}"
```

Continue once `ownership_state` is `verified`. Sending before verification returns `403 from_domain_not_authorized`. Ownership verification is separate from publishing DKIM, SPF and DMARC records.

### 3. Submit the message

Set `MAILX_RECIPIENT` to an address you control:

```bash
export MAILX_RECIPIENT='recipient@example.net'

curl --fail-with-body --request POST http://localhost:8080/v1/emails \
  --header "Authorization: Bearer ${MAILX_API_KEY}" \
  --header 'Content-Type: application/json' \
  --header 'Idempotency-Key: local-example-001' \
  --data "{\"from\":\"sender@${MAILX_DOMAIN}\",\"to\":[\"${MAILX_RECIPIENT}\"],\"subject\":\"Hello from MailX\",\"text\":\"This message was submitted through MailX.\"}"
```

`202 Accepted` means MailX committed the message and its delivery work to PostgreSQL. The worker sends it asynchronously. Use the returned email `id` to check progress:

```bash
curl --fail-with-body "http://localhost:8080/v1/emails/EMAIL-ID" \
  --header "Authorization: Bearer ${MAILX_API_KEY}"

curl --fail-with-body http://localhost:8080/v1/events \
  --header "Authorization: Bearer ${MAILX_API_KEY}"
```

`delivered` means the remote SMTP server accepted the message. Inbox placement depends on the receiver.

Each email's To, Cc and Bcc recipients must share one destination domain. Use separate messages or a broadcast to reach recipients across domains.

## Architecture

All outbound entry points share the same message, outbox and delivery pipeline. Broadcasts hand off one message per recipient in bounded batches.

```mermaid
flowchart TD
    API["Tenant REST API"] --> Accept["Validate sender and limits<br/>Build MIME and sign DKIM"]
    Submission["Authenticated SMTP submission<br/>Implicit TLS, optional"] --> Accept
    Campaign["Scheduled broadcast"] --> Expand["Snapshot and personalize<br/>Bounded expansion"]
    Expand --> Accept
    Accept --> Files[("Local message storage<br/>Raw MIME and attachments")]
    Accept --> DB[("PostgreSQL<br/>Messages, recipients, outbox and events")]
    DB --> Dispatch["Dispatch due outbox work"]
    Dispatch --> Redis[("Redis<br/>Available jobs and leased claims")]
    Redis --> Worker["Bounded delivery workers"]
    Worker --> Transport["MX lookup or trusted relay<br/>SMTP and STARTTLS policy"]
    Transport --> Remote["Recipient mail server"]
    Worker -->|"Persist outcome before ack or retry"| DB
    DB --> Fanout["Durable event fan-out"]
    Fanout --> Hooks["Webhook workers<br/>Sign, deliver and retry"]
    Hooks --> Target["Subscriber HTTPS endpoint"]
    Capture["Local SMTP capture receiver<br/>Storage only"] --> Files

    classDef store fill:#eef2f6,stroke:#64748b,color:#0f172a
    class Files,DB,Redis store
```

### State and recovery

| Component | Responsibility |
| --- | --- |
| PostgreSQL | Authoritative message state, attempts, tenant data, campaign snapshots and webhook history |
| Local files | Stored message bytes and extracted attachments |
| Redis | Job scheduling and temporary claim ownership |
| Workers | Delivery attempts; commit outcomes before acknowledging jobs or scheduling retries |

Acceptance writes the message, recipients, queued event and outbox row in one database transaction. The dispatcher retries the handoff to Redis. Workers load durable state when reclaiming a job, so they can resume after a crash.

Delivery is at least once. A crash after a remote server accepts mail but before MailX records that outcome can lead to a repeat attempt. SMTP has no transaction that spans both servers.

### SMTP modes

| Mode | Behavior |
| --- | --- |
| Capture receiver | The default `:2525` listener parses and stores mail locally. It creates no delivery jobs and requires no authentication. |
| Authenticated submission | Optional listener configured with `MAILX_SUBMISSION_ADDR` and certificate/key files. Uses implicit TLS and an API key with `emails:send` as the password, then enters the outbound pipeline. |
| Outbound delivery | Direct MX connections by default, or one configured trusted relay. STARTTLS verifies certificates; a TLS attempt that fails never falls back to plaintext. |

Without `DATABASE_URL`, MailX runs the capture receiver and operator listener without the API or delivery pipeline. To inspect locally captured messages:

```bash
go run ./cmd/mailx list
go run ./cmd/mailx inspect MAILX-ID
```

Set `MAILX_STORAGE_ROOT` to inspect a different store. Inspection reports metadata and attachments without rendering HTML.

## Broadcast controls

Create a broadcast with an audience, template and optional RFC 3339 `send_at`. Its template content is copied at acceptance. Audience members added afterward are excluded; members removed before their snapshot batch is scanned may be excluded too.

```mermaid
stateDiagram-v2
    [*] --> accepted: Create
    accepted --> accepted: Reschedule
    accepted --> expanding: Schedule is due
    accepted --> cancelled: Cancel
    expanding --> cancelled: Cancel
    expanding --> completed: Expansion finishes
    expanding --> failed: Broadcast-wide failure
    completed --> [*]
    failed --> [*]
    cancelled --> [*]
```

Both control endpoints require `broadcasts:write`. Replace `BROADCAST-ID` with your broadcast's `id`.

| Action | Endpoint | Allowed states |
| --- | --- | --- |
| Change the schedule | `POST /v1/broadcasts/{id}/reschedule` | `accepted` |
| Stop further handoffs | `POST /v1/broadcasts/{id}/cancel` | `accepted`, `expanding`; repeats on `cancelled` succeed |

```bash
curl --fail-with-body --request POST \
  http://localhost:8080/v1/broadcasts/BROADCAST-ID/reschedule \
  --header "Authorization: Bearer ${MAILX_API_KEY}" \
  --header 'Content-Type: application/json' \
  --data '{"send_at":"2027-01-15T09:00:00Z"}'

curl --fail-with-body --request POST \
  http://localhost:8080/v1/broadcasts/BROADCAST-ID/cancel \
  --header "Authorization: Bearer ${MAILX_API_KEY}"
```

Choose a future timestamp. Rescheduling preserves the original audience and template snapshots. Once expansion starts, rescheduling returns `409 broadcast_state_conflict`.

Cancellation waits for any database handoff already in progress, then prevents new snapshots and message inserts. Messages already queued continue through delivery and cannot be recalled. A completed or failed broadcast returns `409`; another tenant's broadcast returns `404`.

Recipient lists and broadcast analytics distinguish `cancelled` recipients from `materialized` recipients already handed off. Counts cover the stored snapshot, which can be partial when cancellation interrupts expansion. Audience members never snapshotted have no recipient row and are excluded from those counts. `completed` means expansion finished, regardless of the messages' delivery outcomes.

Apply migrations and upgrade all MailX processes before using these controls. Older expander binaries do not enforce the cancellation boundary. Rolling migration 000035 back preserves cancelled broadcasts as failed records so they stay inactive.

## Configuration

The [environment example](./.env.example) covers the core local stack. Optional features also read the settings below; Compose must explicitly pass any additional variables or mount certificate files you configure.

| Settings | Purpose |
| --- | --- |
| `DATABASE_URL`, `REDIS_ADDR` | Database and queue connections |
| `MAILX_HTTP_ADDR`, `MAILX_SMTP_ADDR`, `MAILX_STORAGE_ROOT` | API, capture listener and local storage |
| `MAILX_API_KEY_PEPPER` | API-key verifier protection |
| `MAILX_WEBHOOK_MASTER_KEY`, `MAILX_DKIM_MASTER_KEY` | Distinct base64-encoded 32-byte encryption keys for webhook secrets and DKIM keys |
| `MAILX_SMTP_TLS_POLICY`, `MAILX_SMTP_TLS_CA_FILE` | Outbound `opportunistic` or `required` TLS and optional additional trusted CA roots |
| `MAILX_RELAY_HOST`, `MAILX_RELAY_PORT`, `MAILX_RELAY_USERNAME`, `MAILX_RELAY_PASSWORD` | Optional trusted relay; authenticated relay delivery requires verified STARTTLS |
| `MAILX_SMTP_HOSTNAME`, `MAILX_SENDING_IPS`, `MAILX_SPF_RELAY_INCLUDE` | Public sending identity and DNS readiness guidance |
| `MAILX_LOG_LEVEL`, `MAILX_LOG_FORMAT`, `MAILX_OBSERVABILITY_ADDR` | Logging and the separate operator listener |
| `MAILX_SUBMISSION_ADDR`, `MAILX_SUBMISSION_TLS_CERT`, `MAILX_SUBMISSION_TLS_KEY` | Optional authenticated SMTP submission listener |
| `MAILX_JWT_SECRET`, `MAILX_SYSTEM_TENANT_ID`, `MAILX_SYSTEM_FROM_ADDRESS` | Persistent human sessions and system email delivery from a verified sender |

Use fresh secrets outside local development, set `MAILX_WEBHOOK_ALLOW_INSECURE=false`, and put the HTTP API behind HTTPS. Without a configured system mailer, account verification and password-reset emails cannot be sent. Optional OAuth, MFA, tracking and billing configuration lives in the corresponding [runtime modules](./cmd/mailx).

## Development

Start PostgreSQL and Redis before running integration tests. If you already have them running locally, use an isolated test database and set its connection string instead of starting duplicate services.

```bash
docker compose up -d postgres redis

export MAILX_TEST_DATABASE_URL='postgres://mailx:mailx-dev-only@127.0.0.1:5432/mailx?sslmode=disable'
export REDIS_ADDR='127.0.0.1:6379'
export GOMAXPROCS=2

go fmt ./...
go vet -p 1 ./...
go test -p 1 -parallel 2 -count=1 ./...
go test -race -p 1 -parallel 2 -count=1 ./...
go build -p 1 ./...
git diff --check
```

Tests use isolated PostgreSQL schemas and Redis namespaces. Set `MAILX_TEST_DATABASE_URL` explicitly: database tests can skip if PostgreSQL is unavailable, while Redis integration tests fail if Redis is unreachable.

### Repository map

| Path | Contains |
| --- | --- |
| [`cmd/mailx`](./cmd/mailx) | Runtime wiring, configuration and operator commands |
| [`internal/api`](./internal/api) | HTTP handlers, middleware and the OpenAPI contract |
| [`internal/database`](./internal/database) | PostgreSQL repositories and versioned migrations |
| [`internal/smtp`](./internal/smtp) | SMTP protocol server and outbound client |
| [`internal/broadcast`](./internal/broadcast) | Campaign snapshots and bounded message handoffs |
| [`internal/worker`](./internal/worker), [`internal/queue`](./internal/queue), [`internal/dispatch`](./internal/dispatch) | Delivery processing, claims and outbox dispatch |
| [`internal/webhook`](./internal/webhook) | Signing, fan-out and delivery retries |

## Project status

MailX implements the sending pipeline and account backend, with features through the v0.47 account and organization work. Some features require optional configuration. The repository remains a project under active development. Its [progress record](./.ilana/milestones.md) tracks the completed milestones and remaining scope.

DNS readiness checks describe sender configuration; they cannot guarantee inbox placement. General inbound mailbox hosting is outside the current scope. Generated bounce/DSN reports are recorded locally rather than automatically sent.

## Contributing

Read the [contribution guide](./CONTRIBUTING.md) and [Code of Conduct](./CODE_OF_CONDUCT.md). Keep changes focused, test the affected behavior and use Conventional Commit messages. For a large feature, discuss the scope with the maintainer before submitting it.

Report bugs and feature proposals through [GitHub Issues](https://github.com/Ferousco-dev/Mailx/issues). Report security vulnerabilities privately using [SECURITY.md](./SECURITY.md); keep credentials and private message content out of reports and logs.

MailX is maintained by Feranmi Oresajo and released under the [MIT License](./LICENSE).
