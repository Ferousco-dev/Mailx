-- Associate each delivery outcome event with the retry-level delivery
-- attempt that produced it. One attempt has exactly one outcome event;
-- the partial unique index makes persistence retry-safe without restricting
-- queued or future non-attempt event types.
ALTER TABLE events
    ADD COLUMN delivery_attempt_number INTEGER
        CHECK (delivery_attempt_number IS NULL OR delivery_attempt_number > 0);

CREATE UNIQUE INDEX uq_events_message_delivery_attempt
    ON events (message_id, delivery_attempt_number)
    WHERE delivery_attempt_number IS NOT NULL;
