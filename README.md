<p align="center">
  <img src="./assets/mailx.png" alt="MailX Logo" width="500">
</p>

<p align="center">
  A mail service built <strong>from scratch using Go</strong>.
</p>

---

## About MailX

**MailX** is a mail service I'm building from scratch using Go.

The project started as a way for me to learn Go by building a real backend system instead of only following tutorials, but it is gradually becoming a deeper exploration of how email infrastructure actually works.

The idea is simple:

> Learn the protocol first, build the infrastructure myself, then gradually turn it into a real developer email platform.

MailX does not currently rely on services such as Resend, SendGrid, Mailgun, or Gmail to handle its core SMTP behavior.

## Goal

The long-term goal is to understand and build the core infrastructure behind modern email platforms.

MailX will gradually explore and implement things such as:

- SMTP servers and clients
- Email sending and delivery
- MIME parsing
- Attachments
- Local mail persistence
- DNS and MX resolution
- Delivery queues
- Background workers
- Retries and failed delivery handling
- Bounce handling
- REST APIs
- Authentication and API keys
- Webhooks
- Email templates
- Rate limiting
- Logging and observability
- Database design
- Concurrency with goroutines
- TLS and SMTP authentication
- DKIM, SPF, and DMARC
- Inbound email
- Suppression lists
- Contacts and broadcasts
- Developer SDKs and CLI tools

The project is intentionally being built incrementally so I can understand each layer before introducing the next one.

## Tech Stack

- **Language:** Go
- **Protocol:** SMTP
- **Architecture:** Backend mail infrastructure
- **Storage:** Local filesystem for the current milestone
- **API:** REST API planned
- **Concurrency:** Goroutines

More infrastructure will be introduced only when the project reaches the stage where it is actually needed.

## Current Status

**Current milestone: v0.5 — Local Persistence**

### Completed

#### v0.1 — Basic SMTP Server

MailX can:

- Listen for TCP connections
- Handle multiple connections concurrently
- Send SMTP greetings
- Process basic SMTP commands
- Receive message DATA

#### v0.2 — SMTP Session / State Machine

MailX now understands SMTP command ordering and supports:

- `EHLO`
- `HELO`
- `MAIL FROM`
- `RCPT TO`
- `DATA`
- `RSET`
- `NOOP`
- `QUIT`

Each connection maintains its own SMTP session state.

#### v0.3 — Message Model and Parser

MailX separates the SMTP envelope from the Internet message itself and can parse:

- `From`
- `To`
- `Cc`
- `Bcc`
- `Subject`
- `Date`
- `Message-ID`
- Message body
- Folded headers

The original SMTP DATA is also preserved as raw message content.

#### v0.4 — MIME Basics

MailX now understands common MIME email structures, including:

- `text/plain`
- `text/html`
- `multipart/alternative`
- `multipart/mixed`
- Nested multipart messages
- Base64 decoding
- Quoted-printable decoding
- Attachments
- MIME metadata
- Content-Disposition
- Content-Transfer-Encoding

Attachments are extracted as binary-safe byte data while the original message remains unchanged.

#### v0.5 — Local Persistence

MailX can now persist received email to disk.

Each accepted message receives its own MailX-generated internal ID and is stored as:

```text
data/messages/<mailx-id>/
├── message.eml
├── metadata.json
└── attachments/
    ├── 0001.bin
    ├── 0002.bin
    └── ...