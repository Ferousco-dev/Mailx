-- v0.18 transactional outbox: closes the "PostgreSQL COMMIT then crash
-- before Redis Enqueue" gap identified in v0.15/v0.16/v0.17. One row per
-- message, inserted in the SAME transaction as the message/recipients/
-- event insert (internal/database.InsertMessage). HTTP 202 therefore means
-- "durably recorded and MUST eventually be dispatched", independent of
-- whether Redis was reachable at accept time.
--
-- message_id is the primary key (not a separate generated id): there is
-- exactly one outbox row per message, and message_id doubles as the
-- stable queue.Job.ID/MessageID, so redispatch after a crash reuses
-- v0.16's existing duplicate-active-ID protection instead of a new one.
CREATE TABLE outbox (
    message_id    TEXT PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    tenant_id     TEXT NOT NULL,
    available_at  TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    dispatched_at TIMESTAMPTZ
);

-- Query: "find pending outbox rows due for dispatch, oldest first" — the
-- dispatcher's only query. Partial index (dispatched_at IS NULL) keeps it
-- small forever: dispatched rows (the overwhelming majority over time)
-- never appear in it.
CREATE INDEX idx_outbox_pending ON outbox (available_at) WHERE dispatched_at IS NULL;
