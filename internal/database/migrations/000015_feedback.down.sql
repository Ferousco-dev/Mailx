DROP TABLE IF EXISTS feedback;

ALTER TABLE events DROP CONSTRAINT events_event_type_check;
ALTER TABLE events ADD CONSTRAINT events_event_type_check
    CHECK (event_type IN ('queued','delivery_attempted','delivered','deferred','bounced','failed','suppressed'));

ALTER TABLE recipients
    DROP COLUMN IF EXISTS feedback_status,
    DROP COLUMN IF EXISTS feedback_at,
    DROP COLUMN IF EXISTS feedback_enhanced_status;
