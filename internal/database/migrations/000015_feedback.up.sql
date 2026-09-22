-- v0.32 outbound feedback (asynchronous bounces/DSNs and complaints).
--
-- Design (docs/design-v0.32.md): a message's SMTP transport result
-- (delivery_attempts, messages.status) is never rewritten by feedback that
-- arrives later. Feedback lands at the RECIPIENT level as a separate summary
-- (feedback_status/feedback_at), so "accepted_by_remote_at=T1, later_feedback=
-- hard_bounce, feedback_received_at=T2" can be represented without touching the
-- existing SMTP-truth columns, and without collapsing a multi-recipient
-- message's overall status to one recipient's later outcome.
ALTER TABLE recipients
    ADD COLUMN feedback_status TEXT CHECK (feedback_status IN ('bounced', 'complained')),
    ADD COLUMN feedback_at TIMESTAMPTZ,
    ADD COLUMN feedback_enhanced_status TEXT;

-- events.event_type gains 'complained' (email.complained); 'bounced' already
-- existed (reserved since v0.18, unused until this milestone) and is reused for
-- feedback-driven bounce notifications rather than adding a second bounce type.
ALTER TABLE events DROP CONSTRAINT events_event_type_check;
ALTER TABLE events ADD CONSTRAINT events_event_type_check
    CHECK (event_type IN ('queued','delivery_attempted','delivered','deferred','bounced','failed','suppressed','complained'));

-- feedback: one durable row per DISTINCT feedback item received (history, not a
-- summary). tenant_id is denormalized from messages (matches events.tenant_id)
-- so tenant-scoped queries never need to join messages. Retention classification
-- (docs/design-v0.32.md / architecture.md): historical/diagnostic, intended to be
-- retention-bound like delivery_attempts; no cleanup job ships in this milestone,
-- but nothing here blocks adding one (bounded columns, indexed by created_at).
--
-- recipient_id is NOT NULL: feedback that cannot be matched to one of the
-- message's own recipient rows is refused before it is ever durable (see
-- database.ProcessFeedback) — there is nothing safe to record against an
-- unmatched recipient, and accepting it would let malformed/forged input create
-- unbounded orphan rows.
--
-- raw_sha256 is the idempotency key: the exact same feedback bytes redelivered
-- (feedback transport is at-least-once) hit the UNIQUE constraint and become a
-- no-op (ON CONFLICT DO NOTHING) rather than a duplicate row, duplicate
-- suppression write, or duplicate event. A later, genuinely different DSN about
-- the same recipient (e.g. delayed, then failed) is legitimate new history and
-- gets its own row; only the resulting recipient-level TRANSITION (guarded
-- separately, see ProcessFeedback) drives suppression/events, so repeated
-- semantically-identical feedback never re-fires either.
CREATE TABLE feedback (
    id                TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    message_id        TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    recipient_id      TEXT NOT NULL REFERENCES recipients(id) ON DELETE CASCADE,
    kind              TEXT NOT NULL CHECK (kind IN ('bounce_permanent','bounce_temporary','bounce_unknown','complaint')),
    source            TEXT NOT NULL CHECK (source IN ('dsn','complaint')),
    action            TEXT CHECK (action IN ('failed','delayed','delivered','relayed','expanded','unknown')),
    enhanced_status   TEXT,
    diagnostic        TEXT,
    remote_mta        TEXT,
    reporting_mta     TEXT,
    raw_sha256        TEXT NOT NULL,
    received_at       TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (message_id, recipient_id, raw_sha256)
);

-- Query: "every feedback row for this message" (ON DELETE CASCADE from messages,
-- and the transactional processor's own re-reads) — FK columns are not
-- automatically indexed (same reasoning as idx_outbox_pending_tenant's note).
CREATE INDEX idx_feedback_message ON feedback (message_id);
