<p align="center">
  <img src="./assets/mailx.png" alt="MailX Logo" width="500">
</p>

<p align="center">
  A mail service built <strong>from scratch using Go</strong>.
</p>

---

## About MailX

**MailX** is a mail service I'm building from scratch using Go.

This project is part of my journey learning Go by building a real-world backend system instead of only following tutorials.

## Goal

The goal is to understand how modern email platforms work under the hood and gradually build a functional mail service from the ground up.

The project will explore concepts such as:

- Email sending and delivery
- SMTP
- Email queues
- Background workers
- REST APIs
- Authentication and API keys
- Email templates
- Rate limiting
- Logging and error handling
- Database design
- Concurrency with goroutines
- Retries and failed delivery handling

## Tech Stack

- **Language:** Go
- **Architecture:** Backend service
- **Protocol:** SMTP
- **API:** REST

More technologies will be added as the project evolves.

## Status

**Current milestone: v0.3 - Message Model and Parser — complete.**

The server tracks per-connection SMTP state, parses basic message headers, preserves raw DATA, keeps the SMTP envelope separate from message headers, and supports `EHLO`/`HELO`, `MAIL FROM`, `RCPT TO`, `DATA`, `RSET`, `NOOP`, and `QUIT`. Run it with:

```bash
go run ./cmd/mailx
```

Tests can be run with `GOCACHE=/tmp/mailx-gocache go test ./...` when the default Go cache is not writable.

I'm actively learning Go while building MailX, so the architecture and implementation will evolve as I learn more.

The long-term goal is to understand what it takes to build the core infrastructure behind modern email delivery platforms.

## Why I'm Building This

I want to learn a compiled backend language and understand backend infrastructure at a deeper level.

Instead of building another basic CRUD project, I decided to challenge myself:

> **Build a mail service from scratch with Go.**

This repository documents that journey.
