# Low-memory deployment profile (opt-in)

For small hosts (about 1 GB of RAM in total, for example an AWS Lightsail 1 GB plan) running MailX, PostgreSQL and Redis together.

    docker compose -f compose.yaml -f compose.low-memory.yaml up -d

It is **not** the default. Plain `docker compose up` keeps stock PostgreSQL settings, which local development and the test suite use.

## What it does
`compose.low-memory.yaml` overrides only the `postgres` command with smaller memory settings: `shared_buffers=32MB` (default 128MB), `max_connections=20` (100), `work_mem=2MB` (4MB), `maintenance_work_mem=16MB` (64MB), `wal_buffers=1MB`, `max_wal_size=256MB`, `effective_cache_size=128MB` (a planner hint), and fewer autovacuum/background/parallel workers.

Measured on `postgres:16` with the same small `pgbench` load (5 clients, 8 s, scale 5): **about 122 MiB with defaults, about 51 MiB with this profile**. A long-running dev database showed about 288 MiB because of accumulated data and cache; re-measure on your own host with `docker stats`. Redis (about 7 MiB) and MailX (about 8 MiB) are already tiny, so PostgreSQL is the only piece worth tuning.

## What it does not do: durability
MailX persists a delivery outcome before acknowledging a queue job, and accepts an email only after its rows are committed. So `fsync`, `synchronous_commit` and `full_page_writes` are **pinned on** in the profile. Never turn them off to save memory. At startup MailX reads the server's settings and logs a `db_settings_warning` (with a bounded `code`) if `fsync`, `full_page_writes` or `synchronous_commit` is off, or if its connection pool (10) does not fit the server's non-reserved connections. It never blocks startup.

## Why 20 connections is enough
MailX uses at most 10 pool connections, PostgreSQL reserves 3, and the migrate service and admin CLI need a few more. The full test suite passed repeatedly against a server configured with `max_connections=20`.

## Trade-offs and the rest of the setup
- A small cache means more disk reads; at low volume that costs milliseconds, not correctness.
- Add a swap file (for example 1 GB) as a safety net.
- The profile is a memory trade-off, not resilience: keep nightly `pg_dump` backups copied off the host (API keys, domains and encrypted DKIM keys live only in PostgreSQL).
