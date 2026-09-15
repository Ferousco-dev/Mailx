-- v0.15 initial relational schema.
--
-- Ownership chain: tenants -> messages -> recipients / delivery_attempts / events.
-- Every future-facing table gets tenant_id from day one specifically to avoid a
-- painful backfill migration once multi-tenancy actually matters — see the v0.15
-- report's "Tenant model" section for the full rationale.
--
-- IDs are TEXT, application-generated (crypto/rand, hex-encoded), matching
-- MailX's existing ID style (internal/storage.NewID, internal/queue.NewJobID).
-- Sequential SERIAL/BIGSERIAL ids are deliberately NOT used: MailX IDs are
-- externally exposed (message IDs already appear in mailx inspect/list output
-- and will appear in the future REST API), and a guessable sequential ID would
-- leak volume/ordering information and make ID enumeration trivial.

CREATE TABLE tenants (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- messages is the durable record of one message MailX accepted for delivery.
-- Raw MIME content and attachment bytes are deliberately NOT stored here —
-- id is shared with internal/storage.FileStore's message directory name, so
-- the raw .eml/attachments on disk (or, later, in object storage) are found
-- by this same id without any extra mapping table. See "Raw MIME storage
-- decision" in the v0.15 report.
CREATE TABLE messages (
    id                 TEXT PRIMARY KEY,
    tenant_id          TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    mail_from          TEXT NOT NULL,
    from_header        TEXT NOT NULL DEFAULT '',
    subject            TEXT NOT NULL DEFAULT '',
    message_id_header  TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'queued'
                       CHECK (status IN ('queued','processing','delivered','failed','bounced')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    queued_at          TIMESTAMPTZ,
    delivered_at       TIMESTAMPTZ
);

-- Query: "list newest messages for tenant" / "list messages by tenant + status,
-- newest first" — the future GET /emails list endpoint's primary access path.
-- Column order matters: tenant_id first (equality filter), status second
-- (equality filter, present in the common filtered-list case), created_at DESC
-- last (range/order). A query that only filters tenant_id (no status) can
-- still use this index efficiently via index skip on the leading column.
CREATE INDEX idx_messages_tenant_status_created
    ON messages (tenant_id, status, created_at DESC);

-- Query: "fetch message by id, scoped to tenant" — the future GET
-- /emails/{id} endpoint. The PRIMARY KEY on id alone already makes lookup by
-- id O(1); this index exists so a query that ALSO filters tenant_id (to
-- enforce tenant ownership at the query level, not just in application code)
-- does not need a second lookup/heap fetch after the PK lookup. Deliberately
-- NOT created if id-only lookup is always trusted to already be
-- tenant-correct — it is included here because tenant-scoped lookup is the
-- safe-by-construction pattern the codebase should use once an API exists.
CREATE UNIQUE INDEX idx_messages_tenant_id ON messages (tenant_id, id);

-- recipients represents SMTP ENVELOPE recipients (RCPT TO), which is what
-- delivery/retry/bounce actually track outcomes for — NOT the cosmetic
-- header To/Cc/Bcc lists, which MailX has repeatedly proven (v0.7-v0.14
-- integration tests) can legitimately differ from the envelope (a hidden
-- Bcc recipient may exist in the envelope with no corresponding header).
-- header_kind is a best-effort classification: if this address is also
-- found in the parsed To/Cc/Bcc headers, it is recorded; a hidden
-- envelope-only recipient (real Bcc) has header_kind = NULL.
CREATE TABLE recipients (
    id              TEXT PRIMARY KEY,
    message_id      TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    address         TEXT NOT NULL,
    header_kind     TEXT CHECK (header_kind IN ('to','cc','bcc')),
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','delivered','failed')),
    smtp_code       INTEGER,
    enhanced_status TEXT,
    diagnostic      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A message should not logically RCPT TO the same address twice; this
    -- also makes recipient upsert-on-conflict possible for idempotent writes.
    UNIQUE (message_id, address)
);

-- Query: "list recipients for message" (always scoped by message_id, the FK
-- itself) — Postgres does NOT automatically index foreign key columns (only
-- the referenced side gets an index from the PK), so without this index a
-- lookup by message_id, and every ON DELETE CASCADE from messages, would be
-- a sequential scan of recipients.
CREATE INDEX idx_recipients_message_id ON recipients (message_id);

-- delivery_attempts is one row per retry-level delivery OPERATION (matches
-- retry.DeliveryAttempt / retry.Coordinator.Attempt — one row per Attempt
-- call, i.e. one row per "operation #1", "operation #2", etc., not one row
-- per MX/SMTP-level sub-attempt within that operation).
--
-- mx_attempts (JSONB) preserves the finer MX/SMTP-level truth
-- (delivery.Result.Attempts: which MX host, which preference, was DATA
-- accepted, temporary/permanent, SMTP code, enhanced status, remote text,
-- recipient if RCPT-specific) as an ordered array of objects. This is
-- deliberately JSONB rather than a fully normalized child table: the list is
-- short (bounded by dns.MaxCandidates), read as a whole (never filtered
-- server-side by MX host in any query MailX needs today), and genuinely
-- semi-structured (its shape already varies by outcome kind). Promoting it
-- to a relational table would add a fourth table, a fourth FK, and indexes
-- serving no query MailX currently has, for information that is only ever
-- consumed as "the full attempt list for this operation."
CREATE TABLE delivery_attempts (
    id              TEXT PRIMARY KEY,
    message_id      TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    attempt_number  INTEGER NOT NULL,
    decision        TEXT NOT NULL
                    CHECK (decision IN ('retry','terminal_success','terminal_failure')),
    kind            TEXT NOT NULL,
    accepted        BOOLEAN NOT NULL,
    final_code      INTEGER,
    enhanced_status TEXT,
    remote_message  TEXT,
    failure_stage   TEXT,
    recipient       TEXT,
    quit_error      TEXT,
    mx_attempts     JSONB NOT NULL DEFAULT '[]'::jsonb,
    started_at      TIMESTAMPTZ NOT NULL,
    finished_at     TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (message_id, attempt_number)
);

-- Query: "list attempts for message, in operation order" — the future GET
-- /emails/{id} detail endpoint's delivery-history section.
CREATE INDEX idx_delivery_attempts_message_id
    ON delivery_attempts (message_id, attempt_number);

-- events is an append-only lifecycle log (queued, delivery_attempted,
-- delivered, deferred, bounced, failed). tenant_id is denormalized here
-- (also derivable via messages.tenant_id) specifically so a future
-- "list events for tenant across all messages, chronologically" query
-- (GET /events) never needs to join messages at all — events is written
-- far more than messages is updated, so keeping its hot read path
-- join-free is worth the small write-time denormalization.
CREATE TABLE events (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    message_id  TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    event_type  TEXT NOT NULL
                CHECK (event_type IN ('queued','delivery_attempted','delivered','deferred','bounced','failed')),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata    JSONB NOT NULL DEFAULT '{}'::jsonb
);

-- Query: "list events for tenant, newest first" (GET /events) and, via the
-- same column order, "list events for tenant since cursor" (keyset
-- pagination on (occurred_at, id)). id is included as the pagination
-- tiebreaker for rows sharing a timestamp.
CREATE INDEX idx_events_tenant_occurred
    ON events (tenant_id, occurred_at DESC, id DESC);

-- Query: "list events for one message, chronologically" (GET
-- /emails/{id} timeline).
CREATE INDEX idx_events_message_occurred
    ON events (message_id, occurred_at);
